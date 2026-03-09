# stargazer

![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-blue)
![Terminal UI](https://img.shields.io/badge/UI-BubbleTea-ff69b4)

A terminal UI tool that scrapes GitHub stargazers from any repository, enriches each user with their email address, and exports everything to a CSV file.

> **Minimal by design** — the single self-contained binary is ~7 MB (stripped), with zero runtime dependencies. No daemon, no config file required, no install step beyond dropping the binary in your PATH.

---

## How it works

1. Fetches every stargazer from the target repo via the GitHub API
2. For each user, resolves an email through a three-step fallback chain:
   - **profile** — public email on their GitHub profile
   - **commit** — git author email from commits on their public non-fork repos
   - **noreply** — GitHub's deterministic address: `{id}+{login}@users.noreply.github.com`
3. Writes results to CSV incrementally, so partial results are saved even if the run is interrupted

---

## Install

**From source** (requires Go 1.21+):

```sh
git clone https://github.com/your-org/stargazer
cd stargazer
go install .
```

This puts `stargazer` in your `$GOPATH/bin`.

To build a minimal stripped binary (~7 MB):

```sh
go build -ldflags="-s -w" -o stargazer .
```

**Or just run it directly:**

```sh
go run .
```

---

## Usage

```sh
stargazer
```

A full-screen TUI opens with three screens:

### 1. Config form

Navigate with **Tab / Shift+Tab** or **↑ / ↓**. Press **Enter** to advance, or **Enter on the last field** to start scraping. **Ctrl+C** quits at any time.

| Field | Default | Description |
|---|---|---|
| Repository | `facebook/react` | Target repo — paste a full GitHub URL or `owner/repo` |
| GitHub Token | _(saved)_ | Personal Access Token (strongly recommended) |
| Output CSV Path | `stars/<owner>/<repo>.csv` | Written incrementally; directory is created automatically |
| Concurrency | `5` | Parallel workers (1–20) |
| Max Repos per User | `3` | Own repos to scan for a commit email (1–10) |
| Max Fork Repos | `0` | Forked repos to scan (0 = none) |
| Max Stars | `0` | Stargazers to fetch; `0` = all |
| Request Delay (ms) | `100` | Pause between API calls |
| Use Search API | off | Use GitHub search API as an additional email source |

> **Tip:** You can paste a full URL like `https://github.com/torvalds/linux` into the Repository field — stargazer will strip it down to `torvalds/linux` automatically.

### 2. Progress screen

Live progress bar, current user being processed, and a rolling log of recent results. Press **q** or **Ctrl+C** to abort.

### 3. Done screen

Shows total count, elapsed time, and the output CSV path. Press **Enter**, **q**, or **Ctrl+C** to exit.

---

## GitHub Token

| | Requests/hour |
|---|---|
| No token | 60 |
| Token (no scopes needed) | 5,000 |

Without a token, a repo with more than ~20 stars will hit the rate limit quickly.

**Create a token:** GitHub → Settings → Developer settings → Personal access tokens → Tokens (classic) → Generate new token — no scopes need to be checked for public repos.

Tokens are saved locally and pre-filled on next run.

---

## Output CSV

| Column | Description |
|---|---|
| `login` | GitHub username |
| `name` | Display name |
| `email` | Best email found |
| `email_source` | `profile`, `commit`, or `noreply` |
| `company` | Company field |
| `location` | Location field |
| `bio` | Bio field |
| `followers` | Follower count |
| `public_repos` | Number of public repositories |
| `starred_at` | ISO 8601 timestamp when they starred |
| `profile_url` | Link to their GitHub profile |

---

## Performance

| Scenario | API requests per user |
|---|---|
| Profile email found | ~2 |
| Commit scan (3 repos) | ~8 |

With a token and default settings (5 workers, 100 ms delay, 3 repos/user), a repo with 1,000 stars takes roughly **5–15 minutes** depending on how many users have a public profile email.

To go faster: set delay to `0`, concurrency to `10–20`, max repos to `1`.

---

## Project structure

```
stargazer/
├── main.go                    # Entry point
├── internal/
│   ├── credentials/           # Token persistence
│   ├── github/
│   │   └── client.go          # GitHub REST API client
│   └── scraper/
│       └── scraper.go         # Worker pool, email resolution, CSV writer
└── ui/
    └── model.go               # BubbleTea model (form, progress, done screens)
```
