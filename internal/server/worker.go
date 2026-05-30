package server

import (
	"context"
	"log"
	"strings"
	"time"

	"stargazer/internal/repoqueue"
	"stargazer/internal/scraper"
)

// StartWorker launches the continuous queue worker: as long as it isn't paused,
// it repeatedly scrapes the next repo that still has stargazers left to walk —
// one batch at a time, deep-walking each repo until done, then moving on. No
// schedule; adding repos to the queue just makes them get picked up. It idles
// (cheaply) when the queue is empty or fully walked, and resumes automatically
// when there's new work or it's un-paused.
func StartWorker(ctx context.Context, runner *Runner, settings *SettingsStore, queue *repoqueue.Queue, repos *RepoStore) {
	go workerLoop(ctx, runner, settings, queue, repos)
}

func workerLoop(ctx context.Context, runner *Runner, settings *SettingsStore, queue *repoqueue.Queue, repos *RepoStore) {
	announcedIdle := false
	log.Printf("worker: started (continuous queue mode)")
	for {
		if ctx.Err() != nil {
			return
		}
		if settings.Get().Paused {
			announcedIdle = false
			if !sleep(ctx, 3*time.Second) {
				return
			}
			continue
		}
		targets := queue.AllTargets()
		if len(targets) == 0 {
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}
		repo, ok := nextWithWork(targets, repos)
		if !ok {
			if !announcedIdle {
				log.Printf("worker: queue fully walked — idling until new repos are added")
				announcedIdle = true
			}
			if !sleep(ctx, 20*time.Second) {
				return
			}
			continue
		}
		announcedIdle = false
		report, err := runner.Run("auto", []scraper.RepoTarget{repo})
		if err != nil {
			log.Printf("worker: run error for %s/%s: %v", repo.Owner, repo.Repo, err)
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		// Cool down hard when GitHub throttled us; otherwise a gentle gap.
		gap := 3 * time.Second
		if rateLimitedReport(report) {
			gap = 2 * time.Minute
			log.Printf("worker: rate-limited — cooling down %s before the next pass", gap)
		}
		if !sleep(ctx, gap) {
			return
		}
	}
}

func rateLimitedReport(rep *RunReport) bool {
	if rep == nil {
		return false
	}
	for _, r := range rep.Repos {
		if strings.Contains(strings.ToLower(r.Error), "rate limit") {
			return true
		}
	}
	return false
}

// nextWithWork returns the FIRST repo in queue order that still has stargazers
// left to walk — so a repo is fully exhausted before the next one is touched
// (strictly sequential, one repo at a time). A repo with an unknown total (-1)
// is considered to have work (its total is fetched on the first scrape).
func nextWithWork(targets []scraper.RepoTarget, repos *RepoStore) (scraper.RepoTarget, bool) {
	for _, t := range targets {
		processed, total := repos.Progress(t.Owner + "/" + t.Repo)
		if total < 0 || processed < total {
			return t, true
		}
	}
	return scraper.RepoTarget{}, false
}

// sleep waits d or returns false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
