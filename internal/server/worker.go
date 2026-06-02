package server

import (
	"context"
	"log"
	"strings"
	"sync"
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
		n := settings.Get().MaxConcurrentRepos
		if n < 1 {
			n = 1
		}
		picks := pickRepos(targets, repos, runner.InFlight(), n)
		if len(picks) == 0 {
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

		// Scrape the picked repos concurrently — they share the token pool, which
		// round-robins + throttles per token, so several repos in flight fill the
		// budget a single sequential repo can't. Each repo is independently locked,
		// recorded, and auto-tuned.
		var wg sync.WaitGroup
		var rlMu sync.Mutex
		rateLimited := false
		for _, t := range picks {
			wg.Add(1)
			go func(t scraper.RepoTarget) {
				defer wg.Done()
				rep, ok := runner.ScrapeRepo("auto", t)
				if ok && strings.Contains(strings.ToLower(rep.Error), "rate limit") {
					rlMu.Lock()
					rateLimited = true
					rlMu.Unlock()
				}
			}(t)
		}
		wg.Wait()

		// Cool down hard when GitHub throttled us; otherwise a gentle gap.
		gap := 3 * time.Second
		if rateLimited {
			gap = 2 * time.Minute
			log.Printf("worker: rate-limited — cooling down %s before the next pass", gap)
		}
		if !sleep(ctx, gap) {
			return
		}
	}
}

// pickRepos returns up to n repos in queue order that still have stargazers left
// to walk and aren't already being scraped. Completion is decided by the Done
// flag (set when the GraphQL stream reaches the last page), NOT the
// processed-vs-total estimate (GitHub's total can differ from what the walk
// yields, which would otherwise wedge a repo or stop it early). Repos earlier in
// the queue are still preferred, but several walk at once to fill the budget.
func pickRepos(targets []scraper.RepoTarget, repos *RepoStore, inflight map[string]bool, n int) []scraper.RepoTarget {
	var out []scraper.RepoTarget
	for _, t := range targets {
		name := t.Owner + "/" + t.Repo
		if repos.IsDone(name) || inflight[name] {
			continue
		}
		out = append(out, t)
		if len(out) >= n {
			break
		}
	}
	return out
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
