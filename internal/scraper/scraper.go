package scraper

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "stargazer/internal/github"
)

// Config holds all configurable parameters for a scrape run.
type Config struct {
	Owner        string
	Repo         string
	Tokens       []string
	OutputPath   string
	Concurrency  int
	MaxRepos     int // max repos to scan per user when looking for commit email
	MaxStars     int // 0 = fetch all
	Delay        time.Duration
	UseSearchAPI bool // use the GitHub commit search API as a last resort
}

// Result holds enriched data for one stargazer (written to CSV).
type Result struct {
	Login       string
	Name        string
	Email       string
	EmailSource string // "profile", "commit", "events", "search", or "noreply"
	FetchFailed bool
	Company     string
	Location    string
	Bio         string
	Followers   int
	PublicRepos int
	StarredAt   string
	ProfileURL  string
	ID          int64
}

// UserResult is a lightweight per-user summary sent to the UI.
type UserResult struct {
	Login       string
	Email       string
	EmailSource string
	FetchFailed bool
}

// Progress is sent on the progress channel to report state to the UI.
type Progress struct {
	Current int
	Total   int
	Stage   string // "fetching" or "processing"
	Status  string

	// Set when a single user finishes processing.
	Result *UserResult

	Done  bool
	Error error

	// Populated only when Done=true.
	OutputPath string
	Count      int
}

// workerCfg bundles per-worker config passed into processUser.
type workerCfg struct {
	maxRepos     int
	targetOwner  string
	targetRepo   string
	useSearchAPI bool
}

var csvHeaders = []string{
	"login", "name", "email", "email_source",
	"company", "location", "bio",
	"followers", "public_repos", "starred_at", "profile_url",
}

// Run executes the full scrape pipeline and streams Progress updates to progressCh.
// Designed to be called in a goroutine.
func Run(cfg Config, progressCh chan<- Progress) {
	client := gh.NewClient(cfg.Tokens, cfg.Delay)

	send(progressCh, Progress{Stage: "fetching", Status: "Connecting to GitHub API..."})

	stars, err := client.GetAllStargazers(cfg.Owner, cfg.Repo, cfg.MaxStars, func(fetched int) {
		send(progressCh, Progress{
			Stage:  "fetching",
			Status: fmt.Sprintf("Fetched %d stargazers...", fetched),
		})
	})
	if err != nil {
		send(progressCh, Progress{Done: true, Error: fmt.Errorf("fetching stargazers: %w", err)})
		return
	}

	total := len(stars)
	if total == 0 {
		send(progressCh, Progress{Done: true, OutputPath: cfg.OutputPath, Count: 0})
		return
	}

	send(progressCh, Progress{
		Stage:  "processing",
		Status: fmt.Sprintf("Found %d stargazers. Starting workers...", total),
		Total:  total,
	})

	dir := filepath.Dir(cfg.OutputPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			send(progressCh, Progress{Done: true, Error: fmt.Errorf("creating output directory: %w", err)})
			return
		}
	}

	outputPath := resolveOutputPath(cfg.OutputPath)
	f, err := os.Create(outputPath)
	if err != nil {
		send(progressCh, Progress{Done: true, Error: fmt.Errorf("creating output file: %w", err)})
		return
	}
	defer f.Close()

	csvWriter := csv.NewWriter(f)
	if err := csvWriter.Write(csvHeaders); err != nil {
		send(progressCh, Progress{Done: true, Error: fmt.Errorf("writing CSV headers: %w", err)})
		return
	}
	var csvMu sync.Mutex

	wcfg := workerCfg{
		maxRepos:     cfg.MaxRepos,
		targetOwner:  cfg.Owner,
		targetRepo:   cfg.Repo,
		useSearchAPI: cfg.UseSearchAPI,
	}

	workCh := make(chan gh.StarEntry, cfg.Concurrency*2)
	resultCh := make(chan Result, cfg.Concurrency*2)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for star := range workCh {
				resultCh <- processUser(client, star, wcfg)
			}
		}()
	}

	go func() {
		for _, star := range stars {
			workCh <- star
		}
		close(workCh)
		wg.Wait()
		close(resultCh)
	}()

	count := 0
	for result := range resultCh {
		row := []string{
			result.Login, result.Name, result.Email, result.EmailSource,
			result.Company, result.Location, result.Bio,
			strconv.Itoa(result.Followers), strconv.Itoa(result.PublicRepos),
			result.StarredAt, result.ProfileURL,
		}
		csvMu.Lock()
		_ = csvWriter.Write(row)
		csvWriter.Flush()
		csvMu.Unlock()

		count++
		send(progressCh, Progress{
			Stage:   "processing",
			Current: count,
			Total:   total,
			Result: &UserResult{
				Login:       result.Login,
				Email:       result.Email,
				EmailSource: result.EmailSource,
				FetchFailed: result.FetchFailed,
			},
		})
	}

	send(progressCh, Progress{Done: true, OutputPath: outputPath, Count: count})
}

