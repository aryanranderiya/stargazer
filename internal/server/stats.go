package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Totals are cumulative counters across every run since first boot — the
// numbers the dashboard surfaces ("X emails ingested" etc.).
type Totals struct {
	Runs       int       `json:"runs"`
	Scraped    int       `json:"scraped"`
	Sent       int       `json:"sent"`
	Imported   int       `json:"imported"`
	Skipped    int       `json:"skipped"`
	Suppressed int       `json:"suppressed"`
	Noreply    int       `json:"noreply"`
	Invalid    int       `json:"invalid"`
	LastRunAt  time.Time `json:"lastRunAt,omitempty"`
}

type persistedStats struct {
	Totals  Totals       `json:"totals"`
	History []*RunReport `json:"history"`
}

// StatsStore tracks cumulative totals plus a bounded, newest-first history of
// run reports, persisted to disk for durability across restarts.
type StatsStore struct {
	mu      sync.Mutex
	path    string
	cap     int
	totals  Totals
	history []*RunReport
}

// NewStatsStore loads prior stats from path (cap = max history entries kept).
func NewStatsStore(path string, capacity int) *StatsStore {
	if capacity <= 0 {
		capacity = 50
	}
	s := &StatsStore{path: path, cap: capacity}
	if data, err := os.ReadFile(path); err == nil {
		var p persistedStats
		if json.Unmarshal(data, &p) == nil {
			s.totals = p.Totals
			s.history = p.History
			if len(s.history) > capacity {
				s.history = s.history[:capacity]
			}
		}
	}
	return s
}

// Record folds a finished run into the totals and history, then persists.
func (s *StatsStore) Record(r *RunReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals.Runs++
	s.totals.LastRunAt = r.FinishedAt
	for _, rep := range r.Repos {
		s.totals.Scraped += rep.Count
		s.totals.Sent += rep.Push.Sent
		s.totals.Imported += rep.Push.Imported
		s.totals.Skipped += rep.Push.Skipped
		s.totals.Suppressed += rep.Push.Suppressed
		s.totals.Noreply += rep.Push.Noreply
		s.totals.Invalid += rep.Push.Invalid
	}
	s.history = append([]*RunReport{r}, s.history...)
	if len(s.history) > s.cap {
		s.history = s.history[:s.cap]
	}
	_ = s.persist()
}

// Snapshot returns the current totals and a copy of the run history.
func (s *StatsStore) Snapshot() (Totals, []*RunReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := make([]*RunReport, len(s.history))
	copy(h, s.history)
	return s.totals, h
}

func (s *StatsStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(persistedStats{Totals: s.totals, History: s.history}, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
