package server

import (
	"context"
	"log"
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
	cursor := 0
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
		repo, ok := nextWithWork(targets, repos, &cursor)
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
		if _, err := runner.Run("auto", []scraper.RepoTarget{repo}); err != nil {
			log.Printf("worker: run error for %s/%s: %v", repo.Owner, repo.Repo, err)
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		// Gentle gap between batches (the scrape itself is throttled by the
		// per-call Delay; this just avoids back-to-back bursts).
		if !sleep(ctx, 3*time.Second) {
			return
		}
	}
}

// nextWithWork returns the next repo (from cursor, wrapping once) that still has
// stargazers left to walk. A repo with an unknown total (-1) is considered to
// have work (its total gets fetched on the first scrape).
func nextWithWork(targets []scraper.RepoTarget, repos *RepoStore, cursor *int) (scraper.RepoTarget, bool) {
	n := len(targets)
	for i := 0; i < n; i++ {
		idx := (*cursor + i) % n
		t := targets[idx]
		processed, total := repos.Progress(t.Owner + "/" + t.Repo)
		if total < 0 || processed < total {
			*cursor = idx + 1
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
