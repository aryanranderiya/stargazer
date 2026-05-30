package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	gh "stargazer/internal/github"
	"stargazer/internal/pusher"
	"stargazer/internal/repoqueue"
	"stargazer/internal/scraper"
)

// Runner executes scrape+push runs, serialised so only one runs at a time. The
// tunable parameters come from the SettingsStore (live-editable); finished runs
// are folded into the StatsStore and per-repo RepoStore.
type Runner struct {
	cfg      Config
	queue    *repoqueue.Queue
	push     *pusher.Client
	settings *SettingsStore
	stats    *StatsStore
	repos    *RepoStore
	gh       *gh.Client

	mu      sync.Mutex
	running bool
	last    *RunReport
	seq     int
}

// RepoReport is the per-repo outcome of a run.
type RepoReport struct {
	Repo  string       `json:"repo"`
	Count int          `json:"scraped"`
	Push  pusher.Stats `json:"push"`
	Error string       `json:"error,omitempty"`
}

// RunReport summarises a single run.
type RunReport struct {
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt time.Time    `json:"finishedAt"`
	Trigger    string       `json:"trigger"` // "schedule" | "manual" | "startup"
	Repos      []RepoReport `json:"repos"`
	Error      string       `json:"error,omitempty"`
}

// NewRunner constructs a Runner.
func NewRunner(cfg Config, q *repoqueue.Queue, p *pusher.Client, settings *SettingsStore, stats *StatsStore, repos *RepoStore) *Runner {
	return &Runner{
		cfg:      cfg,
		queue:    q,
		push:     p,
		settings: settings,
		stats:    stats,
		repos:    repos,
		gh:       gh.NewClient(cfg.Tokens, 100*time.Millisecond),
	}
}

// Run scrapes and pushes a set of repos. With an empty override it pops the
// next ReposPerRun repos from the queue (advancing the cursor); with an
// override it scrapes exactly those repos without touching the cursor.
func (r *Runner) Run(trigger string, override []scraper.RepoTarget) (*RunReport, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil, fmt.Errorf("a scrape run is already in progress")
	}
	r.running = true
	r.seq++
	seq := r.seq
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	set := r.settings.Get()
	report := &RunReport{StartedAt: time.Now(), Trigger: trigger}

	targets := override
	if len(targets) == 0 {
		next, err := r.queue.Next(set.ReposPerRun)
		if err != nil {
			report.Error = err.Error()
			report.FinishedAt = time.Now()
			r.store(report)
			r.stats.Record(report)
			return report, err
		}
		targets = next
	}

	// Scrape each run into a unique dir so the scraper never appends "_1" to a
	// pre-existing CSV (which would make the output path nondeterministic).
	runDir := filepath.Join(r.cfg.OutputDir, fmt.Sprintf("run-%d-%d", report.StartedAt.Unix(), seq))
	defer os.RemoveAll(runDir) // CSVs already pushed; keep the volume tidy

	for _, t := range targets {
		report.Repos = append(report.Repos, r.scrapeAndPush(runDir, t, set))
	}

	report.FinishedAt = time.Now()
	r.store(report)
	r.stats.Record(report)
	log.Printf("run done (trigger=%s, repos=%d, took=%s)", trigger, len(report.Repos), report.FinishedAt.Sub(report.StartedAt).Truncate(time.Second))
	return report, nil
}

func (r *Runner) scrapeAndPush(runDir string, t scraper.RepoTarget, set Settings) RepoReport {
	name := t.Owner + "/" + t.Repo
	rep := RepoReport{Repo: name}

	// Ensure we know the repo's total stars (for the dashboard progress bar).
	if r.repos.NeedsTotal(name) {
		if total, err := r.gh.GetRepoStarCount(t.Owner, t.Repo); err == nil {
			r.repos.SetTotal(name, total)
		}
	}
	// Resume deeper into the repo each run instead of re-scraping the top.
	offset := r.repos.Offset(name)

	cfg := scraper.Config{
		Repos:        []scraper.RepoTarget{t},
		Tokens:       r.cfg.Tokens,
		OutputDir:    runDir,
		Concurrency:  set.Concurrency,
		MaxRepos:     set.MaxRepos,
		MaxForkRepos: set.MaxForkRepos,
		MaxStars:     set.MaxStars,
		StartOffset:  offset,
		Delay:        time.Duration(set.DelayMs) * time.Millisecond,
		UseSearchAPI: r.cfg.UseSearchAPI,
		CachePath:    r.cfg.CachePath,
	}

	// Drain progress concurrently; scraper.Run sends Done (with Count) for the
	// final repo and never closes the channel, so we close it ourselves.
	progressCh := make(chan scraper.Progress, 256)
	done := make(chan struct{})
	var scrapeErr error
	var count int
	go func() {
		defer close(done)
		for p := range progressCh {
			if p.Error != nil {
				scrapeErr = p.Error
			}
			if p.Done {
				count = p.Count
			}
		}
	}()
	scraper.Run(cfg, progressCh)
	close(progressCh)
	<-done

	if scrapeErr != nil {
		rep.Error = scrapeErr.Error()
	}
	rep.Count = count

	csvPath := filepath.Join(runDir, t.Owner, t.Repo+".csv")
	stats, perr := r.push.PushCSV(csvPath, name)
	rep.Push = stats
	if perr != nil {
		if rep.Error != "" {
			rep.Error += "; "
		}
		rep.Error += "push: " + perr.Error()
	}

	r.repos.RecordRun(name, count, stats.Imported, rep.Error, time.Now())

	log.Printf("repo %s: offset=%d scraped=%d sent=%d imported=%d skipped=%d suppressed=%d noreply=%d invalid=%d%s",
		name, offset, count, stats.Sent, stats.Imported, stats.Skipped, stats.Suppressed, stats.Noreply, stats.Invalid,
		errSuffix(rep.Error))
	return rep
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " err=" + e
}

func (r *Runner) store(report *RunReport) {
	r.mu.Lock()
	r.last = report
	r.mu.Unlock()
}

// Last returns the most recent run report (nil if none yet).
func (r *Runner) Last() *RunReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// RefreshTotals fetches and caches the total star count for any of the given
// repos whose total is still unknown — used to populate the dashboard progress
// bars immediately when repos are added to the queue.
func (r *Runner) RefreshTotals(targets []scraper.RepoTarget) {
	for _, t := range targets {
		name := t.Owner + "/" + t.Repo
		if r.repos.NeedsTotal(name) {
			if total, err := r.gh.GetRepoStarCount(t.Owner, t.Repo); err == nil {
				r.repos.SetTotal(name, total)
			}
		}
	}
}

// Running reports whether a run is currently in progress.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}
