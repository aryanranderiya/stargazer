package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"stargazer/internal/repoqueue"
	"stargazer/internal/repos"
	"stargazer/internal/scraper"
)

// Server exposes operational HTTP endpoints over the runner and queue.
type Server struct {
	cfg    Config
	queue  *repoqueue.Queue
	runner *Runner
	http   *http.Server
}

// New builds the HTTP server and routes.
func New(cfg Config, q *repoqueue.Queue, runner *Runner) *Server {
	s := &Server{cfg: cfg, queue: q, runner: runner}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/queue", s.handleQueue)
	mux.HandleFunc("/scrape", s.handleScrape)
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"running":     s.runner.Running(),
		"queue":       s.queue.Snapshot(),
		"lastRun":     s.runner.Last(),
		"schedule":    s.cfg.ScrapeAt,
		"reposPerRun": s.cfg.ReposPerRun,
		"tokens":      len(s.cfg.Tokens),
		"pushNoreply": s.cfg.PushNoreply,
		"listId":      s.cfg.EmailListID,
		"emailApi":    s.cfg.EmailAPIURL,
	})
}

func (s *Server) handleQueue(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.queue.Snapshot())
}

// handleScrape triggers a run asynchronously. With no body it uses the queue;
// with {"repo":"owner/repo"} or {"repos":["a/b","c/d"]} it scrapes those
// specific repos without advancing the queue cursor.
func (s *Server) handleScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var body struct {
		Repo  string   `json:"repo"`
		Repos []string `json:"repos"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // empty/invalid body => use queue
	}

	parts := append([]string{}, body.Repos...)
	if strings.TrimSpace(body.Repo) != "" {
		parts = append(parts, body.Repo)
	}
	var override []scraper.RepoTarget
	if len(parts) > 0 {
		targets, warn := repos.ParseTargets(strings.Join(parts, ","))
		if warn != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": warn})
			return
		}
		override = targets
	}

	if s.runner.Running() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a scrape run is already in progress"})
		return
	}
	go func() {
		if _, err := s.runner.Run("manual", override); err != nil {
			log.Printf("manual run error: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "repos": override})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
