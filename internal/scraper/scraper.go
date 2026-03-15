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

	"stargazer/internal/cache"
	gh "stargazer/internal/github"
)

// RepoTarget identifies a single GitHub repository.
type RepoTarget struct {
	Owner string
	Repo  string
}

// Config holds all configurable parameters for a scrape run.
type Config struct {
	Repos        []RepoTarget // repositories to scrape, processed sequentially
	Tokens       []string
	OutputDir    string // base directory for output CSVs (e.g. "stars/")
	Concurrency  int
	MaxRepos     int // max own (non-fork) repos to scan per user
	MaxForkRepos int // max forked repos to scan per user
	MaxStars     int // 0 = fetch all
	Delay        time.Duration
	UseSearchAPI bool   // use the GitHub commit search API as a last resort
	CachePath    string // path for on-disk user profile cache; defaults to $XDG_CONFIG/stargazer/user_cache.json
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

	// Populated on the initial rate-limit probe message.
	RateLimits []gh.RateLimitStatus

	Done  bool
	Error error

	// Populated only when Done=true.
	OutputPath string
	Count      int

	// Multi-repo tracking: which repo is currently being processed.
	RepoName  string // "owner/repo"
	RepoIndex int    // 0-based index of current repo
	RepoTotal int    // total number of repos to scrape
}

// workerCfg bundles per-worker config passed into processUser.
type workerCfg struct {
	maxRepos        int
	maxForkRepos    int
	targetOwner     string
	targetRepo      string
	useSearchAPI    bool
	profiles        map[string]*gh.User  // pre-fetched profiles; may be nil
	prefetchedRepos map[string][]gh.Repo // pre-fetched repos via GraphQL; may be nil
}

var csvHeaders = []string{
	"login", "name", "email", "email_source",
	"company", "location", "bio",
	"followers", "public_repos", "starred_at", "profile_url",
}

// Run executes the full scrape pipeline for all repos in cfg.Repos
// and streams Progress updates to progressCh.
// Designed to be called in a goroutine.
func Run(cfg Config, progressCh chan<- Progress) {
	client := gh.NewClient(cfg.Tokens, cfg.Delay)

	// Resolve cache path.
	cachePath := cfg.CachePath
	if cachePath == "" {
		if configDir, err := os.UserConfigDir(); err == nil {
			cachePath = filepath.Join(configDir, "stargazer", "user_cache.json")
		}
	}
	userCache, err := cache.Load(cachePath)
	if err != nil {
		// Non-fatal: fall back to an in-memory-only cache.
		userCache, _ = cache.Load("")
	}

	send(progressCh, Progress{
		Stage:      "fetching",
		Status:     "Checking rate limits...",
		RateLimits: client.ProbeAllTokens(),
	})

	totalRepos := len(cfg.Repos)
	for ri, repo := range cfg.Repos {
		repoName := repo.Owner + "/" + repo.Repo
		outputPath := filepath.Join(cfg.OutputDir, repo.Owner, repo.Repo+".csv")

		send(progressCh, Progress{
			Stage:     "fetching",
			Status:    fmt.Sprintf("Starting repo %d/%d: %s", ri+1, totalRepos, repoName),
			RepoName:  repoName,
			RepoIndex: ri,
			RepoTotal: totalRepos,
		})

		count, repoOutputPath, err := runRepo(client, cfg, repo, outputPath, userCache, progressCh, ri, totalRepos)
		if err != nil {
			send(progressCh, Progress{
				Done:      true,
				Error:     fmt.Errorf("repo %s: %w", repoName, err),
				RepoName:  repoName,
				RepoIndex: ri,
				RepoTotal: totalRepos,
			})
			return
		}

		// If this was the last repo, signal done with the final output path and total count.
		if ri == totalRepos-1 {
			send(progressCh, Progress{
				Done:       true,
				OutputPath: repoOutputPath,
				Count:      count,
				RepoName:   repoName,
				RepoIndex:  ri,
				RepoTotal:  totalRepos,
			})
		}
	}

	// Persist the user profile cache to disk.
	_ = userCache.Save()
}

