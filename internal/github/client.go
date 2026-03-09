package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const apiBase = "https://api.github.com"

// tokenSlot tracks per-token timing so that the delay is applied independently
// per token rather than globally. With N tokens the effective throughput is N×.
type tokenSlot struct {
	mu       sync.Mutex
	lastCall time.Time
}

// Client is a minimal GitHub REST API client.
type Client struct {
	tokens     []string
	tokenIdx   atomic.Int64
	httpClient *http.Client
	delay      time.Duration
	slots      []tokenSlot
}

// User represents a GitHub user profile.
type User struct {
	Login       string `json:"login"`
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Company     string `json:"company"`
	Location    string `json:"location"`
	Bio         string `json:"bio"`
	PublicRepos int    `json:"public_repos"`
	Followers   int    `json:"followers"`
	HTMLURL     string `json:"html_url"`
	Type        string `json:"type"`
}

// StarEntry is returned by the star+json accept header for stargazers.
type StarEntry struct {
	StarredAt string `json:"starred_at"`
	User      User   `json:"user"`
}

// Repo is a partial GitHub repository object.
type Repo struct {
	FullName string `json:"full_name"`
	Fork     bool   `json:"fork"`
	Private  bool   `json:"private"`
	PushedAt string `json:"pushed_at"`
}

// CommitResponse is a partial commit object from the commits list endpoint.
type CommitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
	} `json:"commit"`
}

// Event is a partial GitHub public event.
type Event struct {
	Type    string          `json:"type"`
	Repo    struct {
		Name string `json:"name"` // "owner/repo"
	} `json:"repo"`
	Payload json.RawMessage `json:"payload"`
}

