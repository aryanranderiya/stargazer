// Package pusher imports scraped stargazer contacts into the GAIA email
// platform via its HTTP import API (POST /api/contacts/import). It validates
// email addresses client-side (the platform rejects an entire batch if any
// address is malformed) and, by default, drops GitHub no-reply addresses which
// are not deliverable.
package pusher

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const noreplySuffix = "@users.noreply.github.com"

// validEmailRe mirrors the email platform's Zod email validator (minus its two
// negative-lookahead guards, which RE2 can't express — those are checked in
// validEmail). The old loose check let through addresses the platform rejects,
// and a single rejected address 400s the ENTIRE import batch. Commit author
// emails come straight from git metadata and are often junk (you@example.com,
// user@localhost, name@host.(none)), so this matters now that we resolve them.
var validEmailRe = regexp.MustCompile(`^[A-Za-z0-9_'+\-.]*[A-Za-z0-9_+-]@([A-Za-z0-9][A-Za-z0-9\-]*\.)+[A-Za-z]{2,}$`)

// validEmail reports whether email will be accepted by the platform's importer.
func validEmail(email string) bool {
	if email == "" || strings.HasPrefix(email, ".") || strings.Contains(email, "..") {
		return false
	}
	if !validEmailRe.MatchString(email) {
		return false
	}
	// Reject git's universal placeholder / non-routable identities — valid format
	// but never a real person.
	switch {
	case strings.HasSuffix(email, "@example.com"),
		strings.HasSuffix(email, "@example.org"),
		strings.HasSuffix(email, "@example.net"),
		strings.HasSuffix(email, "@localhost"),
		strings.HasSuffix(email, ".local"),
		strings.HasSuffix(email, ".invalid"):
		return false
	}
	return true
}

// Client imports contacts into the email platform.
type Client struct {
	BaseURL     string // e.g. http://api:3020
	Secret      string // platform API_SECRET (Bearer token)
	ListID      string // target list UUID (the "scraped" list)
	Source      string // contact source tag (default "stargazer")
	PushNoreply bool   // include *@users.noreply.github.com addresses
	BatchSize   int    // contacts per request (default 500)
	HTTP        *http.Client

	// OnContact, if set, is called once per CSV row with the per-row outcome —
	// used to feed a live "recent contacts" view.
	OnContact func(Record)
}

// Record is the per-row outcome reported via OnContact.
type Record struct {
	Login       string `json:"login"`
	Email       string `json:"email"`
	EmailSource string `json:"emailSource"` // profile | commit | events | search | noreply
	Repo        string `json:"repo"`
	Status      string `json:"status"` // sent | noreply | invalid | duplicate
}

func (c *Client) emit(r Record) {
	if c.OnContact != nil {
		c.OnContact(r)
	}
}

// Contact is the import payload shape expected by the platform.
type Contact struct {
	Email      string         `json:"email"`
	FirstName  string         `json:"firstName,omitempty"`
	LastName   string         `json:"lastName,omitempty"`
	Company    string         `json:"company,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Source     string         `json:"source,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
}

// Stats summarises one PushCSV call.
type Stats struct {
	Rows       int `json:"rows"`       // data rows read from the CSV
	Invalid    int `json:"invalid"`    // dropped: blank/malformed email
	Noreply    int `json:"noreply"`    // dropped: github no-reply (when disabled)
	Duplicate  int `json:"duplicate"`  // dropped: duplicate email within this push
	Sent       int `json:"sent"`       // contacts sent to the API
	Imported   int `json:"imported"`   // newly inserted (reported by API)
	Skipped    int `json:"skipped"`    // already existed (reported by API)
	Suppressed int `json:"suppressed"` // on the suppression list (reported by API)
	Batches    int `json:"batches"`
}

type importRequest struct {
	Contacts []Contact `json:"contacts"`
	ListID   string    `json:"listId,omitempty"`
}

type importResponse struct {
	Imported   int `json:"imported"`
	Skipped    int `json:"skipped"`
	Suppressed int `json:"suppressed"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

func (c *Client) batchSize() int {
	if c.BatchSize > 0 {
		return c.BatchSize
	}
	return 500
}

func (c *Client) source() string {
	if c.Source != "" {
		return c.Source
	}
	return "stargazer"
}

// Ping verifies the import API is reachable and the token accepted, by issuing
// a no-op import (empty contacts list).
func (c *Client) Ping() error {
	_, err := c.postImport([]Contact{})
	return err
}

// ListCount returns the current contact count of the target list (the true size
// of the Scraped audience, including contacts added outside this scraper).
func (c *Client) ListCount() (int, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/api/lists"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var lists []struct {
		ID           string `json:"id"`
		ContactCount int    `json:"contactCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lists); err != nil {
		return 0, err
	}
	for _, l := range lists {
		if l.ID == c.ListID {
			return l.ContactCount, nil
		}
	}
	return 0, nil
}