// runRepo scrapes a single repo and writes results to outputPath.
// Returns the count of results, the resolved output path, and any error.
func runRepo(client *gh.Client, cfg Config, repo RepoTarget, outputPath string, userCache *cache.Cache, progressCh chan<- Progress, repoIdx, repoTotal int) (int, string, error) {
	repoName := repo.Owner + "/" + repo.Repo

	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, "", fmt.Errorf("creating output directory: %w", err)
		}
	}

	resolved := resolveOutputPath(outputPath)
	f, err := os.Create(resolved)
	if err != nil {
		return 0, "", fmt.Errorf("creating output file: %w", err)
	}
	defer f.Close()

	csvWriter := csv.NewWriter(f)
	if err := csvWriter.Write(csvHeaders); err != nil {
		return 0, "", fmt.Errorf("writing CSV headers: %w", err)
	}
	var csvMu sync.Mutex

	fetchErrCh := make(chan error, 1)
	starCh := client.StreamStargazers(repo.Owner, repo.Repo, cfg.MaxStars, cfg.Concurrency, func(fetched int) {
		send(progressCh, Progress{
			Stage:     "fetching",
			Status:    fmt.Sprintf("[%s] Fetched %d stargazers...", repoName, fetched),
			RepoName:  repoName,
			RepoIndex: repoIdx,
			RepoTotal: repoTotal,
		})
	}, fetchErrCh)

	const profileBatchSize = 20

	workCh := make(chan gh.StarEntry, profileBatchSize*2)
	resultCh := make(chan Result, profileBatchSize*2)

	var profileMu sync.RWMutex
	profiles := make(map[string]*gh.User)
	prefetchedRepos := make(map[string][]gh.Repo)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for star := range workCh {
				profileMu.RLock()
				wcfg := workerCfg{
					maxRepos:        cfg.MaxRepos,
					maxForkRepos:    cfg.MaxForkRepos,
					targetOwner:     repo.Owner,
					targetRepo:      repo.Repo,
					useSearchAPI:    cfg.UseSearchAPI,
					profiles:        profiles,
					prefetchedRepos: prefetchedRepos,
				}
				profileMu.RUnlock()
				resultCh <- processUser(client, star, wcfg)
			}
		}()
	}

	// errFromProducer is set if the producer goroutine encounters a fatal error.
	var producerErr error

	go func() {
		type prefetchResult struct {
			entries         []gh.StarEntry
			profiles        map[string]*gh.User
			prefetchedRepos map[string][]gh.Repo
		}

		startPrefetch := func(batch []gh.StarEntry) <-chan prefetchResult {
			ch := make(chan prefetchResult, 1)
			entries := make([]gh.StarEntry, len(batch))
			copy(entries, batch)
			go func() {
				var needFetch []string
				cached := make(map[string]*gh.User)
				for _, s := range entries {
					if u, ok := userCache.Get(s.User.Login); ok {
						cached[s.User.Login] = u
					} else {
						needFetch = append(needFetch, s.User.Login)
					}
				}

				profileMu.Lock()
				for k, v := range cached {
					profiles[k] = v
				}
				profileMu.Unlock()

				fetched := make(map[string]*gh.User)
				fetchedRepos := make(map[string][]gh.Repo)
				if len(needFetch) > 0 {
					fetched, fetchedRepos = client.GetUsersBatch(needFetch, nil)
					userCache.SetMany(fetched)
				}

				ch <- prefetchResult{
					entries:         entries,
					profiles:        fetched,
					prefetchedRepos: fetchedRepos,
				}
			}()
			return ch
		}

		dispatchPrefetched := func(pending <-chan prefetchResult) {
			if pending == nil {
				return
			}
			res := <-pending
			profileMu.Lock()
			for k, v := range res.profiles {
				profiles[k] = v
			}
			for k, v := range res.prefetchedRepos {
				prefetchedRepos[k] = v
			}
			profileMu.Unlock()
			for _, star := range res.entries {
				workCh <- star
			}
		}

		var batch []gh.StarEntry
		var pending <-chan prefetchResult
		total := 0

		for star := range starCh {
			total++
			batch = append(batch, star)
			if len(batch) >= profileBatchSize {
				send(progressCh, Progress{
					Stage:     "processing",
					Status:    fmt.Sprintf("[%s] Processing stargazers... (%d fetched so far)", repoName, total),
					Total:     total,
					RepoName:  repoName,
					RepoIndex: repoIdx,
					RepoTotal: repoTotal,
				})
				newPending := startPrefetch(batch)
				batch = batch[:0]
				dispatchPrefetched(pending)
				pending = newPending
			}
		}

		if len(batch) > 0 {
			newPending := startPrefetch(batch)
			dispatchPrefetched(pending)
			pending = newPending
		}
		dispatchPrefetched(pending)

		if fetchErr := <-fetchErrCh; fetchErr != nil && total == 0 {
			producerErr = fetchErr
			close(workCh)
			wg.Wait()
			close(resultCh)
			return
		}

		if total == 0 {
			close(workCh)
			wg.Wait()
			close(resultCh)
			return
		}

		send(progressCh, Progress{
			Stage:     "processing",
			Status:    fmt.Sprintf("[%s] All %d stargazers fetched. Processing...", repoName, total),
			Total:     total,
			RepoName:  repoName,
			RepoIndex: repoIdx,
			RepoTotal: repoTotal,
		})

		close(workCh)
		wg.Wait()
		close(resultCh)
	}()

	count := 0
	const flushInterval = 50
	for result := range resultCh {
		row := []string{
			result.Login, result.Name, result.Email, result.EmailSource,
			result.Company, result.Location, result.Bio,
			strconv.Itoa(result.Followers), strconv.Itoa(result.PublicRepos),
			result.StarredAt, result.ProfileURL,
		}
		csvMu.Lock()
		_ = csvWriter.Write(row)
		if count%flushInterval == 0 {
			csvWriter.Flush()
		}
		csvMu.Unlock()

		count++
		send(progressCh, Progress{
			Stage:     "processing",
			Current:   count,
			RepoName:  repoName,
			RepoIndex: repoIdx,
			RepoTotal: repoTotal,
			Result: &UserResult{
				Login:       result.Login,
				Email:       result.Email,
				EmailSource: result.EmailSource,
				FetchFailed: result.FetchFailed,
			},
		})
	}

	csvMu.Lock()
	csvWriter.Flush()
	csvMu.Unlock()

	if producerErr != nil {
		return 0, "", producerErr
	}

	return count, resolved, nil
}

