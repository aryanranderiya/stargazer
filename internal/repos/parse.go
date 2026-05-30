// Package repos parses GitHub repository references ("owner/repo" or
// github.com URLs) into structured targets. It is shared between the
// interactive TUI and the headless HTTP server so both accept identical input.
package repos

import (
	"strings"

	"stargazer/internal/scraper"
)

// githubPrefixes are stripped from a reference so full URLs are accepted.
var githubPrefixes = []string{
	"https://github.com/",
	"http://github.com/",
	"github.com/",
}

// ParseOne normalises a single repo reference to the canonical "owner/repo"
// form. The returned warning is non-empty when the reference is malformed.
func ParseOne(raw string) (normalised, warn string) {
	s := strings.TrimSpace(raw)
	for _, prefix := range githubPrefixes {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return s, ""
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return s, "Format must be owner/repo (e.g. torvalds/linux)"
	}
	return parts[0] + "/" + parts[1], ""
}

// ParseTargets parses comma-separated repo references into RepoTargets,
// failing on the first malformed entry. Used for interactive input where the
// user should be told immediately about a typo.
func ParseTargets(raw string) ([]scraper.RepoTarget, string) {
	var targets []scraper.RepoTarget
	for _, seg := range strings.Split(raw, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		n, warn := ParseOne(seg)
		if warn != "" {
			return nil, warn
		}
		owner, repo, _ := strings.Cut(n, "/")
		targets = append(targets, scraper.RepoTarget{Owner: owner, Repo: repo})
	}
	if len(targets) == 0 {
		return nil, "At least one repository is required"
	}
	return targets, ""
}

// ParseList leniently parses a newline- and/or comma-separated list of repo
// references (e.g. a queue file of hundreds of repos). Blank lines, '#'
// comments and inline trailing comments are ignored, duplicates are collapsed,
// and malformed entries are skipped and reported via warnings rather than
// aborting the whole list.
func ParseList(raw string) (targets []scraper.RepoTarget, warnings []string) {
	seen := make(map[string]struct{})
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	for _, field := range fields {
		seg := strings.TrimSpace(field)
		// Drop inline comments / trailing tokens after the repo reference.
		if i := strings.IndexAny(seg, " \t#"); i >= 0 {
			seg = strings.TrimSpace(seg[:i])
		}
		if seg == "" {
			continue
		}
		n, warn := ParseOne(seg)
		if warn != "" {
			warnings = append(warnings, seg+": "+warn)
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		owner, repo, _ := strings.Cut(n, "/")
		targets = append(targets, scraper.RepoTarget{Owner: owner, Repo: repo})
	}
	return targets, warnings
}
