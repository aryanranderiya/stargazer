// Command stargazer-server runs the stargazer scraper as a long-lived HTTP
// service: a daily scheduler pops repos from a persistent queue, scrapes their
// GitHub stargazers, and pushes the resulting contacts into the GAIA email
// platform via its import API. See cmd/server/README.md for configuration.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"stargazer/internal/pusher"
	"stargazer/internal/repoqueue"
	"stargazer/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := server.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir %s: %v", cfg.DataDir, err)
	}
	// Seed an empty, self-documenting repos file so operators see where to add repos.
	if _, statErr := os.Stat(cfg.ReposPath); os.IsNotExist(statErr) {
		_ = os.WriteFile(cfg.ReposPath,
			[]byte("# stargazer repo queue — one owner/repo per line.\n# Blank lines and '#' comments are ignored. Add hundreds; the cursor advances daily.\n"),
			0o644)
	}

	queue, err := repoqueue.Open(cfg.ReposPath, cfg.StatePath)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}

	recent := server.NewRecentStore(2000)
	push := &pusher.Client{
		BaseURL:     cfg.EmailAPIURL,
		Secret:      cfg.EmailAPISecret,
		ListID:      cfg.EmailListID,
		Source:      cfg.Source,
		PushNoreply: cfg.PushNoreply,
	}
	if err := push.Ping(); err != nil {
		log.Printf("WARNING: email import API not reachable at %s yet (%v) — will retry on each run", cfg.EmailAPIURL, err)
	} else {
		log.Printf("email import API reachable at %s (list %s)", cfg.EmailAPIURL, cfg.EmailListID)
	}

	settings := server.NewSettingsStore(cfg.SettingsPath, cfg.InitialSettings())
	stats := server.NewStatsStore(cfg.StatsPath, 50)
	repoStore := server.NewRepoStore(cfg.RepoStatsPath)
	runner := server.NewRunner(cfg, queue, push, settings, stats, repoStore, recent)
	audienceTotal := &atomic.Int64{}
	srv := server.New(cfg, queue, runner, settings, stats, repoStore, recent, audienceTotal)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Keep the true Scraped-audience size fresh (includes contacts added outside
	// this scraper, e.g. bulk imports), for the dashboard.
	refreshAudience := func() {
		if n, err := push.ListCount(); err == nil {
			audienceTotal.Store(int64(n))
		}
	}
	refreshAudience()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refreshAudience()
			}
		}
	}()

	// Populate total star counts for already-queued repos so the dashboard
	// shows their progress bars without waiting for the first scrape.
	go runner.RefreshTotals(queue.AllTargets())

	// Continuous queue worker: scrapes repos automatically (no schedule),
	// deep-walking each until done. Pausable via settings.
	server.StartWorker(ctx, runner, settings, queue, repoStore)

	go func() {
		log.Printf("stargazer-server listening on %s (tokens=%d, reposPerRun=%d, maxStars=%d, delayMs=%d, pushNoreply=%t)",
			cfg.Addr, len(cfg.Tokens), cfg.ReposPerRun, cfg.MaxStars, int(cfg.Delay.Milliseconds()), cfg.PushNoreply)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
