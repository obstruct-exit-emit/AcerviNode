# Configuration

AcerviNode reads `config.yaml` from its working directory (override the path with
`ACERVINODE_CONFIG`). Every key can also be set via an `ACERVINODE_*` environment
variable, which takes precedence over the file — useful for containers and systemd
units without a mounted config file.

| Key | Env var | Default | Purpose |
|---|---|---|---|
| `port` | `ACERVINODE_PORT` | `7846` | HTTP port for both compat shims and the native API. Editable via the settings API/UI, but **requires a restart** — AcerviNode doesn't rebind its listener live |
| `data_dir` | `ACERVINODE_DATA_DIR` | `./data` | Where the SQLite database file lives. `PUT /api/v1/settings/general` still accepts it (persisted, **requires a restart** — the database connection isn't reopened live), but the web UI shows it read-only rather than offering an editable field — changing it doesn't move the existing `acervinode.db`, so editing it there would make a restart look like all local history vanished. Move the database file yourself first, then change this via `config.yaml`/the env var |
| `api_key` | `ACERVINODE_API_KEY` | *(generated on first run)* | Key required by the native `/api/v1` endpoints, the qBittorrent shim's login password, and the SABnzbd shim's `apikey` param. Viewable and regeneratable live (no restart) via the web UI's Settings tab, or `GET`/`POST /api/v1/settings/general` and `/api/v1/settings/api-key/regenerate` — see [API](api.md) |
| `log_level` | `ACERVINODE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. Editable live (no restart) via `PUT /api/v1/settings/general` or the web UI — applies immediately via a `slog.LevelVar` |
| `download_dir` | `ACERVINODE_DOWNLOAD_DIR` | `./downloads` | Fallback destination for Completed Download Handling when the *arr app didn't supply its own `save_path`. Editable live (no restart) — `internal/importer.SetConfig` |
| `import_interval_seconds` | `ACERVINODE_IMPORT_INTERVAL_SECONDS` | `10` | How often `internal/importer` ticks: proactively refreshes every tracked download's status from its provider (not just when an *arr app happens to poll — see [Providers](providers.md#proactive-status-refresh)) and checks for provider-completed downloads to fetch to local disk; also the base of its retry backoff (attempt *N* waits ~`import_interval_seconds`×2^*N*, capped at 1 hour). Editable live (no restart) — the running ticker resets to the new interval immediately rather than waiting out the old one |
| `import_max_retries` | `ACERVINODE_IMPORT_MAX_RETRIES` | `5` | How many failed fetch attempts a download gets before `internal/importer` gives up and moves it to `error` instead of retrying again. Editable live (no restart) |
| `max_concurrent_downloads` | `ACERVINODE_MAX_CONCURRENT_DOWNLOADS` | `3` | How many `provider_completed` downloads `internal/importer` fetches to local disk at once — previously always strictly one at a time, with no way to change it. Editable live (no restart), with one caveat worth knowing: a **batch already in flight keeps the limit it started with**. `internal/importer` sizes its semaphore once per tick, and the next tick cannot begin until the current one's fetches finish, so raising the limit while downloads are running takes effect when they drain rather than immediately — the same shape as `import_fetch_timeout_seconds` above. Measured, not assumed: with six downloads queued and three fetching, raising this to five left it at three until the batch completed. Must be at least 1 — `0` or negative is **rejected** (HTTP 400 from the settings API, refusing to start from `config.yaml`), not clamped |
| `import_fetch_timeout_seconds` | `ACERVINODE_IMPORT_FETCH_TIMEOUT_SECONDS` | `600` | An **idle/stall** deadline, not a total-transfer one: `internal/importer` gives up on a file fetch only after this many seconds pass with zero bytes received (covers the connect-and-wait-for-headers phase too, not just the body) — same retry/backoff as any other fetch failure applies. A transfer that's slow overall but never actually stops making progress is unaffected by this however long the whole download takes; only a connection that's actually gone quiet trips it — see [Providers](providers.md#completed-download-handling-internalimporter). Editable live (no restart); in-flight fetches keep whatever deadline they already started with |
| `cleanup_after_days` | `ACERVINODE_CLEANUP_AFTER_DAYS` | `0` (disabled) | Automatically removes a **Managed** download once it's sat in `ready_for_import` (already handed off to Sonarr/Radarr) for at least this many days — local files, the provider-side download, and the row itself all removed. `0` disables cleanup entirely; the only setting here where `0` is a meaningful, valid value rather than an error. Never touches a Manual download — see [Providers](providers.md#retentioncleanup-policy). Editable live (no restart) |
| `cleanup_error_after_days` | `ACERVINODE_CLEANUP_ERROR_AFTER_DAYS` | `0` (disabled) | Automatically removes a download once it's sat in `error` for at least this many days — same removal as `cleanup_after_days` above, but applies to **both** Managed and Manual downloads (an error already means AcerviNode gave up or the provider lost track of it). `0` disables it entirely — see [Providers](providers.md#error-state-cleanup). Editable live (no restart) |
| `managed_add_delete_after_fetch` | `ACERVINODE_MANAGED_ADD_DELETE_AFTER_FETCH` | `true` | Default for the "delete from provider once fetched" choice offered when adding a **Managed** download through AcerviNode's own UI or API. Removes the provider-side copy as soon as the files are on local disk, rather than leaving it until `cleanup_after_days` runs — the provider copy is dead weight against quota once the bytes are local. Local files and the row itself are untouched. Overridable per download at add time. **Never applies to an \*arr app's own adds** — see below. Editable live (no restart) |
| `managed_add_keep_files` | `ACERVINODE_MANAGED_ADD_KEEP_FILES` | `true` | Default for the "keep local files" choice on the same form. Exempts the download from `cleanup_after_days`. That policy removes a Managed download's local copy on the premise that an \*arr app already imported it elsewhere — true of an \*arr grab, false of one added here, where nothing imports it and cleanup would delete the files the operator asked for. Overridable per download. Editable live (no restart) |
| `base32_infohashes` | `ACERVINODE_BASE32_INFOHASHES` | `false` | Accepts the 32-character **base32** spelling of a v1 infohash as a torrent, converting it to hex before it reaches a provider. Some older trackers still hand these out. **Off by default, and the reason matters:** that shape is indistinguishable from any other 32-character base32 string — a two-factor secret, an API key, a share code — so with it on, pasting one of those into the add box is read as a torrent and attempted. The add fails at the provider rather than fetching anything and the string goes nowhere else, but it is still an add nobody meant to make. Turn it on only for a tracker that actually gives you base32 hashes. Enforced server-side in `normalizeMagnet`, so the setting governs the API as well as the web UI. Editable live (no restart) |
| `decode_base64_links` | `ACERVINODE_DECODE_BASE64_LINKS` | `true` | Unwraps a base64-wrapped link pasted into the add form, repeatedly if it was wrapped more than once, and only ever keeps a decode that lands on a magnet, infohash or URL. The transformation is shown with a one-click undo rather than done silently. Switch it off if you would rather nothing you paste is ever rewritten; encoded links then stay as the text they look like, and the field says so rather than appearing to ignore you. **Purely a browser-side convenience** — nothing on the server or at any provider base64-decodes an add either way, so unlike `base32_infohashes` above this governs the web UI only. URL percent-escapes are still unwrapped regardless, since an escape only turns a link back into the link it already was. Editable live (no restart) |
| | | | **Why these never touch \*arr adds:** `added_via` is `arr` for a hand-added Managed download *and* a real \*arr grab, so it cannot tell them apart. The add endpoints record a value only when the caller supplies one, so an \*arr grab leaves both columns `NULL` and behaves exactly as before. The distinction is the stored NULL, not a special case in the cleanup logic |
| `backup_interval_hours` | `ACERVINODE_BACKUP_INTERVAL_HOURS` | `0` (disabled) | How often AcerviNode snapshots its own database to `backup_dir`. **Opt-in** — set a value to enable it. Worth enabling: losing the database loses the configuration, the download history, the categories and every login account at once, and no provider can hand any of that back. It is off by default because a snapshot is a copy of every account and session on disk, which is a decision to make deliberately rather than inherit. `0` disables scheduled backups; manual ones still work. Not range-validated — a negative value simply disables it the same way `0` does. The schedule is timed from the newest snapshot on disk, not from when the process started, so an instance restarted more often than its interval still backs up — while a restart loop still cannot produce more than one snapshot per interval, since each restart sees the one before it. Editable live (no restart) — a changed interval retunes the schedule immediately rather than waiting out the old one |
| `backup_keep` | `ACERVINODE_BACKUP_KEEP` | `7` | How many snapshots to retain; the oldest beyond this are deleted after each **successful** backup, never before one (trimming first would mean a failed backup had also thrown away a good one). `0` or less retains everything, so a misconfigured value can't wipe the lot. Only files AcerviNode itself wrote (`acervinode-<timestamp>.db`) are ever considered — anything else in the directory, including a snapshot you renamed deliberately to keep, is left strictly alone. Editable live (no restart) |
| `backup_dir` | `ACERVINODE_BACKUP_DIR` | `<data_dir>/backups` | Where snapshots are written. Left unset it follows `data_dir`, which is what keeps the packaged systemd unit working unchanged — `ReadWritePaths=/var/lib/acervinode` already covers it. **Setting this to a path outside `ReadWritePaths` makes every backup fail under the packaged unit**, since `ProtectSystem=strict` denies the write; add the path to the unit's `ReadWritePaths` if you move it. Requires a restart |
| `stuck_download_timeout_minutes` | `ACERVINODE_STUCK_DOWNLOAD_TIMEOUT_MINUTES` | `0` (disabled) | Auto-errors a download that's sat `queued`/`downloading` with no genuine change reported by the provider for at least this many minutes — keyed on whether anything actually changed (`updated_at`), not simply how long it's been running, so a large download still steadily making progress is never affected however long the whole thing takes. `0` disables the watchdog entirely — see [Providers](providers.md#stuck-download-watchdog). Editable live (no restart) |
| `min_fetch_file_size_bytes` | `ACERVINODE_MIN_FETCH_FILE_SIZE_BYTES` | `0` (disabled) | Skips any file smaller than this when fetching a download's files to local disk — samples, `.nfo`/`.txt` junk. `0` fetches every file regardless — see [Providers](providers.md#per-file-fetch-filtering). Editable live (no restart) |
| `max_fetch_file_size_bytes` | `ACERVINODE_MAX_FETCH_FILE_SIZE_BYTES` | `0` (disabled) | Skips any file larger than this when fetching a download's files to local disk — e.g. an oversized bonus/extra bundled alongside the main file. `0` fetches every file regardless. Must not be less than `min_fetch_file_size_bytes` when both are set — see [Providers](providers.md#per-file-fetch-filtering). Editable live (no restart) |
| `include_file_regex` / `exclude_file_regex` | `ACERVINODE_INCLUDE_FILE_REGEX` / `ACERVINODE_EXCLUDE_FILE_REGEX` | *(none — disabled)* | Skip a file whose path doesn't match `include_file_regex`, or does match `exclude_file_regex`, when fetching a download's files to local disk — both can be set at once, a file has to satisfy both. Must compile as a valid regular expression or the update is rejected. See [Providers](providers.md#per-file-fetch-filtering). Editable live (no restart) |
| `download_dir_mode` | `ACERVINODE_DOWNLOAD_DIR_MODE` | `0777` | Octal permission mode every directory `internal/importer` creates under `download_dir` gets — world-writable by default so a Managed download's completed-import step (Sonarr/Radarr, likely running as a different user/container) can move or hardlink files out of it — see [Providers](providers.md#directory-permissions). Editable live (no restart), including retroactively for a directory that already exists. Must parse as a valid octal permission string (0000-0777) or the update is rejected |
| `fast_poll_interval_seconds` | `ACERVINODE_FAST_POLL_INTERVAL_SECONDS` | `3` | How often `internal/importer` checks each actively in-flight Managed download individually via a targeted per-ID provider call, independent of `import_interval_seconds`'s own full-account listing — see [Providers](providers.md#proactive-status-refresh). The default was tuned live against a real provider to stay responsive without risking a rate limit; going much lower risks one once several downloads are active at once. Editable live (no restart) — the running ticker resets to the new interval immediately |
| `provider_request_timeout_seconds` | `ACERVINODE_PROVIDER_REQUEST_TIMEOUT_SECONDS` | `30` | Bounds a single call to the debrid provider's own API (list, status, add, delete, account — every one of them) before it's cancelled. A plain **total-request** deadline, not an idle one like `import_fetch_timeout_seconds` above — a provider API response is a small JSON payload, not a multi-gigabyte file, so there's no legitimate "slow but actively trickling for 30+ seconds" case to protect against. Previously fixed at construction with no way to change it at all; found worth exposing live after a real TorBox outage where every account-status call took the full 30s to fail, blocking nothing except the account panel itself but still worth being able to tune. Editable live (no restart) — rebuilds the provider from the current key with the new timeout, same as changing the key itself does |
| `default_provider` | *(none)* | *(the only configured provider)* | Which provider a new download goes to when nothing says otherwise — the native add endpoints without an explicit `provider`, and both compat shims, which have no field in their protocols to carry one. Unset selects the first registered provider, so a single-provider install needs nothing here. A name that isn't configured is ignored rather than rejected: this is persisted, and the provider it names may simply have been removed since |
| `providers.<name>.type` | *(none)* | *(the entry's own name)* | Which provider implementation an entry uses, when that differs from its name. This is what allows **two accounts on the same service**: entries named `torbox` and `torbox-work` can both set `type: torbox` and be configured, defaulted and routed to independently. Omit it for the normal one-account-per-service case |
| `providers.torbox.api_key` | `ACERVINODE_PROVIDERS_TORBOX_API_KEY` | *(none — required to enable TorBox)* | Bearer token used for every TorBox API call. Can also be set (or changed) without a restart via the web UI's Settings tab, or `PUT /api/v1/settings/providers/torbox` — see [API](api.md) and [Providers](providers.md#live-settings). `POST /api/v1/settings/providers/torbox/test` makes one real, live call to confirm the key actually works |
| `providers.<name>.disabled_kinds` | *(none)* | *(nothing disabled — every supported kind is on)* | Turns off kinds this provider would otherwise handle: `torrent`, `usenet`, `webdl`. A disabled kind registers no wrapper at all, so the provider never appears for it — routing, polling and the add endpoints all reuse the same "not registered for this kind" path a provider with no such service already takes, and adds fall through to whichever provider still handles it. Stored as the **disabled** set so absent means everything supported is on, which keeps existing configs working untouched. Useful for splitting kinds between two accounts on one service to keep their rate limits apart, or dropping polling for a kind you never use. Editable live (no restart) from Settings → Provider, or `PUT /api/v1/settings/providers/{name}/kinds` |
| `providers.alldebrid.api_key` | `ACERVINODE_PROVIDERS_ALLDEBRID_API_KEY` | *(none — required to enable AllDebrid)* | API key used for every AllDebrid call. Same live-editable treatment as TorBox's above (web UI Settings, or `PUT /api/v1/settings/providers/alldebrid`), and `POST /api/v1/settings/providers/alldebrid/test` confirms it live. AllDebrid covers **torrents and hoster links only — it has no usenet service**, so it never appears as an option for usenet adds however it's configured; see [Providers](providers.md#alldebrid-internaldebridalldebrid) |
| `category_paths.<category>` | *(none — set via API/UI, not env)* | *(none)* | Per-category override for `download_dir`, e.g. to route one category to a different disk/mount — see [Categories and save paths](#categories-and-save-paths) below. Editable live (no restart) via `PUT /api/v1/settings/categories/path` or the web UI's Settings tab |
| `default_categories_seeded` | *(none — managed by AcerviNode)* | *(set to `true` after the first start)* | Internal marker, written once. On an instance's very first start AcerviNode seeds the well-known \*arr default category names into `category_paths` so Sonarr/Radarr accept them immediately; this flag records that it happened, which is what makes those seeded categories genuinely **editable and deletable** — without it they'd silently reappear on every restart. Deleting the line (or setting it `false`) re-seeds them on the next start, which is the supported way to get them back after deleting some. Don't set it `true` on a fresh instance unless you want no seeding at all |
| `auth.users` | *(none — set via API/UI, not env)* | *(none — first run triggers the setup wizard)* | Login accounts for the web UI, on top of the API key (which keeps working unaffected — Sonarr/Radarr and scripts always use it, never a login session). Login is mandatory for the web UI: an instance with no accounts yet shows the first-run setup wizard instead of the dashboard, not an API-key prompt. Managed via the web UI's Settings → Security, the first-run setup wizard, or `/api/v1/settings/users`/`/api/v1/setup` directly — see [Providers](providers.md#auth-login-accounts-and-roles) for the full design (roles, the protected Default account, session mechanics). Never hand-edit a password hash into this file — there's no supported way to generate one outside the app |
| `tls_enabled` | `ACERVINODE_TLS_ENABLED` | `false` | Starts a second HTTP server on `tls_port`, serving HTTPS with a self-signed certificate auto-generated on first need — the plain-HTTP listener on `port` keeps running completely unchanged either way (dual-listen, never a replacement). Mainly exists so the browser's File System Access API (folder-picker downloads) works when AcerviNode is only reachable over a plain LAN IP, not `localhost` — that API requires a secure context. Editable via the web UI's Settings tab or the first-run wizard, but **requires a restart** — see [Providers](providers.md#tls-self-signed-https) |
| `tls_port` | `ACERVINODE_TLS_PORT` | `8443` | Where the HTTPS listener binds when `tls_enabled`. Must differ from `port`. Requires a restart to take effect, same as `tls_enabled` |
| `tls_cert_file` / `tls_key_file` | `ACERVINODE_TLS_CERT_FILE` / `ACERVINODE_TLS_KEY_FILE` | *(none — auto-generated)* | Optional: point at a real certificate/key pair instead of the auto-generated self-signed one (e.g. one obtained through Tailscale's own cert tooling). Both or neither — set only one and AcerviNode refuses to start. Config/env only, deliberately not an editable Settings UI field, the same treatment `data_dir` gets |

`download_dir`, `log_level`, `import_interval_seconds`, `import_max_retries`,
`max_concurrent_downloads`, `import_fetch_timeout_seconds`,
`cleanup_after_days`, `cleanup_error_after_days`,
`stuck_download_timeout_minutes`, `min_fetch_file_size_bytes`,
`max_fetch_file_size_bytes`, `include_file_regex`, `exclude_file_regex`,
`download_dir_mode`, `fast_poll_interval_seconds`, `backup_interval_hours`,
`backup_keep`, `managed_add_delete_after_fetch`, `managed_add_keep_files`,
`base32_infohashes` and `decode_base64_links` can all be changed
together in one call via
`PUT /api/v1/settings/general` (alongside `port`/`data_dir`/`tls_enabled`/
`tls_port`, which persist but need a restart) — see [API](api.md).
`api_key`, `providers.torbox.api_key`, and `category_paths` each have their
own dedicated endpoint instead (regenerate, provider settings, and category
paths respectively).

### Categories

Both compat shims track category names *arr apps declare (purely so
`/api/v2/torrents/categories`/`mode=get_config` have something to report back —
AcerviNode never interprets a category itself). `GET /api/v1/settings/categories`
reports them, and `POST /api/v1/settings/categories` lets a caller pre-declare
one directly, the same way an *arr app's own createCategory call does.

`PUT /api/v1/settings/categories/path` (the web UI's Settings → Categories
section) also registers the category with both compat shims as a side effect
of setting its path override — even with an empty path, which just registers
the name with no override applied. This is the only way to make a category
show up before Sonarr/Radarr has ever connected, which matters specifically
for the SABnzbd shim: real SABnzbd (and this one, faithfully) has no API to
create a category on the fly, so Radarr/Sonarr's own SABnzbd "Test" step
rejects a category outright unless AcerviNode already knows about it — see
[SABnzbd API compatibility](sabnzbd-api.md#categories). Every registered
category (with or without a path) is persisted in `category_paths` and
survives a restart — `SetShimServers` re-seeds both shims' category stores
from it at startup, since those stores are otherwise purely in-memory.

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

`providers` is a map keyed by **entry name**, not by service. `torbox` and
`alldebrid` ship today; others land here as they're implemented — see
[Providers](providers.md) and the [roadmap](../ROADMAP.md).

An entry's name is free text and its `type` is what picks the implementation,
defaulting to the name. That split is what lets one service hold **two
accounts**: `torbox` and `torbox-work` can both set `type: torbox` and be
configured, defaulted and routed to entirely independently, each with its own
credentials, listing cache and rate-limit backoff — one account hitting a rate
limit doesn't slow the other down.

Providers also differ in what they can do, and routing accounts for that per
kind rather than globally: `default_provider` names one entry, but an add for a
kind that entry doesn't support falls through to the first configured entry
that *does*. Making AllDebrid the default would otherwise break usenet
outright — its adds would resolve to a provider with no usenet service and
fail, even with TorBox configured right beside it. Both compat shims are always mounted, whether or not a
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
([Providers](providers.md#completed-download-handling-internalimporter)) writes fetched files there
when one was supplied — an explicit `save_path` from the *arr app always wins.

When no `save_path` was supplied, AcerviNode falls back to `download_dir`, organized
as `<download_dir>/<category>/<name>/` — unless that category has its own override
configured via `category_paths` (`PUT /api/v1/settings/categories/path`, or the web
UI's Settings → Categories section), in which case files land
directly under `<override>/<name>/` instead. Useful for routing one category to a
different disk or mount than the rest — e.g. movies to a large secondary drive while
everything else stays on `download_dir`. Setting one doesn't require the category to
have been declared/seen by Sonarr/Radarr first — the web UI's "Register" form
takes any category name directly (path is optional — see [Categories](#categories)
above). `GET /api/v1/settings/categories`'s `paths` field reports the current
overrides.
