package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"stargazer/internal/repoqueue"
	"stargazer/internal/repos"
	"stargazer/internal/scraper"
)

//go:embed web/index.html
var dashboardHTML []byte

// Server exposes the dashboard and JSON API.
type Server struct {
	cfg      Config
	queue    *repoqueue.Queue
	runner   *Runner
	settings *SettingsStore
	stats    *StatsStore
	repos    *RepoStore
	http     *http.Server
}

// New builds the HTTP server and routes.
func New(cfg Config, q *repoqueue.Queue, runner *Runner, settings *SettingsStore, stats *StatsStore, repoStore *RepoStore) *Server {
	s := &Server{cfg: cfg, queue: q, runner: runner, settings: settings, stats: stats, repos: repoStore}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/scrape", s.handleScrape)
	mux.HandleFunc("/api/queue", s.handleQueue)
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) queueOrder() []string {
	targets := s.queue.AllTargets()
	order := make([]string, len(targets))
	for i, t := range targets {
		order[i] = t.Owner + "/" + t.Repo
	}
	return order
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	totals, history := s.stats.Snapshot()
	var lastRun *RunReport
	if len(history) > 0 {
		lastRun = history[0]
	} else {
		lastRun = s.runner.Last()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running":   s.runner.Running(),
		"queue":     s.queue.Snapshot(),
		"repos":     s.repos.Snapshot(s.queueOrder()),
		"settings":  s.settings.Get(),
		"totals":    totals,
		"lastRun":   lastRun,
		"tokens":    len(s.cfg.Tokens),
		"listId":    s.cfg.EmailListID,
		"emailApi":  s.cfg.EmailAPIURL,
		"source":    s.cfg.Source,
		"timezone":  time.Now().Format("MST"),
		"serverNow": time.Now(),
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	_, history := s.stats.Snapshot()
	writeJSON(w, http.StatusOK, history)
}

// handleQueue: GET snapshot+progress, POST to add repos, DELETE to remove one.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"queue": s.queue.Snapshot(),
			"repos": s.repos.Snapshot(s.queueOrder()),
		})
	case http.MethodPost:
		var body struct {
			Repos []string `json:"repos"`
			Text  string   `json:"text"`
			Repo  string   `json:"repo"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw := strings.Join(body.Repos, "\n") + "\n" + body.Text + "\n" + body.Repo
		targets, warnings := repos.ParseList(raw)
		if len(targets) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no valid owner/repo entries", "warnings": warnings})
			return
		}
		added, err := s.queue.Add(targets)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		go s.runner.RefreshTotals(targets) // populate star totals for the dashboard
		writeJSON(w, http.StatusOK, map[string]any{"added": added, "warnings": warnings, "queue": s.queue.Snapshot()})
	case http.MethodDelete:
		var body struct {
			Repo string `json:"repo"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		owner, repo, _ := strings.Cut(strings.TrimSpace(body.Repo), "/")
		if owner == "" || repo == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo"})
			return
		}
		removed, err := s.queue.Remove(owner, repo)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "queue": s.queue.Snapshot()})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET, POST or DELETE"})
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.settings.Get())
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		var patch SettingsPatch
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.settings.Update(patch))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
	}
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
		_ = json.NewDecoder(r.Body).Decode(&body)
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
