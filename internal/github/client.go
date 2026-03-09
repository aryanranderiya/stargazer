package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const apiBase = "https://api.github.com"

// Client is a minimal GitHub REST API client.
type Client struct {
	tokens     []string
	tokenIdx   atomic.Int64
	httpClient *http.Client
	delay      time.Duration
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
	return &Client{
		tokens:     tokens,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		delay:      delay,
	}
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
	if c.delay > 0 {
		time.Sleep(c.delay)
	}

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
