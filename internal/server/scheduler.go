package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// parseDaily parses "HH:MM" (24h) into hour and minute.
func parseDaily(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, err
	}
	return t.Hour(), t.Minute(), nil
}

// nextRun returns the next occurrence of hour:minute at or after `from`.
func nextRun(from time.Time, hour, minute int) time.Time {
	next := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, from.Location())
	if !next.After(from) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// StartScheduler launches a goroutine that invokes a scrape run once per day at
// cfg.ScrapeAt, until ctx is cancelled. An empty ScrapeAt disables scheduling.
func StartScheduler(ctx context.Context, cfg Config, runner *Runner) error {
	if strings.TrimSpace(cfg.ScrapeAt) == "" {
		log.Printf("scheduler: SCRAPE_AT empty — daily scheduling disabled")
		return nil
	}
	hour, minute, err := parseDaily(cfg.ScrapeAt)
	if err != nil {
		return fmt.Errorf("invalid SCRAPE_AT %q (want HH:MM): %w", cfg.ScrapeAt, err)
	}
	go runSchedulerLoop(ctx, hour, minute, func() {
		if _, err := runner.Run("schedule", nil); err != nil {
			log.Printf("scheduled run error: %v", err)
		}
	})
	return nil
}

func runSchedulerLoop(ctx context.Context, hour, minute int, run func()) {
	for {
		next := nextRun(time.Now(), hour, minute)
		wait := time.Until(next)
		log.Printf("scheduler: next run at %s (in %s)", next.Format(time.RFC3339), wait.Truncate(time.Second))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			run()
		}
	}
}