// PushPayload is the payload for a PushEvent.
type PushPayload struct {
	Commits []struct {
		Author struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"author"`
	} `json:"commits"`
}

func NewClient(tokens []string, delay time.Duration) *Client {
	numSlots := len(tokens)
	if numSlots == 0 {
		numSlots = 1 // anonymous slot
	}
	return &Client{
		tokens:     tokens,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		delay:      delay,
		slots:      make([]tokenSlot, numSlots),
	}
}

// throttle sleeps only as long as needed to respect the per-token delay.
// With N tokens, N concurrent workers can each use a different token slot
// without blocking each other.
func (c *Client) throttle() {
	idx := 0
	if len(c.tokens) > 0 {
		idx = int(c.tokenIdx.Load() % int64(len(c.tokens)))
	}
	slot := &c.slots[idx]
	slot.mu.Lock()
	if c.delay > 0 {
		elapsed := time.Since(slot.lastCall)
		if elapsed < c.delay {
			time.Sleep(c.delay - elapsed)
		}
	}
	slot.lastCall = time.Now()
	slot.mu.Unlock()
}

func (c *Client) currentToken() string {
	if len(c.tokens) == 0 {
		return ""
	}
	idx := c.tokenIdx.Load() % int64(len(c.tokens))
	return c.tokens[idx]
}

func (c *Client) rotateToken() {
	c.tokenIdx.Add(1)
}

func (c *Client) doRequest(url, accept, token string, v interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		return json.NewDecoder(resp.Body).Decode(v)
	case 204:
		return nil
	case 401:
		return fmt.Errorf("unauthorized – check your GitHub token")
	case 403:
		return fmt.Errorf("rate limited (403)")
	case 429:
		return fmt.Errorf("rate limited (429)")
	case 404:
		return fmt.Errorf("not found (404): %s", url)
	case 422:
		return fmt.Errorf("unprocessable entity (422)")
	default:
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
}

func (c *Client) get(url, accept string, v interface{}) error {
	c.throttle()

	attempts := len(c.tokens)
	if attempts == 0 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		token := c.currentToken()
		err := c.doRequest(url, accept, token, v)
		if err == nil {
			return nil
		}
		msg := err.Error()
		if strings.HasPrefix(msg, "rate limited") && len(c.tokens) > 1 {
			c.rotateToken()
			lastErr = err
			continue
		}
		return err
	}
	return fmt.Errorf("all %d token(s) rate-limited: %w", len(c.tokens), lastErr)
}

// GetStargazersPage fetches one page (up to 100) of stargazers with star timestamps.
func (c *Client) GetStargazersPage(owner, repo string, page int) ([]StarEntry, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/stargazers?per_page=100&page=%d", apiBase, owner, repo, page)
	var entries []StarEntry
	err := c.get(url, "application/vnd.github.star+json", &entries)
	return entries, err
}

// GetAllStargazers fetches all stargazers across pages. maxCount=0 means fetch all.
func (c *Client) GetAllStargazers(owner, repo string, maxCount int, onPage func(int)) ([]StarEntry, error) {
	var all []StarEntry
	for page := 1; ; page++ {
		entries, err := c.GetStargazersPage(owner, repo, page)
		if err != nil {
			if len(all) > 0 {
				return all, nil
			}
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		all = append(all, entries...)
		if onPage != nil {
			onPage(len(all))
		}
		if len(entries) < 100 {
			break
		}
		if maxCount > 0 && len(all) >= maxCount {
			all = all[:maxCount]
			break
		}
	}
	return all, nil
}

// GetUser fetches a full user profile.
func (c *Client) GetUser(login string) (*User, error) {
	url := fmt.Sprintf("%s/users/%s", apiBase, login)
	var user User
	if err := c.get(url, "", &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUserRepos fetches the user's public repos sorted by most recently pushed.
func (c *Client) GetUserRepos(login string) ([]Repo, error) {
	url := fmt.Sprintf("%s/users/%s/repos?sort=pushed&direction=desc&per_page=30&type=owner", apiBase, login)
	var repos []Repo
	if err := c.get(url, "", &repos); err != nil {
		return nil, err
	}
	return repos, nil
}

// GetCommitsByUser fetches recent commits authored by login in repoFullName.
func (c *Client) GetCommitsByUser(repoFullName, login string) ([]CommitResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/commits?author=%s&per_page=5", apiBase, repoFullName, login)
	var commits []CommitResponse
	if err := c.get(url, "", &commits); err != nil {
		return nil, err
	}
	return commits, nil
}

// GetUserEvents fetches the user's recent public events (up to 30).
func (c *Client) GetUserEvents(login string) ([]Event, error) {
	url := fmt.Sprintf("%s/users/%s/events/public?per_page=30", apiBase, login)
	var events []Event
	if err := c.get(url, "", &events); err != nil {
		return nil, err
	}
	return events, nil
}

// GetUserEventData returns the first real email found in push event payloads,
// plus all repo full-names (owner/repo) seen in PushEvents.
func (c *Client) GetUserEventData(login string) (email string, repos []string, err error) {
	events, err := c.GetUserEvents(login)
	if err != nil {
		return "", nil, err
	}
	seen := make(map[string]bool)
	for _, ev := range events {
		if ev.Type != "PushEvent" {
			continue
		}
		// Collect the repo name (deduplicated).
		if ev.Repo.Name != "" && !seen[ev.Repo.Name] {
			seen[ev.Repo.Name] = true
			repos = append(repos, ev.Repo.Name)
		}
		// Extract email from payload commits (first real one wins).
		if email == "" {
			var payload PushPayload
			if jsonErr := json.Unmarshal(ev.Payload, &payload); jsonErr == nil {
				for _, cm := range payload.Commits {
					if IsRealEmail(cm.Author.Email) {
						email = cm.Author.Email
						break
					}
				}
			}
		}
	}
	return email, repos, nil
}

// SearchCommitsByAuthor uses the GitHub commit search API to find commits authored
// by login across all public repos on GitHub. Requires a token and uses a separate
// rate limit (30 req/min with token, 10 req/min without).
func (c *Client) SearchCommitsByAuthor(login string) (string, error) {
	url := fmt.Sprintf("%s/search/commits?q=author:%s&sort=author-date&order=desc&per_page=5", apiBase, login)
	var result struct {
		Items []struct {
			Commit struct {
				Author struct {
					Email string `json:"email"`
				} `json:"author"`
			} `json:"commit"`
		} `json:"items"`
	}
	// This endpoint requires the cloak-preview header.
	if err := c.get(url, "application/vnd.github.cloak-preview+json", &result); err != nil {
		return "", err
	}
	for _, item := range result.Items {
		email := item.Commit.Author.Email
		if IsRealEmail(email) {
			return email, nil
		}
	}
	return "", nil
}

// graphql executes a GraphQL POST against the GitHub API, decoding the response Data into v.
func (c *Client) graphql(query string, v interface{}) error {
	c.throttle()

	attempts := len(c.tokens)
	if attempts == 0 {
		attempts = 1
	}

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return err
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		token := c.currentToken()

		req, err := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode == 403 || resp.StatusCode == 429 {
			resp.Body.Close()
			if len(c.tokens) > 1 {
				c.rotateToken()
				lastErr = fmt.Errorf("rate limited (%d)", resp.StatusCode)
				continue
			}
			return fmt.Errorf("rate limited (%d)", resp.StatusCode)
		}

		respBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}

		var envelope struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(respBytes, &envelope); err != nil {
			return err
		}
		if len(envelope.Errors) > 0 {
			return fmt.Errorf("graphql error: %s", envelope.Errors[0].Message)
		}
		return json.Unmarshal(envelope.Data, v)
	}
	return fmt.Errorf("all %d token(s) rate-limited: %w", len(c.tokens), lastErr)
}

// gqlUser is a local struct used to deserialize GraphQL user responses.
type gqlUser struct {
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Company    string `json:"company"`
	Location   string `json:"location"`
	Bio        string `json:"bio"`
	Followers  struct {
		TotalCount int `json:"totalCount"`
	} `json:"followers"`
	Repositories struct {
		TotalCount int `json:"totalCount"`
	} `json:"repositories"`
	URL string `json:"url"`
}

// GetUsersBatch fetches up to len(logins) user profiles using GraphQL aliases
// in chunks of 20 per request. Returns a map of login → *User.
// Missing/suspended users are silently omitted from the map.
// onProgress is called after each chunk with the count fetched so far.
func (c *Client) GetUsersBatch(logins []string, onProgress func(int)) map[string]*User {
	result := make(map[string]*User)
	const chunkSize = 20

	fetched := 0
	for start := 0; start < len(logins); start += chunkSize {
		end := start + chunkSize
		if end > len(logins) {
			end = len(logins)
		}
		chunk := logins[start:end]

		// Build the GraphQL query with aliases u0, u1, ...
		var sb strings.Builder
		sb.WriteString("query {\n")
		for i, login := range chunk {
			fmt.Fprintf(&sb, "  u%d: user(login: %q) { login databaseId name email company location bio followers { totalCount } repositories { totalCount } url }\n", i, login)
		}
		sb.WriteString("}")

		var data map[string]json.RawMessage
		if err := c.graphql(sb.String(), &data); err != nil {
			// Silently skip this chunk on error.
			if onProgress != nil {
				fetched += len(chunk)
				onProgress(fetched)
			}
			continue
		}

		for _, raw := range data {
			if string(raw) == "null" {
				continue
			}
			var gu gqlUser
			if err := json.Unmarshal(raw, &gu); err != nil {
				continue
			}
			u := &User{
				Login:       gu.Login,
				ID:          gu.DatabaseID,
				Name:        gu.Name,
				Email:       gu.Email,
				Company:     gu.Company,
				Location:    gu.Location,
				Bio:         gu.Bio,
				Followers:   gu.Followers.TotalCount,
				PublicRepos: gu.Repositories.TotalCount,
				HTMLURL:     gu.URL,
			}
			result[gu.Login] = u
		}

		fetched += len(chunk)
		if onProgress != nil {
			onProgress(fetched)
		}
	}
	return result
}

// GetUserOrgs returns the logins of the user's public organizations (up to 10).
func (c *Client) GetUserOrgs(login string) ([]string, error) {
	url := fmt.Sprintf("%s/users/%s/orgs?per_page=10", apiBase, login)
	var orgs []struct {
		Login string `json:"login"`
	}
	if err := c.get(url, "", &orgs); err != nil {
		return nil, err
	}
	logins := make([]string, len(orgs))
	for i, o := range orgs {
		logins[i] = o.Login
	}
	return logins, nil
}

// GetOrgRepos returns public repos for the org sorted by most recently pushed (up to limit).
func (c *Client) GetOrgRepos(org string, limit int) ([]Repo, error) {
	url := fmt.Sprintf("%s/orgs/%s/repos?type=public&sort=pushed&direction=desc&per_page=%d", apiBase, org, limit)
	var repos []Repo
	if err := c.get(url, "", &repos); err != nil {
		return nil, err
	}
	return repos, nil
}

// IsNoReplyEmail returns true for GitHub-generated no-reply addresses.
func IsNoReplyEmail(email string) bool {
	return strings.HasSuffix(email, "@users.noreply.github.com") ||
		strings.HasSuffix(email, "@noreply.github.com") ||
		strings.Contains(email, "noreply")
}

// IsRealEmail returns true if the email looks like a genuine personal address.
func IsRealEmail(email string) bool {
	return email != "" &&
		strings.Contains(email, "@") &&
		!IsNoReplyEmail(email)
}

// NoReplyEmail returns the deterministic GitHub no-reply address for a user.
func NoReplyEmail(id int64, login string) string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
}
