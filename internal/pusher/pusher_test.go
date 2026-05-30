package pusher

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// scraper CSV header (mirrors internal/scraper.csvHeaders).
var header = []string{
	"login", "name", "email", "email_source",
	"company", "location", "bio",
	"followers", "public_repos", "starred_at", "profile_url",
}

func writeCSV(t *testing.T, rows [][]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "repo.csv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write(header)
	for _, r := range rows {
		_ = w.Write(r)
	}
	w.Flush()
	return path
}

func TestPushCSV(t *testing.T) {
	rows := [][]string{
		{"octocat", "The Octocat", "octo@github.com", "profile", "GitHub", "SF", "hi", "100", "8", "2020-01-01T00:00:00Z", "https://github.com/octocat"},
		{"ghost", "Ghost", "12+ghost@users.noreply.github.com", "noreply", "", "", "", "0", "0", "", "https://github.com/ghost"},
		{"bad", "Bad Row", "not-an-email", "profile", "", "", "", "1", "1", "", "url"},
		{"octoclone", "Octo Clone", "octo@github.com", "commit", "", "", "", "2", "2", "", "url"}, // dup email
		{"cher", "Cher", "cher@example.com", "profile", "", "", "", "5", "5", "", "https://github.com/cher"},
	}
	path := writeCSV(t, rows)

	var received []Contact
	var gotAuth, gotListID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req importRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		gotListID = req.ListID
		received = append(received, req.Contacts...)
		_ = json.NewEncoder(w).Encode(importResponse{Imported: len(req.Contacts)})
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:   srv.URL,
		Secret:    "secret-token",
		ListID:    "list-123",
		BatchSize: 1, // force multiple batches
	}
	stats, err := c.PushCSV(path, "octo/repo")
	if err != nil {
		t.Fatalf("PushCSV: %v", err)
	}

	if stats.Rows != 5 {
		t.Errorf("Rows = %d, want 5", stats.Rows)
	}
	if stats.Noreply != 1 {
		t.Errorf("Noreply = %d, want 1", stats.Noreply)
	}
	if stats.Invalid != 1 {
		t.Errorf("Invalid = %d, want 1", stats.Invalid)
	}
	if stats.Duplicate != 1 {
		t.Errorf("Duplicate = %d, want 1", stats.Duplicate)
	}
	if stats.Sent != 2 || stats.Imported != 2 {
		t.Errorf("Sent=%d Imported=%d, want 2/2", stats.Sent, stats.Imported)
	}
	if stats.Batches != 2 {
		t.Errorf("Batches = %d, want 2 (BatchSize=1, 2 sent)", stats.Batches)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
	if gotListID != "list-123" {
		t.Errorf("listId = %q, want list-123", gotListID)
	}

	// Verify the octocat mapping.
	var octo *Contact
	for i := range received {
		if received[i].Email == "octo@github.com" {
			octo = &received[i]
		}
	}
	if octo == nil {
		t.Fatal("octocat contact not sent")
	}
	if octo.FirstName != "The" || octo.LastName != "Octocat" {
		t.Errorf("name split = %q/%q, want The/Octocat", octo.FirstName, octo.LastName)
	}
	if octo.Company != "GitHub" {
		t.Errorf("company = %q, want GitHub", octo.Company)
	}
	if octo.Source != "stargazer:octo/repo" {
		t.Errorf("source = %q, want stargazer:octo/repo", octo.Source)
	}
	if octo.Attributes["github_login"] != "octocat" {
		t.Errorf("attributes.github_login = %v, want octocat", octo.Attributes["github_login"])
	}
	if octo.Attributes["source_repo"] != "octo/repo" {
		t.Errorf("attributes.source_repo = %v, want octo/repo", octo.Attributes["source_repo"])
	}
	// followers parsed as a number via JSON round-trip (float64).
	if f, ok := octo.Attributes["followers"].(float64); !ok || f != 100 {
		t.Errorf("attributes.followers = %v (%T), want 100", octo.Attributes["followers"], octo.Attributes["followers"])
	}
	if len(octo.Tags) < 2 || octo.Tags[0] != "stargazer" || octo.Tags[1] != "repo:octo/repo" {
		t.Errorf("tags = %v, want [stargazer repo:octo/repo]", octo.Tags)
	}
}

func TestPushNoreplyEnabled(t *testing.T) {
	rows := [][]string{
		{"ghost", "Ghost", "12+ghost@users.noreply.github.com", "noreply", "", "", "", "0", "0", "", ""},
	}
	path := writeCSV(t, rows)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req importRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(importResponse{Imported: len(req.Contacts)})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, ListID: "l", PushNoreply: true}
	stats, err := c.PushCSV(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 || stats.Noreply != 0 {
		t.Errorf("with PushNoreply: Sent=%d Noreply=%d, want 1/0", stats.Sent, stats.Noreply)
	}
}
