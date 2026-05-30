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

	// Scheduling ("HH:MM" 24h, server local time; empty disables the scheduler)
	ScrapeAt   string
	RunOnStart bool

	// Storage (mounted volume)
	DataDir   string
	ReposPath string
	StatePath string
	CachePath string
	OutputDir string
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
		ScrapeAt:       getenv("SCRAPE_AT", "03:00"),
		RunOnStart:     getbool("RUN_ON_START", false),
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

	if c.EmailAPISecret == "" {
		return c, fmt.Errorf("EMAIL_API_SECRET is required")
	}
	if c.EmailListID == "" {
		return c, fmt.Errorf("EMAIL_LIST_ID is required")
	}
	if strings.TrimSpace(c.ScrapeAt) != "" {
		if _, _, err := parseDaily(c.ScrapeAt); err != nil {
			return c, fmt.Errorf("invalid SCRAPE_AT %q (want HH:MM): %w", c.ScrapeAt, err)
		}
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
