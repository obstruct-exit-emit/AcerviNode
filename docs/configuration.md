# Configuration

AcerviNode reads `config.yaml` from its working directory (override the path with
`ACERVINODE_CONFIG`). Every key can also be set via an `ACERVINODE_*` environment
variable, which takes precedence over the file — useful for containers and systemd
units without a mounted config file.

| Key | Env var | Default | Purpose |
|---|---|---|---|
| `port` | `ACERVINODE_PORT` | `7846` | HTTP port for both compat shims and the native API. Editable via the settings API/UI, but **requires a restart** — AcerviNode doesn't rebind its listener live |
| `data_dir` | `ACERVINODE_DATA_DIR` | `./data` | Where the SQLite database file lives. Editable via the settings API/UI, but **requires a restart** — the database connection isn't reopened live |
| `api_key` | `ACERVINODE_API_KEY` | *(generated on first run)* | Key required by the native `/api/v1` endpoints, the qBittorrent shim's login password, and the SABnzbd shim's `apikey` param. Viewable and regeneratable live (no restart) via the web UI's Settings tab, or `GET`/`POST /api/v1/settings/general` and `/api/v1/settings/api-key/regenerate` — see [API](api.md) |
| `log_level` | `ACERVINODE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. Editable live (no restart) via `PUT /api/v1/settings/general` or the web UI — applies immediately via a `slog.LevelVar` |
| `download_dir` | `ACERVINODE_DOWNLOAD_DIR` | `./downloads` | Fallback destination for Completed Download Handling when the *arr app didn't supply its own `save_path`. Editable live (no restart) — `internal/importer.SetConfig` |
| `import_interval_seconds` | `ACERVINODE_IMPORT_INTERVAL_SECONDS` | `10` | How often `internal/importer` ticks: proactively refreshes every tracked download's status from its provider (not just when an *arr app happens to poll — see [Providers](providers.md#proactive-status-refresh)) and checks for provider-completed downloads to fetch to local disk; also the base of its retry backoff (attempt *N* waits ~`import_interval_seconds`×2^*N*, capped at 1 hour). Editable live (no restart) — the running ticker resets to the new interval immediately rather than waiting out the old one |
| `import_max_retries` | `ACERVINODE_IMPORT_MAX_RETRIES` | `5` | How many failed fetch attempts a download gets before `internal/importer` gives up and moves it to `error` instead of retrying again. Editable live (no restart) |
| `max_concurrent_downloads` | `ACERVINODE_MAX_CONCURRENT_DOWNLOADS` | `3` | How many `provider_completed` downloads `internal/importer` fetches to local disk at once — previously always strictly one at a time, with no way to change it. Editable live (no restart); a value below 1 is clamped up to 1 rather than rejected |
| `import_fetch_timeout_seconds` | `ACERVINODE_IMPORT_FETCH_TIMEOUT_SECONDS` | `600` | Deadline for a single file's whole fetch (not just connecting) before `internal/importer` gives up on it — same retry/backoff as any other fetch failure applies. Raise it if large files on a slow connection are failing partway through. Editable live (no restart); in-flight fetches keep whatever deadline they already started with |
| `providers.torbox.api_key` | `ACERVINODE_PROVIDERS_TORBOX_API_KEY` | *(none — required to enable TorBox)* | Bearer token used for every TorBox API call. Can also be set (or changed) without a restart via the web UI's Settings tab, or `PUT /api/v1/settings/providers/torbox` — see [API](api.md) and [Providers](providers.md#live-settings). `POST /api/v1/settings/providers/torbox/test` makes one real, live call to confirm the key actually works |
| `category_paths.<category>` | *(none — set via API/UI, not env)* | *(none)* | Per-category override for `download_dir`, e.g. to route one category to a different disk/mount — see [Categories and save paths](#categories-and-save-paths) below. Editable live (no restart) via `PUT /api/v1/settings/categories/path` or the web UI's Settings tab |

`download_dir`, `log_level`, `import_interval_seconds`, `import_max_retries`,
`max_concurrent_downloads`, and `import_fetch_timeout_seconds` can all be
changed together in one call via `PUT /api/v1/settings/general` (alongside
`port`/`data_dir`, which persist but need a restart) — see [API](api.md).
`api_key`, `providers.torbox.api_key`, and `category_paths` each have their
own dedicated endpoint instead (regenerate, provider settings, and category
paths respectively).

### Categories

Both compat shims track category names *arr apps declare (purely so
`/api/v2/torrents/categories`/`mode=get_config` have something to report back —
AcerviNode never interprets a category itself). `GET /api/v1/settings/categories`
and `POST /api/v1/settings/categories` (or the web UI's Settings tab) let you view
them and pre-declare one manually, e.g. to populate the "Add Download" form's
category field before Sonarr/Radarr has ever added anything.

## Example

```yaml
port: 7846
data_dir: /var/lib/acervinode
download_dir: /var/lib/acervinode/downloads
log_level: info
providers:
  torbox:
    api_key: "your-torbox-api-key"
```

## Provider config shape

`providers` is a map keyed by provider name (`torbox` today; `realdebrid` and
others land here as they're implemented — see [Providers](providers.md) and the
[roadmap](../ROADMAP.md)). Both compat shims are always mounted, whether or not a
provider is configured yet — see [Providers](providers.md#live-settings) for why
that's what makes setting a key through the web UI (rather than only at startup)
possible at all.

## Regenerating the API key

`POST /api/v1/settings/api-key/regenerate` (the Settings tab's "Regenerate API
key" button) replaces `api_key` with a fresh random one and applies it
immediately — every authenticated route, both compat shims included, stops
accepting the old key right away. This means every *arr app configured against
AcerviNode's qBittorrent/SABnzbd shims needs its download client password/API
key updated to match, and any other browser tab/session using the old key gets
logged out. The web UI's own session updates itself automatically since it's
the one that triggered the change; nothing else does.

## Categories and save paths

*arr apps set a category on every add (`tv-sonarr`, `radarr`, ...) and, for the
qBittorrent shim, generally rely on the category's own configured path rather than
sending an explicit `save_path`. AcerviNode stores whatever category and save path
the calling app does send, and Completed Download Handling
([Providers](providers.md#completed-download-handling)) writes fetched files there
when one was supplied — an explicit `save_path` from the *arr app always wins.

When no `save_path` was supplied, AcerviNode falls back to `download_dir`, organized
as `<download_dir>/<category>/<name>/` — unless that category has its own override
configured via `category_paths` (`PUT /api/v1/settings/categories/path`, or the web
UI's Settings tab under Categories → "Save path overrides"), in which case files land
directly under `<override>/<name>/` instead. Useful for routing one category to a
different disk or mount than the rest — e.g. movies to a large secondary drive while
everything else stays on `download_dir`. `GET /api/v1/settings/categories`'s `paths`
field reports the current overrides.
