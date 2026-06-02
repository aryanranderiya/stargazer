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
	MaxStars     int // max stargazers to scrape this run (0 = all from the offset onward)
	StartOffset  int // legacy REST page offset; only used to fast-forward when StartCursor is empty
	StartCursor  string // GraphQL stargazer cursor to resume from (preferred resume point)
	Delay        time.Duration
	UseSearchAPI bool   // use the GitHub commit search API as a last resort
	CachePath    string // path for on-disk user profile cache; defaults to $XDG_CONFIG/stargazer/user_cache.json
}

// Result holds enriched data for one stargazer (written to CSV).
type Result struct {
	Login       string
	Name        string
	Email       string
	EmailSource string // "profile", "commit", "search", or "noreply"
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
	Cursor     string // GraphQL stargazer cursor reached this run (resume point)
	Exhausted  bool   // true when the repo has no more stargazers to walk

	// Multi-repo tracking: which repo is currently being processed.
	RepoName  string // "owner/repo"
	RepoIndex int    // 0-based index of current repo
	RepoTotal int    // total number of repos to scrape
}

// workerCfg bundles per-worker config passed into processUser. The prefetched
// user/repos for THIS stargazer are copied in by the worker under the profile
// lock — processUser must never touch the shared prefetch maps directly, or it
// races the prefetcher writing the next batch ("concurrent map read and write").
type workerCfg struct {
	maxRepos     int
	maxForkRepos int
	targetOwner  string
	targetRepo   string
	useSearchAPI bool
	user         *gh.User  // pre-fetched profile for this stargazer; nil if not prefetched
	userRepos    []gh.Repo // pre-fetched repos (GraphQL) for this stargazer; nil if none
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

		count, repoOutputPath, cursor, exhausted, err := runRepo(client, cfg, repo, outputPath, userCache, progressCh, ri, totalRepos)
		if err != nil {
			send(progressCh, Progress{
				Done:      true,
				Error:     fmt.Errorf("repo %s: %w", repoName, err),
				RepoName:  repoName,
				RepoIndex: ri,
				RepoTotal: totalRepos,
				Count:     count, // partial progress before the error
				Cursor:    cursor, // persist however far we got before the error
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
				Cursor:     cursor,
				Exhausted:  exhausted,
			})
		}
	}

	// Persist the user profile cache to disk.
	_ = userCache.Save()
}

