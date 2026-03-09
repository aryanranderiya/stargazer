# stargazer

A terminal UI tool that scrapes GitHub stargazers from any repository, enriches each user with their email address, and exports everything to CSV.

Built with [BubbleTea](https://github.com/charmbracelet/bubbletea).

---

## Requirements

- Go 1.21+
- A GitHub Personal Access Token (optional but strongly recommended)

---

## Build

```sh
git clone <this-repo>
cd stargazer
go mod tidy      # download dependencies (only needed once)
go build -o stargazer .
```

This produces a single binary: `./stargazer`

---

## Run

```sh
./stargazer
```

Or without building first:

```sh
go run .
```

---

## Usage

The tool opens a full-screen TUI with three screens.

### 1. Config form

Navigate fields with **Tab / Shift+Tab** or **↑ / ↓**.
Press **Enter** to advance to the next field.
Press **Enter on the last field** to start scraping.
Press **Ctrl+C** at any time to quit.

| Field | Default | Description |
|---|---|---|
| Repository | `existence-master/Sentient` | Target repo in `owner/repo` format |
| GitHub Token | _(empty)_ | Personal Access Token — see note below |
| Output CSV Path | `stars/<repo>-stars.csv` | Path to write results; directory is created automatically |
| Concurrency | `5` | Number of parallel workers (1–20) |
| Max Repos per User | `3` | How many of the user's own repos to scan for a commit email (1–10) |
| Max Stars | `0` | How many stargazers to fetch; `0` means all |
| Request Delay (ms) | `100` | Pause between API calls in milliseconds |

### 2. Progress screen

Shows a live progress bar, current user being processed, and a rolling log of recent results. Press **q** or **Ctrl+C** to abort.

### 3. Done screen

Displays the total count and the path to the CSV file. Press **Enter**, **q**, or **Ctrl+C** to exit.

---

## GitHub Token

Without a token the GitHub API allows only **60 requests per hour**, which is not enough for any repo with more than ~20 stars.

A token with no special scopes (just public read access) gives you **5,000 requests per hour**.

**Create one:**
GitHub → Settings → Developer settings → Personal access tokens → Tokens (classic) → Generate new token
No scopes need to be checked for public repositories.

---

## Output CSV

The output file is written incrementally, so partial results are saved even if the run is interrupted.

| Column | Description |
|---|---|
| `login` | GitHub username |
| `name` | Display name |
| `email` | Best email found (see fallback chain below) |
| `email_source` | Where the email came from: `profile`, `commit`, or `noreply` |
| `company` | Company field from GitHub profile |
| `location` | Location field |
| `bio` | Bio field |
| `followers` | Follower count |
| `public_repos` | Number of public repositories |
| `starred_at` | ISO 8601 timestamp when they starred the repo |
| `profile_url` | Link to their GitHub profile |

### Email fallback chain

For each user the tool tries three sources in order:

1. **profile** — the public email on their GitHub profile page
2. **commit** — the git author email found in commits on their own non-fork repos (scans up to *Max Repos per User* repos)
3. **noreply** — GitHub's deterministic no-reply address: `{id}+{login}@users.noreply.github.com`

The `email_source` column tells you which source was used for each row.

---

## Rate limits and performance

| Scenario | Requests per star | Stars before hitting limit (no token) |
|---|---|---|
| Profile email found | ~2 | ~30 |
| Need commit scan (3 repos) | ~8 | ~7 |

With a token and the default settings (100 ms delay, 5 workers, 3 repos per user) a repo with 1,000 stars takes roughly 5–15 minutes depending on how many users have public profile emails.

To go faster:
- Lower the delay to `0`
- Increase concurrency to `10`–`20`
- Reduce Max Repos per User to `1`

---

## Example

Scrape all stars from `torvalds/linux` into `output/linux.csv`, checking up to 2 repos per user:

1. Run `./stargazer`
2. Set **Repository** to `torvalds/linux`
3. Set **Output CSV Path** to `output/linux.csv`
4. Set **Max Repos per User** to `2`
5. Paste your token into **GitHub Token**
6. Press Enter through to the last field and confirm

---

## Project structure

```
stargazer/
├── main.go                    # Entry point – starts the BubbleTea program
├── go.mod / go.sum            # Module dependencies
├── internal/
│   ├── github/
│   │   └── client.go          # GitHub REST API client (stargazers, users, repos, commits)
│   └── scraper/
│       └── scraper.go         # Worker pool, email resolution, CSV writer
└── ui/
    └── model.go               # BubbleTea model – form, progress, and done screens
```
