package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RepoProgress is the persisted per-repo scraping progress that powers the
// dashboard's progress bars.
type RepoProgress struct {
	Repo       string    `json:"repo"`
	TotalStars int       `json:"totalStars"` // from GitHub; -1 = unknown
	Processed  int       `json:"processed"`  // stargazers walked so far (deep-walk offset)
	Scraped    int       `json:"scraped"`    // cumulative scraped
	Imported   int       `json:"imported"`   // cumulative imported into the email list
	Cursor     string    `json:"cursor,omitempty"` // GraphQL stargazer resume cursor
	Done       bool      `json:"done,omitempty"`   // fully walked (no more stargazers)
	LastRunAt  time.Time `json:"lastRunAt,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
}

// RepoStore tracks per-repo progress, persisted to disk.
type RepoStore struct {
	mu   sync.Mutex
	path string
	m    map[string]*RepoProgress
}

// NewRepoStore loads per-repo progress from path.
func NewRepoStore(path string) *RepoStore {
	s := &RepoStore{path: path, m: map[string]*RepoProgress{}}
	if data, err := os.ReadFile(path); err == nil {
		var list []*RepoProgress
		if json.Unmarshal(data, &list) == nil {
			for _, p := range list {
				s.m[p.Repo] = p
			}
		}
	}
	return s
}

func (s *RepoStore) entry(repo string) *RepoProgress {
	p := s.m[repo]
	if p == nil {
		p = &RepoProgress{Repo: repo, TotalStars: -1}
		s.m[repo] = p
	}
	return p
}

// Offset returns the deep-walk resume point (stars already processed) for repo.
func (s *RepoStore) Offset(repo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.m[repo]; p != nil {
		return p.Processed
	}
	return 0
}

// Cursor returns the GraphQL stargazer resume cursor for repo (empty if none).
func (s *RepoStore) Cursor(repo string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.m[repo]; p != nil {
		return p.Cursor
	}
	return ""
}

// IsDone reports whether repo has been fully walked (no more stargazers).
func (s *RepoStore) IsDone(repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.m[repo]; p != nil {
		return p.Done
	}
	return false
}

// Progress returns (processed, total) for repo; total is -1 when unknown.
func (s *RepoStore) Progress(repo string) (processed, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.m[repo]; p != nil {
		return p.Processed, p.TotalStars
	}
	return 0, -1
}

// NeedsTotal reports whether the total star count is still unknown for repo.
func (s *RepoStore) NeedsTotal(repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[repo]
	return p == nil || p.TotalStars < 0
}

// SetTotal records the repo's total stargazer count.
func (s *RepoStore) SetTotal(repo string, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(repo).TotalStars = total
	_ = s.persistLocked()
}

// RecordRun folds a finished repo run into its progress. cursor is the GraphQL
// resume point reached this run; exhausted marks the repo fully walked.
func (s *RepoStore) RecordRun(repo string, scraped, imported int, errStr string, at time.Time, cursor string, exhausted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.entry(repo)
	p.Processed += scraped
	p.Scraped += scraped
	p.Imported += imported
	if cursor != "" {
		p.Cursor = cursor
	}
	if exhausted {
		p.Done = true // sticky: once fully walked, stop re-running it
	}
	p.LastRunAt = at
	p.LastError = errStr
	_ = s.persistLocked()
}

// Snapshot returns per-repo progress, ordered by the supplied queue order first
// (then any extra repos alphabetically).
func (s *RepoStore) Snapshot(order []string) []RepoProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []RepoProgress{}
	seen := map[string]bool{}
	// Emit an entry for every queued repo in order — synthesizing a
	// zero/unknown entry for repos not yet scraped so they still show with a
	// progress bar (and "fetching total…").
	for _, k := range order {
		if seen[k] {
			continue
		}
		seen[k] = true
		if p := s.m[k]; p != nil {
			out = append(out, *p)
		} else {
			out = append(out, RepoProgress{Repo: k, TotalStars: -1})
		}
	}
	var rest []string
	for k := range s.m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, *s.m[k])
	}
	return out
}

func (s *RepoStore) persistLocked() error {
	list := make([]*RepoProgress, 0, len(s.m))
	for _, p := range s.m {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Repo < list[j].Repo })
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