// PushCSV reads a stargazer CSV file and imports its contacts into the list.
// sourceRepo (e.g. "facebook/react") is recorded on each contact's attributes
// and tags for provenance.
func (c *Client) PushCSV(path, sourceRepo string) (Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stats{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolerate ragged rows
	r.LazyQuotes = true
	header, err := r.Read()
	if err == io.EOF {
		return Stats{}, nil
	}
	if err != nil {
		return Stats{}, fmt.Errorf("reading header of %s: %w", path, err)
	}
	idx := indexColumns(header)

	// Per-repo source so contacts are attributable + filterable by repo in the
	// email platform (e.g. "stargazer:facebook/react").
	contactSource := c.source()
	if sourceRepo != "" {
		contactSource = contactSource + ":" + sourceRepo
	}

	var stats Stats
	seen := make(map[string]struct{})
	batch := make([]Contact, 0, c.batchSize())

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		resp, dropped, err := c.importResilient(batch)
		if err != nil {
			return err
		}
		stats.Batches++
		stats.Sent += len(batch) - dropped
		stats.Invalid += dropped
		stats.Imported += resp.Imported
		stats.Skipped += resp.Skipped
		stats.Suppressed += resp.Suppressed
		batch = batch[:0]
		return nil
	}

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed CSV rows rather than aborting
		}
		stats.Rows++
		get := func(col string) string {
			i, ok := idx[col]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}

		email := strings.ToLower(get("email"))
		login, esrc := get("login"), get("email_source")
		if !validEmail(email) {
			stats.Invalid++
			c.emit(Record{Login: login, Email: get("email"), EmailSource: esrc, Repo: sourceRepo, Status: "invalid"})
			continue
		}
		if !c.PushNoreply && strings.HasSuffix(email, noreplySuffix) {
			stats.Noreply++
			c.emit(Record{Login: login, Email: email, EmailSource: esrc, Repo: sourceRepo, Status: "noreply"})
			continue
		}
		if _, dup := seen[email]; dup {
			stats.Duplicate++
			c.emit(Record{Login: login, Email: email, EmailSource: esrc, Repo: sourceRepo, Status: "duplicate"})
			continue
		}
		seen[email] = struct{}{}
		c.emit(Record{Login: login, Email: email, EmailSource: esrc, Repo: sourceRepo, Status: "sent"})

		first, last := splitName(get("name"))
		attrs := map[string]any{}
		addIfSet(attrs, "github_login", get("login"))
		addIfSet(attrs, "email_source", get("email_source"))
		addIfSet(attrs, "profile_url", get("profile_url"))
		addIfSet(attrs, "location", get("location"))
		addIfSet(attrs, "bio", get("bio"))
		addIfSet(attrs, "starred_at", get("starred_at"))
		addInt(attrs, "followers", get("followers"))
		addInt(attrs, "public_repos", get("public_repos"))
		if sourceRepo != "" {
			attrs["source_repo"] = sourceRepo
		}

		tags := []string{"stargazer"}
		if sourceRepo != "" {
			tags = append(tags, "repo:"+sourceRepo)
		}

		batch = append(batch, Contact{
			Email:      email,
			FirstName:  first,
			LastName:   last,
			Company:    get("company"),
			Attributes: attrs,
			Source:     contactSource,
			Tags:       tags,
		})
		if len(batch) >= c.batchSize() {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	return stats, nil
}

// importResilient imports a batch, isolating any contacts the platform rejects
// for validation (HTTP 400) by bisecting the batch, so one malformed address
// can't fail the whole import. Returns the summed response and the number of
// contacts dropped. Non-validation errors (network, auth, 5xx) propagate so the
// run surfaces a real failure. With validEmail mirroring the platform's rules
// this rarely triggers — it's a safety net for any rule we don't replicate.
func (c *Client) importResilient(contacts []Contact) (importResponse, int, error) {
	resp, err := c.postImport(contacts)
	if err == nil {
		return resp, 0, nil
	}
	if !strings.Contains(err.Error(), "400") {
		return importResponse{}, 0, err
	}
	if len(contacts) <= 1 {
		return importResponse{}, len(contacts), nil // the offending contact — drop it
	}
	mid := len(contacts) / 2
	r1, d1, e1 := c.importResilient(contacts[:mid])
	if e1 != nil {
		return importResponse{}, 0, e1
	}
	r2, d2, e2 := c.importResilient(contacts[mid:])
	if e2 != nil {
		return importResponse{}, 0, e2
	}
	return importResponse{
		Imported:   r1.Imported + r2.Imported,
		Skipped:    r1.Skipped + r2.Skipped,
		Suppressed: r1.Suppressed + r2.Suppressed,
	}, d1 + d2, nil
}

func (c *Client) postImport(contacts []Contact) (importResponse, error) {
	if contacts == nil {
		contacts = []Contact{} // marshal as [] not null (zod rejects null)
	}
	body, err := json.Marshal(importRequest{Contacts: contacts, ListID: c.ListID})
	if err != nil {
		return importResponse{}, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/api/contacts/import"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return importResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return importResponse{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return importResponse{}, fmt.Errorf("import API %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	var out importResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return importResponse{}, fmt.Errorf("decoding import response: %w (body=%s)", err, string(rb))
	}
	return out, nil
}

func indexColumns(header []string) map[string]int {
	m := make(map[string]int, len(header))
	for i, h := range header {
		m[strings.TrimSpace(strings.ToLower(h))] = i
	}
	return m
}

func splitName(name string) (first, last string) {
	parts := strings.Fields(name)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

func addIfSet(m map[string]any, k, v string) {
	if strings.TrimSpace(v) != "" {
		m[k] = v
	}
}

func addInt(m map[string]any, k, v string) {
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil {
		m[k] = n
	}
}
