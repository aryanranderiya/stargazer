// Package server wires the stargazer scraper into a long-running HTTP service
// with a daily scheduler, a persistent repo queue, and a pusher that imports
// scraped contacts into the GAIA email platform.
package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"stargazer/internal/credentials"
)

// Config holds the service configuration, populated from environment variables.
type Config struct {
	// HTTP
	Addr string

	// Email platform import API
	EmailAPIURL    string
	EmailAPISecret string
	EmailListID    string
	Source         string
	PushNoreply    bool

	// GitHub auth
	Tokens []string

	// Scrape tuning
	ReposPerRun  int
	MaxStars     int
	MaxRepos     int
	MaxForkRepos int
	Concurrency  int
	Delay        time.Duration
	UseSearchAPI bool

	// Storage (mounted volume)
	DataDir      string
	ReposPath    string
	StatePath    string
	CachePath    string
	OutputDir     string
	SettingsPath  string
	StatsPath     string
	RepoStatsPath string
}

// FromEnv builds a Config from environment variables, applying defaults.
func FromEnv() (Config, error) {
	c := Config{
		Addr:           getenv("STARGAZER_ADDR", ":8080"),
		EmailAPIURL:    getenv("EMAIL_API_URL", "http://api:3020"),
		EmailAPISecret: os.Getenv("EMAIL_API_SECRET"),
		EmailListID:    os.Getenv("EMAIL_LIST_ID"),
		Source:         getenv("STARGAZER_SOURCE", "stargazer"),
		PushNoreply:    getbool("PUSH_NOREPLY", false),
		ReposPerRun:    getint("REPOS_PER_RUN", 1),
		MaxStars:       getint("MAX_STARS", 0),
		MaxRepos:       getint("MAX_REPOS", 1),
		MaxForkRepos:   getint("MAX_FORK_REPOS", 1),
		Concurrency:    getint("CONCURRENCY", 5),
		Delay:          time.Duration(getint("REQUEST_DELAY_MS", 150)) * time.Millisecond,
		UseSearchAPI:   getbool("USE_SEARCH_API", false),
		DataDir:        getenv("DATA_DIR", "/data"),
	}

	if raw := strings.TrimSpace(os.Getenv("STARGAZER_GITHUB_TOKENS")); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				c.Tokens = append(c.Tokens, t)
			}
		}
	} else if creds, err := credentials.Load(); err == nil {
		c.Tokens = creds.Tokens
	}

	c.ReposPath = getenv("REPOS_FILE", filepath.Join(c.DataDir, "repos.txt"))
	c.StatePath = getenv("STATE_FILE", filepath.Join(c.DataDir, "state.json"))
	c.CachePath = getenv("CACHE_FILE", filepath.Join(c.DataDir, "user_cache.json"))
	c.OutputDir = getenv("OUTPUT_DIR", filepath.Join(c.DataDir, "out"))
	c.SettingsPath = getenv("SETTINGS_FILE", filepath.Join(c.DataDir, "settings.json"))
	c.StatsPath = getenv("STATS_FILE", filepath.Join(c.DataDir, "stats.json"))
	c.RepoStatsPath = getenv("REPOSTATS_FILE", filepath.Join(c.DataDir, "repostats.json"))

	if c.EmailAPISecret == "" {
		return c, fmt.Errorf("EMAIL_API_SECRET is required")
	}
	if c.EmailListID == "" {
		return c, fmt.Errorf("EMAIL_LIST_ID is required")
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getint(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getbool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

// InitialSettings derives the default runtime Settings from the env config.
// These are used the first time the service boots; thereafter the persisted
// settings.json (editable from the dashboard) takes over.
func (c Config) InitialSettings() Settings {
	return Settings{
		Paused:       false,
		ReposPerRun:  c.ReposPerRun,
		MaxStars:     c.MaxStars,
		DelayMs:      int(c.Delay / time.Millisecond),
		Concurrency:  c.Concurrency,
		MaxRepos:     c.MaxRepos,
		MaxForkRepos: c.MaxForkRepos,
	}
}
