package repos

import "testing"

func TestParseOne(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		wantWarn bool
	}{
		{"facebook/react", "facebook/react", false},
		{"  facebook/react  ", "facebook/react", false},
		{"https://github.com/facebook/react", "facebook/react", false},
		{"https://github.com/facebook/react.git", "facebook/react", false},
		{"github.com/facebook/react/", "facebook/react", false},
		{"facebook/react/tree/main", "facebook/react", false}, // extra path trimmed
		{"justowner", "justowner", true},
		{"/react", "/react", true},
	}
	for _, c := range cases {
		got, warn := ParseOne(c.in)
		if got != c.want {
			t.Errorf("ParseOne(%q) = %q, want %q", c.in, got, c.want)
		}
		if (warn != "") != c.wantWarn {
			t.Errorf("ParseOne(%q) warn=%q, wantWarn=%v", c.in, warn, c.wantWarn)
		}
	}
}

func TestParseList(t *testing.T) {
	raw := `
# a comment line
facebook/react
vercel/next.js   # inline comment
https://github.com/sveltejs/svelte

facebook/react
not-valid-entry
denoland/deno, golang/go
`
	targets, warnings := ParseList(raw)
	want := []string{"facebook/react", "vercel/next.js", "sveltejs/svelte", "denoland/deno", "golang/go"}
	if len(targets) != len(want) {
		t.Fatalf("got %d targets %v, want %d %v", len(targets), targets, len(want), want)
	}
	for i, w := range want {
		got := targets[i].Owner + "/" + targets[i].Repo
		if got != w {
			t.Errorf("target[%d] = %q, want %q", i, got, w)
		}
	}
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning for the malformed entry, got %d: %v", len(warnings), warnings)
	}
}
