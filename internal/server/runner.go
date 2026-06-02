package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	recent   *RecentStore
	gh       *gh.Client

	mu       sync.Mutex
	inflight map[string]bool // repos currently being scraped (per-repo lock)
	last     *RunReport
	seq      int
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
func NewRunner(cfg Config, q *repoqueue.Queue, p *pusher.Client, settings *SettingsStore, stats *StatsStore, repos *RepoStore, recent *RecentStore) *Runner {
	return &Runner{
		cfg:      cfg,
		queue:    q,
		push:     p,
		settings: settings,
		stats:    stats,
		repos:    repos,
		recent:   recent,
		gh:       gh.NewClient(cfg.Tokens, 100*time.Millisecond),
		inflight: map[string]bool{},
	}
}

// claim marks a repo in-flight; returns false if it's already being scraped, so
// the same repo never runs twice at once (different repos run concurrently).
func (r *Runner) claim(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[name] {
		return false
	}
	r.inflight[name] = true
	r.seq++
	return true
}

func (r *Runner) release(name string) {
	r.mu.Lock()
	delete(r.inflight, name)
	r.mu.Unlock()
}

// InFlight returns the set of repos currently being scraped.
func (r *Runner) InFlight() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]bool, len(r.inflight))
	for k := range r.inflight {
		m[k] = true
	}
	return m
}

// ScrapeRepo scrapes+pushes ONE repo's batch. Safe to call concurrently for
// DIFFERENT repos (they share the token pool, which round-robins + throttles
// per token); the same repo is skipped if already in flight (ok=false). Each
// finished batch is folded into stats + the per-repo store, and the delay is
// auto-tuned from the shared rate-limit budget.
func (r *Runner) ScrapeRepo(trigger string, t scraper.RepoTarget) (RepoReport, bool) {
	name := t.Owner + "/" + t.Repo
	if !r.claim(name) {
		return RepoReport{Repo: name}, false
	}
	defer r.release(name)

	set := r.settings.Get()
	started := time.Now()
	r.mu.Lock()
	seq := r.seq
	r.mu.Unlock()
	// Unique per-repo run dir so concurrent repos never collide on CSV paths.
	safe := strings.NewReplacer("/", "-").Replace(name)
	runDir := filepath.Join(r.cfg.OutputDir, fmt.Sprintf("run-%s-%d-%d", safe, started.Unix(), seq))
	defer os.RemoveAll(runDir)

	rep := r.scrapeAndPush(runDir, t, set)
	report := &RunReport{StartedAt: started, FinishedAt: time.Now(), Trigger: trigger, Repos: []RepoReport{rep}}
	r.store(report)
	r.stats.Record(report)
	return rep, true
}