// runRepo scrapes a single repo and writes results to outputPath. Returns the
// count of results, the resolved output path, the stargazer cursor reached (to
// resume from next run), whether the repo is fully walked, and any error.
func runRepo(client *gh.Client, cfg Config, repo RepoTarget, outputPath string, userCache *cache.Cache, progressCh chan<- Progress, repoIdx, repoTotal int) (int, string, string, bool, error) {
	repoName := repo.Owner + "/" + repo.Repo

	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, "", "", false, fmt.Errorf("creating output directory: %w", err)
		}
	}

	resolved := resolveOutputPath(outputPath)
	f, err := os.Create(resolved)
	if err != nil {
		return 0, "", "", false, fmt.Errorf("creating output file: %w", err)
	}
	defer f.Close()

	csvWriter := csv.NewWriter(f)
	if err := csvWriter.Write(csvHeaders); err != nil {
		return 0, "", "", false, fmt.Errorf("writing CSV headers: %w", err)
	}
	var csvMu sync.Mutex

	// Resume by cursor when we have one; otherwise fast-forward past the legacy
	// REST page offset (one-time, validated to land at the same place).
	skip := 0
	if cfg.StartCursor == "" {
		skip = cfg.StartOffset
	}
	var finalCursor string = cfg.StartCursor
	var exhausted bool
	starCh, resCh := client.StreamStargazersGraphQL(repo.Owner, repo.Repo, cfg.MaxStars, skip, cfg.StartCursor, func(fetched int) {
		status := fmt.Sprintf("[%s] Fetched %d stargazers...", repoName, fetched)
		if skip > 0 && fetched == 0 {
			status = fmt.Sprintf("[%s] Fast-forwarding to resume point (offset %d)...", repoName, cfg.StartOffset)
		}
		send(progressCh, Progress{
			Stage:     "fetching",
			Status:    status,
			RepoName:  repoName,
			RepoIndex: repoIdx,
			RepoTotal: repoTotal,
		})
	})

	// One prefetch batch = profileBatchSize logins, fetched as concurrent GraphQL
	// chunks (GetUsersBatch parallelises internally, ~10 logins/chunk). Size it to
	// ONE chunk-round across all tokens — N tokens × 10 — so a batch resolves in
	// ~a single GraphQL round-trip regardless of token count; add tokens and the
	// batch (and prefetch throughput) scales automatically. Clamped for sanity.
	const graphqlChunkSize = 10 // mirrors GetUsersBatch's chunkSize
	profileBatchSize := len(cfg.Tokens) * graphqlChunkSize
	if profileBatchSize < 20 {
		profileBatchSize = 20
	}
	if profileBatchSize > 150 {
		profileBatchSize = 150
	}

	workCh := make(chan gh.StarEntry, profileBatchSize*3)
	resultCh := make(chan Result, profileBatchSize*3)

	var profileMu sync.RWMutex
	profiles := make(map[string]*gh.User)
	prefetchedRepos := make(map[string][]gh.Repo)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for star := range workCh {
				// Copy out THIS stargazer's prefetched data under the lock; never
				// hand processUser a reference to the shared maps (it would race the
				// prefetcher writing the next batch).
				profileMu.RLock()
				u := profiles[star.User.Login]
				ur := prefetchedRepos[star.User.Login]
				profileMu.RUnlock()
				wcfg := workerCfg{
					maxRepos:     cfg.MaxRepos,
					maxForkRepos: cfg.MaxForkRepos,
					targetOwner:  repo.Owner,
					targetRepo:   repo.Repo,
					useSearchAPI: cfg.UseSearchAPI,
					user:         u,
					userRepos:    ur,
				}
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

		// merge folds a finished prefetch into the shared maps and feeds its
		// stargazers to the worker pool.
		merge := func(res prefetchResult) {
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

		// Depth-N prefetch pipeline: keep up to prefetchDepth GetUsersBatch calls
		// in flight at once (each itself fetches its chunks concurrently) instead
		// of one batch at a time. The serial depth-1 pipeline was the throughput
		// ceiling (~750/min); running several batches in parallel — paced by the
		// per-token throttle + secondary-limit backoff — fills the token budget.
		// The buffered pendingCh gives backpressure; a dispatcher drains it IN
		// ORDER so stargazers still reach the workers in listing order.
		// Depth 4 saturates a single repo's sequential cursor listing (the listing
		// streams stargazers in order and shares the tokens with the prefetch, so
		// it — not the token budget — is the per-repo ceiling at ~2k/min). More
		// depth measured no faster; it just adds in-flight concurrency.
		const prefetchDepth = 4
		pendingCh := make(chan (<-chan prefetchResult), prefetchDepth)
		dispDone := make(chan struct{})
		go func() {
			defer close(dispDone)
			for ch := range pendingCh {
				merge(<-ch)
			}
		}()

		var batch []gh.StarEntry
		total := 0
		for star := range starCh {
			total++
			batch = append(batch, star)
			if len(batch) >= profileBatchSize {
				pendingCh <- startPrefetch(batch) // startPrefetch copies the batch synchronously
				batch = batch[:0]
				if total%(profileBatchSize*4) == 0 {
					send(progressCh, Progress{
						Stage: "processing", Status: fmt.Sprintf("[%s] Processing stargazers... (%d fetched so far)", repoName, total),
						Total: total, RepoName: repoName, RepoIndex: repoIdx, RepoTotal: repoTotal,
					})
				}
			}
		}
		if len(batch) > 0 {
			pendingCh <- startPrefetch(batch)
		}
		close(pendingCh)
		<-dispDone // all prefetched stargazers have been handed to the workers

		// The GraphQL stargazer stream reports its final state once (buffered),
		// available now without blocking. Capture resume cursor + exhausted flag.
		res := <-resCh
		finalCursor = res.LastCursor
		exhausted = res.Exhausted
		if res.Err != nil && total == 0 {
			producerErr = res.Err
		}

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
		return count, resolved, finalCursor, exhausted, producerErr
	}

	return count, resolved, finalCursor, exhausted, nil
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
	user := cfg.user
	if user == nil {
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

	// 2. Commit author-email from the user's own repos, resolved during the
	// GraphQL batch fetch (default-branch history) — no extra REST call. This
	// taps the otherwise-idle GraphQL rate-limit pool and resolves the bulk of
	// emails before any REST fallback runs.
	if user != nil && gh.IsRealEmail(user.CommitEmail) {
		result.Email = user.CommitEmail
		result.EmailSource = "commit"
		return result
	}

	// 3. Target-repo commits — the stargazer may be a contributor to the very
	// repo we're scraping.
	if cfg.targetOwner != "" && cfg.targetRepo != "" {
		repoFull := cfg.targetOwner + "/" + cfg.targetRepo
		scanned[repoFull] = true
		if email := firstRealEmailInCommits(client, repoFull, star.User.Login); email != "" {
			result.Email = email
			result.EmailSource = "commit"
			return result
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

	// Goroutine A: steps 4+5 — own repos then forked repos. Skipped entirely
	// when the user has no public repos (nothing to scan) or both caps are 0.
	go func() {
		if (cfg.maxRepos == 0 && cfg.maxForkRepos == 0) || result.PublicRepos == 0 {
			parallelCh <- repoEmailResult{}
			return
		}
		localScanned := copyScanned()
		var repos []gh.Repo
		if cfg.userRepos != nil {
			repos = cfg.userRepos
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
