package server

import (
	"sort"
	"sync"
	"time"
)

// ContactRow is one scraped contact's live outcome, shown in the dashboard's
// Contacts view.
type ContactRow struct {
	Login       string    `json:"login"`
	Email       string    `json:"email"`
	EmailSource string    `json:"emailSource"`
	Repo        string    `json:"repo"`
	Status      string    `json:"status"` // sent | noreply | invalid | duplicate
	At          time.Time `json:"at"`
}

// RecentStore is a fixed-size ring buffer of the most recently scraped contacts
// (live progress, like the CLI's stream). In-memory only.
type RecentStore struct {
	mu   sync.Mutex
	buf  []ContactRow
	head int
	full bool
	cap  int
}

// NewRecentStore creates a ring holding the last `capacity` contacts.
func NewRecentStore(capacity int) *RecentStore {
	if capacity <= 0 {
		capacity = 2000
	}
	return &RecentStore{buf: make([]ContactRow, capacity), cap: capacity}
}

// Add records a contact outcome.
func (s *RecentStore) Add(r ContactRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.head] = r
	s.head = (s.head + 1) % s.cap
	if s.head == 0 {
		s.full = true
	}
}

// Snapshot returns up to `limit` newest-first rows matching repo/status filters
// (empty = no filter), the per-status counts over the repo-filtered set, and
// the sorted list of all repos seen (for the filter dropdown).
func (s *RecentStore) Snapshot(repo, status string, limit int) ([]ContactRow, map[string]int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	n := s.cap
	if !s.full {
		n = s.head
	}
	out := make([]ContactRow, 0, limit)
	counts := map[string]int{"total": 0, "scraped": 0, "noreply": 0, "none": 0, "failed": 0}
	repoSet := map[string]struct{}{}
	for i := 0; i < n; i++ {
		idx := (s.head - 1 - i + 2*s.cap) % s.cap // newest-first
		r := s.buf[idx]
		if r.Repo != "" {
			repoSet[r.Repo] = struct{}{}
		}
		if repo != "" && r.Repo != repo {
			continue
		}
		counts["total"]++
		counts[r.Status]++
		if status != "" && r.Status != status {
			continue
		}
		if len(out) < limit {
			out = append(out, r)
		}
	}
	repos := make([]string, 0, len(repoSet))
	for k := range repoSet {
		repos = append(repos, k)
	}
	sort.Strings(repos)
	return out, counts, repos
}
