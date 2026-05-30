package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Settings are the runtime-adjustable scrape parameters. They are editable from
// the dashboard (to slow things down or pause) and persisted to disk so they
// survive restarts.
type Settings struct {
	Paused       bool `json:"paused"`
	AutoTune     bool `json:"autoTune"` // self-adjust delay based on fetch-failure rate
	ReposPerRun  int  `json:"reposPerRun"`
	MaxStars     int  `json:"maxStars"` // 0 = all stargazers of the repo
	DelayMs      int  `json:"delayMs"`
	Concurrency  int  `json:"concurrency"`
	MaxRepos     int  `json:"maxRepos"`
	MaxForkRepos int  `json:"maxForkRepos"`
}

// SettingsPatch is a partial update; nil fields are left unchanged.
type SettingsPatch struct {
	Paused       *bool `json:"paused"`
	AutoTune     *bool `json:"autoTune"`
	ReposPerRun  *int  `json:"reposPerRun"`
	MaxStars     *int  `json:"maxStars"`
	DelayMs      *int  `json:"delayMs"`
	Concurrency  *int  `json:"concurrency"`
	MaxRepos     *int  `json:"maxRepos"`
	MaxForkRepos *int  `json:"maxForkRepos"`
}

// SettingsStore is a thread-safe, disk-backed Settings holder.
type SettingsStore struct {
	mu   sync.RWMutex
	path string
	s    Settings
}

// NewSettingsStore loads settings from path, falling back to def (and writing
// def to disk) when the file is absent or unreadable.
func NewSettingsStore(path string, def Settings) *SettingsStore {
	st := &SettingsStore{path: path, s: def}
	if data, err := os.ReadFile(path); err == nil {
		var loaded Settings
		if json.Unmarshal(data, &loaded) == nil {
			st.s = loaded
		}
	} else {
		_ = st.persist()
	}
	return st
}

// Get returns a copy of the current settings.
func (st *SettingsStore) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s
}

// Update applies the non-nil fields of p (with light validation) and persists.
func (st *SettingsStore) Update(p SettingsPatch) Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	if p.Paused != nil {
		st.s.Paused = *p.Paused
	}
	if p.AutoTune != nil {
		st.s.AutoTune = *p.AutoTune
	}
	if p.ReposPerRun != nil && *p.ReposPerRun > 0 {
		st.s.ReposPerRun = *p.ReposPerRun
	}
	if p.MaxStars != nil && *p.MaxStars >= 0 {
		st.s.MaxStars = *p.MaxStars
	}
	if p.DelayMs != nil && *p.DelayMs >= 0 {
		st.s.DelayMs = *p.DelayMs
	}
	if p.Concurrency != nil && *p.Concurrency > 0 {
		st.s.Concurrency = *p.Concurrency
	}
	if p.MaxRepos != nil && *p.MaxRepos >= 0 {
		st.s.MaxRepos = *p.MaxRepos
	}
	if p.MaxForkRepos != nil && *p.MaxForkRepos >= 0 {
		st.s.MaxForkRepos = *p.MaxForkRepos
	}
	_ = st.persist()
	return st.s
}

func (st *SettingsStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st.s, "", "  ")
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
