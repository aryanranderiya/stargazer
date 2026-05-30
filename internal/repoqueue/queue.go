// Package repoqueue provides a forward-advancing cursor over a persisted list
// of repositories. The list is read from a text file (one "owner/repo" per
// line) and the cursor position is persisted to a JSON state file, so daily
// scrape runs resume where the previous run stopped instead of restarting.
package repoqueue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"stargazer/internal/repos"
	"stargazer/internal/scraper"
)

// Queue is a persistent, thread-safe cursor over a repo list.
type Queue struct {
	mu        sync.Mutex
	reposPath string
	statePath string

	targets  []scraper.RepoTarget
	warnings []string
	state    State
}

// State is the persisted cursor and bookkeeping for the queue.
type State struct {
	Cursor    int       `json:"cursor"` // index of the NEXT repo to scrape
	Cycles    int       `json:"cycles"` // number of complete passes over the list
	UpdatedAt time.Time `json:"updatedAt"`
}

// Snapshot is a read-only view of the queue for status reporting.
type Snapshot struct {
	Total    int                 `json:"total"`
	Cursor   int                 `json:"cursor"`
	Cycles   int                 `json:"cycles"`
	Next     *scraper.RepoTarget `json:"next,omitempty"`
	Warnings []string            `json:"warnings,omitempty"`
}

// Open loads the repo list and cursor state from disk.
func Open(reposPath, statePath string) (*Queue, error) {
	q := &Queue{reposPath: reposPath, statePath: statePath}
	if err := q.reload(); err != nil {
		return nil, err
	}
	if err := q.loadState(); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *Queue) reload() error {
	data, err := os.ReadFile(q.reposPath)
	if err != nil {
		return fmt.Errorf("reading repos file %s: %w", q.reposPath, err)
	}
	q.targets, q.warnings = repos.ParseList(string(data))
	return nil
}

func (q *Queue) loadState() error {
	data, err := os.ReadFile(q.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			q.state = State{}
			return nil
		}
		return fmt.Errorf("reading state file %s: %w", q.statePath, err)
	}
	return json.Unmarshal(data, &q.state)
}

func (q *Queue) persist() error {
	if err := os.MkdirAll(filepath.Dir(q.statePath), 0o755); err != nil {
		return err
	}
	q.state.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(q.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := q.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, q.statePath) // atomic replace
}

// Next returns up to n repositories starting at the current cursor, advancing
// and persisting the cursor. The queue wraps to the start after the final
// repo (incrementing Cycles); because the email platform dedupes contacts by
// email, repeated passes are harmless and act as a slow refresh.
func (q *Queue) Next(n int) ([]scraper.RepoTarget, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.targets) == 0 {
		return nil, fmt.Errorf("repo queue is empty (%s); add 'owner/repo' lines", q.reposPath)
	}
	if n < 1 {
		n = 1
	}
	if n > len(q.targets) {
		n = len(q.targets)
	}

	cursor := q.state.Cursor
	if cursor < 0 || cursor >= len(q.targets) {
		cursor = 0
	}
	out := make([]scraper.RepoTarget, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, q.targets[cursor])
		cursor++
		if cursor >= len(q.targets) {
			cursor = 0
			q.state.Cycles++
		}
	}
	q.state.Cursor = cursor
	if err := q.persist(); err != nil {
		return out, fmt.Errorf("persisting queue state: %w", err)
	}
	return out, nil
}

// Snapshot returns the current queue position for status endpoints.
func (q *Queue) Snapshot() Snapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	var next *scraper.RepoTarget
	if len(q.targets) > 0 {
		c := q.state.Cursor
		if c < 0 || c >= len(q.targets) {
			c = 0
		}
		t := q.targets[c]
		next = &t
	}
	return Snapshot{
		Total:    len(q.targets),
		Cursor:   q.state.Cursor,
		Cycles:   q.state.Cycles,
		Next:     next,
		Warnings: append([]string(nil), q.warnings...),
	}
}

// AllTargets returns a copy of the queued repos in order.
func (q *Queue) AllTargets() []scraper.RepoTarget {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]scraper.RepoTarget, len(q.targets))
	copy(out, q.targets)
	return out
}

// Add appends new (deduped) repos to the queue file and reloads. Returns the
// number actually added.
func (q *Queue) Add(refs []scraper.RepoTarget) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	existing := make(map[string]bool, len(q.targets))
	for _, t := range q.targets {
		existing[t.Owner+"/"+t.Repo] = true
	}
	var toAppend []string
	for _, r := range refs {
		k := r.Owner + "/" + r.Repo
		if r.Owner == "" || r.Repo == "" || existing[k] {
			continue
		}
		existing[k] = true
		toAppend = append(toAppend, k)
	}
	if len(toAppend) == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(q.reposPath), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(q.reposPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	for _, l := range toAppend {
		fmt.Fprintln(f, l)
	}
	_ = f.Close()
	if err := q.reload(); err != nil {
		return len(toAppend), err
	}
	return len(toAppend), nil
}

// Remove drops a repo from the queue (rewriting the file) and reloads.
func (q *Queue) Remove(owner, repo string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	target := owner + "/" + repo
	kept := make([]string, 0, len(q.targets))
	found := false
	for _, t := range q.targets {
		k := t.Owner + "/" + t.Repo
		if k == target {
			found = true
			continue
		}
		kept = append(kept, k)
	}
	if !found {
		return false, nil
	}
	content := ""
	if len(kept) > 0 {
		content = strings.Join(kept, "\n") + "\n"
	}
	if err := os.WriteFile(q.reposPath, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := q.reload(); err != nil {
		return true, err
	}
	if q.state.Cursor >= len(q.targets) {
		q.state.Cursor = 0
		_ = q.persist()
	}
	return true, nil
}