// processUser enriches a single stargazer entry with profile + email data.
func processUser(client *gh.Client, star gh.StarEntry, cfg workerCfg) Result {
	result := Result{
		Login:      star.User.Login,
		ID:         star.User.ID,
		StarredAt:  star.StarredAt,
		ProfileURL: fmt.Sprintf("https://github.com/%s", star.User.Login),
	}

	var fetchErr error
	user, hasCached := cfg.profiles[star.User.Login]
	if !hasCached {
		user, fetchErr = client.GetUser(star.User.Login)
	}
	if fetchErr != nil || user == nil {
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

	// Track repos we've already scanned to avoid duplicate commit lookups.
	scanned := make(map[string]bool)

	// 1. Public profile email.
	if gh.IsRealEmail(result.Email) {
		result.EmailSource = "profile"
		return result
	}

	// 2 & 3. Target repo commits and event data — fetched concurrently.
	type commitResult struct{ email string }
	type eventResult struct {
		email string
		repos []string
		err   error
	}

	commitCh := make(chan commitResult, 1)
	eventCh := make(chan eventResult, 1)

	if cfg.targetOwner != "" && cfg.targetRepo != "" {
		repoFull := cfg.targetOwner + "/" + cfg.targetRepo
		scanned[repoFull] = true
		go func() {
			commitCh <- commitResult{firstRealEmailInCommits(client, repoFull, star.User.Login)}
		}()
	} else {
		commitCh <- commitResult{}
	}

	go func() {
		email, repos, err := client.GetUserEventData(star.User.Login)
		eventCh <- eventResult{email, repos, err}
	}()

	cr := <-commitCh
	if cr.email != "" {
		result.Email = cr.email
		result.EmailSource = "commit"
		// Drain event goroutine.
		<-eventCh
		return result
	}

	er := <-eventCh
	if er.err == nil {
		if er.email != "" {
			result.Email = er.email
			result.EmailSource = "events"
			return result
		}
		cap := 3
		for _, repoName := range er.repos {
			if cap == 0 {
				break
			}
			if scanned[repoName] {
				continue
			}
			scanned[repoName] = true
			cap--
			if email := firstRealEmailInCommits(client, repoName, star.User.Login); email != "" {
				result.Email = email
				result.EmailSource = "commit"
				return result
			}
		}
	}

	// 4, 5 & 6 — run user repos (steps 4+5) and org repos (step 6) concurrently.
	// Each goroutine gets its own scanned set so there are no data races.
	type repoEmailResult struct {
		email  string
		source string
	}
	parallelCh := make(chan repoEmailResult, 2)

	// Copy scanned set for each goroutine.
	copyScanned := func() map[string]bool {
		m := make(map[string]bool, len(scanned))
		for k, v := range scanned {
			m[k] = v
		}
		return m
	}

	// Goroutine A: steps 4+5 — own repos then forked repos.
	go func() {
		if cfg.maxRepos == 0 && cfg.maxForkRepos == 0 {
			parallelCh <- repoEmailResult{}
			return
		}
		localScanned := copyScanned()
		var repos []gh.Repo
		if pr, ok := cfg.prefetchedRepos[star.User.Login]; ok && pr != nil {
			repos = pr
		} else {
			repos, _ = client.GetUserRepos(star.User.Login)
		}

		// 4. Own non-fork repos (most likely to have original commits).
		if cfg.maxRepos > 0 {
			if email, src := scanRepos(client, repos, star.User.Login, cfg.maxRepos, false, localScanned); email != "" {
				parallelCh <- repoEmailResult{email, src}
				return
			}
		}

		// 5. Forked repos (many users only commit via forks).
		if cfg.maxForkRepos > 0 {
			if email, src := scanRepos(client, repos, star.User.Login, cfg.maxForkRepos, true, localScanned); email != "" {
				parallelCh <- repoEmailResult{email, src}
				return
			}
		}

		parallelCh <- repoEmailResult{}
	}()

	// Goroutine B: step 6 — org repos.
	go func() {
		localScanned := copyScanned()
		orgs, orgErr := client.GetUserOrgs(star.User.Login)
		if orgErr != nil {
			parallelCh <- repoEmailResult{}
			return
		}
		orgCap := 2
		for _, org := range orgs {
			if orgCap == 0 {
				break
			}
			orgCap--
			orgRepos, _ := client.GetOrgRepos(org, 10)
			repoCap := 2
			for _, repo := range orgRepos {
				if repoCap == 0 {
					break
				}
				if repo.Private || localScanned[repo.FullName] {
					continue
				}
				localScanned[repo.FullName] = true
				repoCap--
				if email := firstRealEmailInCommits(client, repo.FullName, star.User.Login); email != "" {
					parallelCh <- repoEmailResult{email, "commit"}
					return
				}
			}
		}
		parallelCh <- repoEmailResult{}
	}()

	// Wait for both goroutines; first real email wins.
	var firstHit repoEmailResult
	for i := 0; i < 2; i++ {
		r := <-parallelCh
		if r.email != "" && firstHit.email == "" {
			firstHit = r
		}
	}
	if firstHit.email != "" {
		result.Email = firstHit.email
		result.EmailSource = firstHit.source
		return result
	}

	// 7. Commit search API (searches ALL public repos on GitHub, separate rate limit).
	if cfg.useSearchAPI {
		if email, err := client.SearchCommitsByAuthor(star.User.Login); err == nil && email != "" {
			result.Email = email
			result.EmailSource = "search"
			return result
		}
	}

	// 8. Deterministic GitHub no-reply as absolute last resort.
	result.Email = gh.NoReplyEmail(result.ID, result.Login)
	result.EmailSource = "noreply"
	return result
}

// scanRepos checks commits across repos of the given fork/non-fork type,
// skipping any repos already present in the scanned set.
func scanRepos(client *gh.Client, repos []gh.Repo, login string, maxRepos int, wantFork bool, scanned map[string]bool) (string, string) {
	checked := 0
	for _, repo := range repos {
		if checked >= maxRepos {
			break
		}
		if repo.Fork != wantFork || repo.Private || scanned[repo.FullName] {
			continue
		}
		scanned[repo.FullName] = true
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
