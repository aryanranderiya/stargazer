package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	gh "stargazer/internal/github"
)

const ttl = 30 * 24 * time.Hour // 30 days — emails rarely change

type entry struct {
	User      gh.User   `json:"user"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Cache is a thread-safe, persistent on-disk store of GitHub user profiles.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry
	path    string
	dirty   bool
}

// Load reads the cache from disk. Returns an empty cache if the file doesn't
// exist or if path is empty.
func Load(path string) (*Cache, error) {
	c := &Cache{
		entries: make(map[string]entry),
		path:    path,
	}

	if path == "" {
		return c, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}

	if err := json.Unmarshal(data, &c.entries); err != nil {
		// If the file is corrupt, start fresh rather than failing hard.
		c.entries = make(map[string]entry)
		return c, nil
	}

	return c, nil
}

// Get returns the user if cached and within TTL.
func (c *Cache) Get(login string) (*gh.User, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[login]
	if !ok {
		return nil, false
	}
	if time.Since(e.FetchedAt) > ttl {
		return nil, false
	}
	u := e.User
	return &u, true
}

// SetMany bulk-stores users with the current timestamp.
func (c *Cache) SetMany(users map[string]*gh.User) {
	if len(users) == 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	for login, u := range users {
		if u == nil {
			continue
		}
		c.entries[login] = entry{User: *u, FetchedAt: now}
	}
	c.dirty = true
}

// Save writes the cache to disk only if dirty.
// It uses an atomic write (temp file + rename) for safety.
func (c *Cache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.dirty {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(c.entries)
	if err != nil {
		return err
	}

	// Write to a temp file in the same directory, then rename atomically.
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".cache_tmp_*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		os.Remove(tmpName)
		return err
	}

	c.dirty = false
	return nil
}
