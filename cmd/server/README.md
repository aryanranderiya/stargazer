# stargazer-server

A long-running HTTP service that wraps the stargazer scraper. A built-in daily
scheduler pops repos from a persistent queue, scrapes their GitHub stargazers
(reusing `internal/scraper`), and pushes the enriched contacts into the GAIA
email platform via its import API. The interactive TUI (`go run .`) is
unchanged — this is an additive `cmd/`.

## Build / run

```sh
go build ./cmd/server          # local binary
docker build -t stargazer-server .   # container (multi-stage, ~15MB alpine)
```

The service stores all mutable state under `DATA_DIR` (default `/data`):

| File | Purpose |
|------|---------|
| `repos.txt` | the repo queue (one `owner/repo` per line) |
| `state.json` | persisted cursor + cycle count |
| `user_cache.json` | 30-day GitHub profile cache (shared with the TUI format) |
| `out/` | per-run scratch CSVs (deleted after each run) |

## Configuration (environment)

Required: `EMAIL_API_SECRET`, `EMAIL_LIST_ID`. See [`deploy/.env.example`](../../deploy/.env.example)
for the full list (GitHub tokens, scrape tuning, schedule, etc.).

Tokens come from `STARGAZER_GITHUB_TOKENS` (comma-separated); if unset, the
service falls back to `~/.config/stargazer/credentials.json` (the TUI's store).

## Dashboard

`GET /` serves a self-contained, Apple-style dashboard (no build step, embedded
via `go:embed`) showing cumulative emails ingested, per-metric totals, live
queue progress, a run history table, and **pace controls** — pause/resume and
live-editable repos-per-day, max-stars, delay, and concurrency (persisted to
`settings.json`, applied on the next run).

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | the dashboard UI |
| `GET` | `/health` | liveness probe |
| `GET` | `/api/status` | running state, queue position, settings, cumulative totals, last run |
| `GET` | `/api/history` | recent run reports (newest first) |
| `GET`/`POST` | `/api/settings` | read / partially update runtime settings (pause, pace) |
| `GET` | `/api/queue` | queue snapshot (size, cursor, next repo, parse warnings) |
| `POST` | `/api/scrape` | trigger a run now. Empty body = next from queue; `{"repo":"owner/repo"}` or `{"repos":[...]}` = those repos (cursor untouched) |

Runs are serialised: a second trigger while one is in flight returns `409`.
While paused, the daily scheduler skips runs (manual `/api/scrape` still works).

## Email mapping & deliverability

Each CSV row becomes an import contact:

- `email` (lowercased, format-validated — invalid rows dropped so the platform's
  batch validation never rejects the request)
- `name` → `firstName` / `lastName` (split on first space)
- `company` → `company`
- everything else (`login`, `email_source`, `location`, `bio`, `followers`,
  `public_repos`, `starred_at`, `profile_url`, `source_repo`) → `attributes`
- `source = "stargazer"`, `tags = ["stargazer", "repo:owner/name"]`

GitHub no-reply addresses (`*@users.noreply.github.com`) are **dropped by
default** since they don't receive mail; set `PUSH_NOREPLY=true` to keep them.
The import endpoint upserts by email and dedupes against the suppression list,
so re-scraping the same repo never creates duplicates.

## Deploy

See [`deploy/docker-compose.yml`](../../deploy/docker-compose.yml) — it builds the
image and joins the email platform's Docker network so the API is reachable at
`http://api:3020`.