// Run scrapes a set of repos (manual trigger / override). With an empty override
// it pops the next ReposPerRun from the queue. Each repo goes through ScrapeRepo
// (per-repo locked, individually recorded); this aggregates them for the caller.
func (r *Runner) Run(trigger string, override []scraper.RepoTarget) (*RunReport, error) {
	set := r.settings.Get()
	report := &RunReport{StartedAt: time.Now(), Trigger: trigger}

	targets := override
	if len(targets) == 0 {
		next, err := r.queue.Next(set.ReposPerRun)
		if err != nil {
			report.Error = err.Error()
			report.FinishedAt = time.Now()
			return report, err
		}
		targets = next
	}
	for _, t := range targets {
		if rep, ok := r.ScrapeRepo(trigger, t); ok {
			report.Repos = append(report.Repos, rep)
		}
	}
	report.FinishedAt = time.Now()
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
	// Resume deeper into the repo each run. Prefer the GraphQL cursor; the
	// integer offset is only a one-time fast-forward fallback for repos last
	// walked under the old REST page model (cursor still empty).
	offset := r.repos.Offset(name)
	cursor := r.repos.Cursor(name)

	cfg := scraper.Config{
		Repos:        []scraper.RepoTarget{t},
		Tokens:       r.cfg.Tokens,
		OutputDir:    runDir,
		Concurrency:  set.Concurrency,
		MaxRepos:     set.MaxRepos,
		MaxForkRepos: set.MaxForkRepos,
		MaxStars:     set.MaxStars,
		StartOffset:  offset,
		StartCursor:  cursor,
		Delay:        time.Duration(set.DelayMs) * time.Millisecond,
		UseSearchAPI: r.cfg.UseSearchAPI,
		CachePath:    r.cfg.CachePath,
	}

	// Drain progress concurrently; scraper.Run sends Done (with Count) for the
	// final repo and never closes the channel, so we close it ourselves.
	progressCh := make(chan scraper.Progress, 256)
	done := make(chan struct{})
	var scrapeErr error
	var count, failed int
	var finalCursor string
	var finalExhausted bool
	go func() {
		defer close(done)
		for p := range progressCh {
			if p.Error != nil {
				scrapeErr = p.Error
			}
			if p.Done {
				count = p.Count
				finalCursor = p.Cursor
				finalExhausted = p.Exhausted
			}
			// Stream each processed stargazer into the live Contacts view.
			if p.Result != nil && r.recent != nil {
				ur := p.Result
				status := "scraped"
				switch {
				case ur.FetchFailed:
					status = "failed"
					failed++
				case ur.Email == "":
					status = "none"
				case ur.EmailSource == "noreply":
					status = "noreply"
				}
				r.recent.Add(ContactRow{
					Login: ur.Login, Email: ur.Email, EmailSource: ur.EmailSource,
					Repo: name, Status: status, At: time.Now(),
				})
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

	r.repos.RecordRun(name, count, stats.Imported, rep.Error, time.Now(), finalCursor, finalExhausted)
	rateLimited := scrapeErr != nil && strings.Contains(strings.ToLower(scrapeErr.Error()), "rate limit")
	r.autoTune(count, failed, rateLimited)

	log.Printf("repo %s: offset=%d scraped=%d failed=%d sent=%d imported=%d skipped=%d suppressed=%d noreply=%d invalid=%d%s",
		name, offset, count, failed, stats.Sent, stats.Imported, stats.Skipped, stats.Suppressed, stats.Noreply, stats.Invalid,
		errSuffix(rep.Error))
	return rep
}

// autoTune sets the per-call throttle. Key insight: when the rate-limit budget
// is ABUNDANT, we should run at the FLOOR (let concurrency + pipeline be the
// governor) — NOT spread the remaining budget evenly over the whole reset window
// (that rations a budget we're nowhere near using and just adds latency to the
// sequential critical path, which is what slowed us down). Only when a pool runs
// LOW (< lowFrac of its limit) do we start rationing what's left over the time
// until reset, so we glide to the reset instead of slamming into a hard 403.
// Considers both REST core and GraphQL; GraphQL now carries ~all the load.
func (r *Runner) autoTune(_ int, _ int, _ bool) {
	if !r.settings.Get().AutoTune {
		return
	}
	statuses := r.gh.ProbeAllTokens()
	if len(statuses) == 0 {
		return
	}
	const floorMs = 25.0
	const lowFrac = 0.30 // only ration a pool once it's below 30% remaining
	worst := floorMs

	var core, coreLimit, gql, gqlLimit int
	var coreReset, gqlReset time.Time
	for _, s := range statuses {
		core += s.Core.Remaining
		coreLimit += s.Core.Limit
		gql += s.GraphQL.Remaining
		gqlLimit += s.GraphQL.Limit
		if coreReset.IsZero() || s.Core.Reset.Before(coreReset) {
			coreReset = s.Core.Reset
		}
		if gqlReset.IsZero() || s.GraphQL.Reset.Before(gqlReset) {
			gqlReset = s.GraphQL.Reset
		}
	}
	ration := func(remaining, limit int, reset time.Time) {
		if limit <= 0 || float64(remaining)/float64(limit) >= lowFrac {
			return // abundant — don't slow down
		}
		horizon := time.Until(reset).Seconds()
		if horizon < 60 {
			horizon = 60
		}
		if remaining < 1 {
			remaining = 1
		}
		if d := float64(len(statuses)) * horizon / float64(remaining) * 1000.0; d > worst {
			worst = d
		}
	}
	ration(core, coreLimit, coreReset)
	ration(gql, gqlLimit, gqlReset)
	next := int(worst)
	if next > 5000 {
		next = 5000
	}
	cur := r.settings.Get().DelayMs
	if next != cur {
		r.settings.Update(SettingsPatch{DelayMs: &next})
		log.Printf("auto-tune: core %d / graphql %d remaining → delay %dms→%dms", core, gql, cur, next)
	}
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

// RateLimits probes every token's current rate-limit state (REST core + GraphQL
// + search). The /rate_limit endpoint is free (doesn't consume budget).
func (r *Runner) RateLimits() []gh.RateLimitStatus {
	return r.gh.ProbeAllTokens()
}

// Running reports whether any repo is currently being scraped.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inflight) > 0
}