// processUser enriches a single stargazer entry with profile + email data.
func processUser(client *gh.Client, star gh.StarEntry, cfg workerCfg) Result {
	result := Result{
		Login:      star.User.Login,
		ID:         star.User.ID,
		StarredAt:  star.StarredAt,
		ProfileURL: fmt.Sprintf("https://github.com/%s", star.User.Login),
	}

	user, err := client.GetUser(star.User.Login)
	if err != nil {
		result.FetchFailed = true
	} else {
		result.Name = user.Name
		result.Email = user.Email
		result.Company = clean(user.Company)
		result.Location = user.Location
		result.Bio = clean(user.Bio)
		result.Followers = user.Followers
		result.PublicRepos = user.PublicRepos
		result.ProfileURL = user.HTMLURL
		result.ID = user.ID
	}

	// --- Email resolution fallback chain ---

	// 1. Public profile email.
	if gh.IsRealEmail(result.Email) {
		result.EmailSource = "profile"
		return result
	}

	// 2. Target repo commits (free – we already know the repo).
	if cfg.targetOwner != "" && cfg.targetRepo != "" {
		repoFull := cfg.targetOwner + "/" + cfg.targetRepo
		if email := firstRealEmailInCommits(client, repoFull, star.User.Login); email != "" {
			result.Email = email
			result.EmailSource = "commit"
			return result
		}
	}

	// 3. Event repos — 1 API call; email from payload OR commit scan of repos seen in push events.
	if eventEmail, eventRepos, evErr := client.GetUserEventData(star.User.Login); evErr == nil {
		if eventEmail != "" {
			result.Email = eventEmail
			result.EmailSource = "events"
			return result
		}
		cap := 3
		for _, repoName := range eventRepos {
			if cap == 0 {
				break
			}
			cap--
			if email := firstRealEmailInCommits(client, repoName, star.User.Login); email != "" {
				result.Email = email
				result.EmailSource = "commit"
				return result
			}
		}
	}

	// 4. Own non-fork repos (most likely to have original commits).
	repos, _ := client.GetUserRepos(star.User.Login)

	if email, src := scanRepos(client, repos, star.User.Login, cfg.maxRepos, false); email != "" {
		result.Email = email
		result.EmailSource = src
		return result
	}

	// 5. Forked repos (many users only commit via forks).
	if email, src := scanRepos(client, repos, star.User.Login, cfg.maxRepos, true); email != "" {
		result.Email = email
		result.EmailSource = src
		return result
	}

	// 6. Commit search API (searches ALL public repos on GitHub, separate rate limit).
	if cfg.useSearchAPI {
		if email, err := client.SearchCommitsByAuthor(star.User.Login); err == nil && email != "" {
			result.Email = email
			result.EmailSource = "search"
			return result
		}
	}

	// 7. Deterministic GitHub no-reply as absolute last resort.
	result.Email = gh.NoReplyEmail(result.ID, result.Login)
	result.EmailSource = "noreply"
	return result
}

// scanRepos checks commits across repos of the given fork/non-fork type.
func scanRepos(client *gh.Client, repos []gh.Repo, login string, maxRepos int, wantFork bool) (string, string) {
	checked := 0
	for _, repo := range repos {
		if checked >= maxRepos {
			break
		}
		if repo.Fork != wantFork || repo.Private {
			continue
		}
		if email := firstRealEmailInCommits(client, repo.FullName, login); email != "" {
			return email, "commit"
		}
		checked++
	}
	return "", ""
}

// firstRealEmailInCommits returns the first non-noreply email found in commits
// authored by login in repoFullName.
func firstRealEmailInCommits(client *gh.Client, repoFullName, login string) string {
	commits, err := client.GetCommitsByUser(repoFullName, login)
	if err != nil {
		return ""
	}
	for _, cm := range commits {
		if gh.IsRealEmail(cm.Commit.Author.Email) {
			return cm.Commit.Author.Email
		}
	}
	return ""
}

// resolveOutputPath returns a non-destructive output path. If the given path
// already exists, it tries appending _1, _2 … _999 before the extension.
// If all suffixes are taken it falls back to the original path.
func resolveOutputPath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i <= 999; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return path
}

func clean(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func send(ch chan<- Progress, p Progress) {
	if p.Done || p.Error != nil {
		ch <- p
		return
	}
	select {
	case ch <- p:
	default:
	}
}
