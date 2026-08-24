# Native API (`/api/v1`)

This is AcerviNode's own REST API — versioned, API-key authenticated, and the
exact same API the embedded web UI is built on. Nothing here is *arr-compat;
see [qBittorrent API](qbittorrent-api.md) and [SABnzbd API](sabnzbd-api.md) for
that surface.

## Auth

Every endpoint except `/health` and the ones listed below requires either
`Authorization: Bearer <api_key>` (see [Configuration](configuration.md)) or
a valid `acervinode_session` cookie from a signed-in login account (see
[Providers](providers.md#auth-login-accounts-and-roles) for the full
design). The API key is what Sonarr/Radarr/scripts always use, since they
can't do cookie logins; the web UI itself always uses a login session — a
person signs in with a username and password, there's no way to browse the
dashboard by pasting the API key in instead. Unlike a provider credential
(see `GET /api/v1/settings/providers`, which never echoes the actual TorBox
key back), `GET /api/v1/settings/general` does return AcerviNode's own
`api_key` in plaintext — there's nothing to protect by hiding it from a
caller who already had to present it to reach the endpoint, and the whole
point of exposing it is so a human can find and copy it from the web UI
instead of digging through server logs or `config.yaml`.

Every `/api/v1/settings/*` endpoint (general config, providers, categories,
user management) additionally requires the **admin** role — a `member`
login account gets `403`. A member's access is scoped to Manual downloads
only; see the same Providers section for exactly what that means and why.

Unauthenticated by design, since each needs to answer before any credentials
necessarily exist yet: `GET /api/v1/health`, `GET /api/v1/auth/status`,
`POST /api/v1/auth/login`, `POST /api/v1/auth/logout`,
`GET /api/v1/setup/status`, `POST /api/v1/setup`.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/health` | Unauthenticated liveness check |
| `GET` | `/api/v1/version` | Build version string |
| `GET` | `/api/v1/providers` | Configured providers and their capabilities (`torrent_capable`/`usenet_capable`/`webdl_capable`) |
| `GET` | `/api/v1/status` | `internal/importer`'s own health signals — meant for an external monitor (Uptime Kuma, Healthchecks.io, ...) to poll and alert on, not for the web UI. `{"last_tick_at"?: "...", "kinds": {"torrent"\|"usenet"\|"webdl": {"last_successful_list_at"?: "...", "rate_limited_until"?: "...", "error_count": N}}}`. `last_tick_at` proves the background tick loop itself hasn't stalled/crashed, regardless of what any one kind found; each kind's `last_successful_list_at` is when that kind's provider last answered a bulk listing call without erroring (a kind can look "stuck" — no state changes — while this keeps advancing just fine, e.g. TorBox's `cooldown_until`, see [Providers](providers.md#cooldown_until--a-real-undocumented-account-restriction) — this endpoint answers a different question, "is polling itself working," not "is the provider account restricted"); `rate_limited_until` mirrors the same per-kind backoff the importer's logs already show; `error_count` is how many downloads of that kind currently sit in local `error` state. Distinct from `GET /api/v1/settings/account`'s `cooldown_until` (the provider's own account state) and `GET /api/v1/providers` (what's configured) — see [Providers](providers.md#status-monitoring) |
| `GET` | `/api/v1/downloads` | Every download — torrent, usenet, or web download — most recently added first. Optional `?added_via=arr\|manual` scopes to just the web UI's Managed or Manual tab (see [Providers](providers.md#managed-vs-manual)); omitted or unrecognized returns everything |
| `POST` | `/api/v1/downloads/torrent` | Adds a torrent directly — `multipart/form-data` with either `magnet` or an uploaded `file` (a `.torrent`), plus optional `category` and `added_via` (admin-only, see below). Returns the created download, 201 (or 200 if the provider deduped it to one already tracked — see below) |
| `POST` | `/api/v1/downloads/usenet` | Adds an NZB directly — `multipart/form-data` with either `url` or an uploaded `file` (a `.nzb`), plus optional `category` and `added_via`. Same response shape/status codes as the torrent endpoint |
| `POST` | `/api/v1/downloads/webdl` | Adds a direct hoster link (Mega, 1Fichier, Mediafire, and ~160 others — see [Providers](providers.md#web-downloads)) — `application/x-www-form-urlencoded` body with `link` (required) and optional `category`/`added_via`. Link-only, no file-upload variant. Same response shape/status codes as the other two add endpoints |
| `GET` | `/api/v1/downloads/{id}` | One download's detail plus its file list — backs the web UI's per-download detail view. Files are queried live from the provider on every call, not cached locally (see below) |
| `GET` | `/api/v1/downloads/{id}/files/{fileId}/link` | Resolves a direct, provider-hosted download URL for one file — `fileId` is a file's `provider_file_id` from the download's `files` array. Fresh on every call, not cached; the URL is the provider's own CDN link, good for a browser to download straight from (no `Authorization` header needed for that second request — it's not one of ours). `503` if the relevant provider isn't configured; `502` for any other provider-side failure |
| `GET` | `/api/v1/downloads/{id}/zip-link` | Same idea, but one URL for every file at once, zipped provider-side — an explicit opt-in for a single archive instead of downloading files individually (see [Direct file downloads](#direct-file-downloads)). Same error shape as the per-file endpoint above |
| `DELETE` | `/api/v1/downloads/{id}?deleteFiles=true` | Deletes a download — provider call is best-effort, the local row is always cleaned up even if the provider call fails (matches the behavior already proven against a real upstream error, see ROADMAP.md Phase 1). `deleteFiles=true` also removes the download's local files, if any (see docs/providers.md#local-file-deletion — a real gap until this was fixed: the provider call alone never touched local disk) |
| `POST` | `/api/v1/downloads/{id}/retry` | Manually retries a download that gave up after exhausting `import_max_retries` — resets `state` back to `provider_completed` and clears `retry_count`/`error_message`, so `internal/importer`'s very next tick attempts the fetch again from scratch. `409` if the download isn't currently in `error` state |
| `POST` | `/api/v1/downloads/{id}/readd` | Stronger sibling of `retry`, for when the *original* provider-side download is gone (e.g. expired from the provider's own list) rather than a transient fetch failure. Resubmits the download's stored original magnet/NZB URL/hoster link to the provider as a brand new add — a torrent's is always a magnet reconstructed from just its hash, so this works regardless of whether it was originally added by magnet or `.torrent` file upload — or, for a usenet download added via an uploaded `.nzb` file rather than a URL, the stored file bytes instead — then points the local row at the new `provider_download_id` (best-effort delete of the old one first). Works for any `protocol`/`added_via`, not just Managed torrents — see `has_source` below and [Providers](providers.md#re-add-for-a-discovered-download). `400` if nothing is stored to resubmit (a discovered download the provider no longer knows the source of, or a webdl added via a link the provider never recorded); `409` if not in `error` state, or if the fresh add happens to dedupe back to a different already-tracked download |
| `GET` | `/api/v1/settings/providers` | Every *registered* provider: `[{"name", "configured", "torrent_capable", "usenet_capable", "webdl_capable", "default"}]` — never the actual key, only whether one is set. Includes providers holding no credentials yet, since this is the surface you configure them from; contrast `GET /api/v1/providers`, which answers "what can I use right now" and omits them |
| `PUT` | `/api/v1/settings/providers/{name}` | Body `{"api_key": "..."}` — applies a key to that provider immediately (no restart) and persists it. An **empty** `api_key` clears the provider's credentials rather than being rejected: that's how a provider is switched off without hand-editing `config.yaml`, and it stays registered so it can be configured again later. `404` for a provider this build doesn't know about, so a typo is visible instead of looking like it worked |
| `PUT` | `/api/v1/settings/providers/default` | Body `{"provider": "..."}` — which provider a new download goes to when the add doesn't name one, and what both compat shims always use. `404` for an unknown name. Deliberately allows naming a provider with no key yet: setting the default before pasting its key is a reasonable order to work in |
| `POST` | `/api/v1/settings/providers/{name}/test` | A real, live connectivity and auth check against that provider with its currently configured key — not just "is a key set". Always `200`: a failed check is reported in the body as `{"ok": false, "error": "..."}`. `404` for an unknown provider |
| `GET` | `/api/v1/settings/general` | AcerviNode's own current configuration, including its own `api_key` in plaintext — see [Auth](#auth) for why that's not a secrecy problem here |
| `PUT` | `/api/v1/settings/general` | Body: `port`, `data_dir`, `download_dir`, `log_level`, `import_interval_seconds`, `import_max_retries`, `max_concurrent_downloads`, `import_fetch_timeout_seconds`, `cleanup_after_days`, `download_dir_mode`, `fast_poll_interval_seconds`, `provider_request_timeout_seconds`, `tls_enabled`, `tls_port`, `tls_cert_file`, `tls_key_file` (all required — send the full set, not a partial patch). Everything except `port`/`data_dir`/`tls_enabled`/`tls_port` applies immediately; those four are persisted but only take effect after a restart. Returns `{"restart_required": bool}` reflecting whether any of those four changed. Rejected (400) if any value fails the same validation `config.Load` applies at startup. `data_dir`/`tls_cert_file`/`tls_key_file` are part of the contract here (the web UI must still send their current, unchanged values), but the UI itself only shows `data_dir` read-only and doesn't surface the cert/key fields at all — see [Configuration](configuration.md) for why |
| `POST` | `/api/v1/settings/api-key/regenerate` | Replaces AcerviNode's own API key with a fresh random one. Takes effect immediately (every route, both compat shims included) and is persisted to `config.yaml`. Returns `{"api_key": "..."}` — the caller must switch to it right away, since the key it just authenticated with is now invalid everywhere, including for this same request's own credentials going forward |
| `GET` | `/api/v1/settings/categories` | `{"torrent": [...], "usenet": [...], "paths": {"category": "override-dir", ...}}` — every category name each compat shim currently knows about (populated reactively as *arr apps declare them), plus any per-category save-path overrides currently set |
| `POST` | `/api/v1/settings/categories` | Body `{"protocol": "torrent"\|"usenet", "name": "..."}` — manually registers a category, the same way an *arr app declaring one does. Not exposed in the web UI (a save-path override can be set for any category name directly, with no need to pre-declare it — see `PUT .../categories/path` below) but still available directly |
| `PUT` | `/api/v1/settings/categories/path` | Body `{"category": "...", "path": "..."}` — sets category's override destination directory, used by Completed Download Handling instead of `download_dir`/`<category>` (see [Configuration](configuration.md#categories-and-save-paths)). An empty `path` clears a previously set override but leaves the category itself registered. Takes effect immediately (no restart) and is persisted to `config.yaml`. 400 if `category` is empty |
| `DELETE` | `/api/v1/settings/categories/{category}` | Forgets a category entirely — its path override (if any) and its registration with both compat shims — unlike an empty `PUT .../path`, which only clears the override. If an *arr app is still actively configured with it, it simply reappears the next time it's declared again (a `createCategory` call, or an add using it), the same as it would against a real qBittorrent/SABnzbd install. Every well-known *arr-app default category (see [SABnzbd API](sabnzbd-api.md#categories)) is deletable the same way — it's registered exactly as if a user had added it by hand, not specially protected. |
| `GET` | `/api/v1/settings/backups` | The database snapshots currently on disk, newest first: `[{"name": "acervinode-20260824-001259.db", "size_bytes": N, "taken_at": "..."}]`. Names and sizes only — this endpoint deliberately never serves a snapshot's **contents**, because one contains every login account and session, and handing that over an API would be a far larger disclosure than any other endpoint here makes. Copy snapshots off the box with the tools you'd use for any other file. `taken_at` is read back out of the filename rather than the file's mtime, which a copy or a restore rewrites. Admin only |
| `POST` | `/api/v1/settings/backups` | Takes a snapshot immediately and prunes to `backup_keep`, returning `201` with `{"name": "..."}`. Identical to what the scheduler runs, so both produce the same result. Written with SQLite's `VACUUM INTO`, which is consistent against a live database and produces a self-contained, already-compacted file with no `-wal`/`-shm` alongside it. Refuses rather than overwrites if a file of that name already exists. Admin only |
| `GET` | `/api/v1/settings/account` | The configured provider's own account status (plan tier, subscription state, premium expiry, lifetime bytes downloaded) — a live call, not a cached snapshot. Always HTTP 200: `{"available": false, "error": "..."}` if nothing's configured yet or the provider doesn't support this; `{"available": true, "plan_name": "...", "is_subscribed": bool, "premium_expires_at": "...", "total_bytes_downloaded": N, "cooldown_until"?: "..."}` otherwise. `cooldown_until` is TorBox-specific, undocumented by TorBox itself, and only present when set — see [Providers](providers.md#cooldown_until--a-real-undocumented-account-restriction) for what it means and how it was found. See [Providers](providers.md#accountprovider) |
| `GET` | `/api/v1/auth/status` | Unauthenticated. `{"auth_enabled": bool, "authenticated": bool, "username"?: "...", "role"?: "admin"\|"member"}` — the web UI's own decision point between the login form and the (unaffected) API-key prompt |
| `POST` | `/api/v1/auth/login` | Unauthenticated. Body `{"username": "...", "password": "..."}` — sets the session cookie on success. `400` if no login accounts exist at all; `401` for a wrong username/password (deliberately slowed by ~500ms and logged, same either way, so a failure doesn't reveal whether the username existed) |
| `POST` | `/api/v1/auth/logout` | Unauthenticated by nature (safe to call with no session). Revokes the current session and clears the cookie. `204` |
| `GET` | `/api/v1/setup/status` | Unauthenticated. `{"needed": bool}` — whether this instance is claimable by its first visitor (no login account *and* no provider configured yet — see [Providers](providers.md#auth-login-accounts-and-roles)) |
| `POST` | `/api/v1/setup` | Unauthenticated, but refused (`403`) once the instance is no longer fresh. Body `{"username": "...", "password": "..."}` (password ≥ 8 characters) — creates the first (always admin, always the protected Default) login account and signs the browser in, in one step. No API key involved |
| `GET` | `/api/v1/settings/users` | Admin-only. `{"users": [{"username": "...", "role": "admin"\|"member", "default": bool}, ...]}` — never a password hash |
| `POST` | `/api/v1/settings/users` | Admin-only. Body `{"username": "...", "password": "...", "role": "admin"\|"member"}` (password ≥ 8 characters) — creates an additional login account. Returns the updated user list |
| `DELETE` | `/api/v1/settings/users/{username}` | Admin-only. Deletes a login account and ends its active sessions immediately. `400` for the protected Default account — promote a replacement via `.../default` first |
| `PUT` | `/api/v1/settings/users/{username}/role` | Admin-only. Body `{"role": "admin"\|"member"}` — promotes/demotes an account and ends its active sessions (so a demoted account can't keep using an admin session it already holds). `400` for the Default account, which can't be demoted |
| `POST` | `/api/v1/settings/users/{username}/default` | Admin-only. Promotes an account to the protected Default (and to admin, in the same step) |
| `PUT` | `/api/v1/settings/users/{username}/password` | **Not** admin-only — any signed-in account may change its own password. Changing someone *else's* password still requires admin. Body `{"password": "..."}` (≥ 8 characters). Ends the account's other sessions (the browser making the change keeps its own) |
| `POST` | `/api/v1/settings/system/restart` | Admin-only. Gracefully shuts down and exits so a restart-required setting already saved to `config.yaml` takes effect on the next start — see [Providers](providers.md#tls-self-signed-https). Returns `{"restarting": true, "supervised": bool}` before the process actually exits; `supervised` reflects whether systemd (or another supervisor setting `INVOCATION_ID`) is watching this process — `false` means nothing will automatically bring it back |
| `POST` | `/api/v1/settings/tls/regenerate` | Admin-only. Forces a fresh self-signed TLS certificate, discarding the current one — the fix when its SANs no longer match how the instance is reached. Requires a restart afterward to load it. `400` if a `tls_cert_file`/`tls_key_file` override is configured. Returns `{"restart_required": true}` |

## Download JSON shape

```json
{
  "id": "a1b2c3d4-...",
  "provider": "torbox",
  "protocol": "torrent",
  "hash": "dd8255ec...",
  "name": "Big Buck Bunny",
  "category": "tv-sonarr",
  "save_path": "/downloads/tv-sonarr",
  "size_bytes": 276445467,
  "state": "ready_for_import",
  "progress": 1,
  "added_at": "2026-07-27T05:15:00Z",
  "updated_at": "2026-07-27T05:16:17Z",
  "completed_at": "2026-07-27T05:16:17Z",
  "cached_at": "2026-07-27T05:15:03Z",
  "added_via": "arr",
  "has_source": true,
  "eta_seconds": 754,
  "seeders": 3,
  "leechers": 1,
  "download_speed_bytes": 191117,
  "phase": "",
  "airlocked": false
}
```

`state` is AcerviNode's own vocabulary (`queued`, `downloading`,
`provider_completed`, `ready_for_import`, `error`) — never either compat shim's
own state strings (qBittorrent's `downloading`/`uploading`/etc., or SABnzbd's
`Queued`/`Downloading`/etc.). `protocol` (`torrent`, `usenet`, or `webdl`) is
which debrid service this download belongs to — internally this is
`database.Kind`; it's named `protocol` here because that reads better to API
consumers than the Go-internal name (`Kind` avoids a clash with Go's own `type`
keyword, and matches the standard library's own `reflect.Kind` naming
convention for "which variant of a thing this is"). `webdl` has no *arr-facing
compat shim behind it at all (see [Providers](providers.md#webdownloadprovider))
— every `webdl` row is always `added_via: "manual"`. `id` is AcerviNode's own
identifier,
not the provider's — use it for `/downloads/{id}` calls, not `hash` or a
provider ID.

`progress` reports internal/importer's own live local-transfer progress
while `state` is `provider_completed` (files being fetched to local disk),
not the provider's own download progress — which is already `1` by that
point, since the provider itself is done. Falls back to the persisted
value once nothing's currently being fetched (e.g. right before a fetch
starts, or after one fails and is awaiting retry) — see
[Providers](providers.md#live-fetch-progress).

`cached_at` is set once, the first time this download is observed as
`provider_completed` — the provider itself is done, regardless of whether
files have been fetched to local disk yet. Distinct from `completed_at`,
which only fires once files are actually on disk: a Manual download that's
cached but never fetched has `cached_at` set and `completed_at` omitted
forever. Omitted (not `null`) until that first observation; cleared by
`POST .../readd` (a new provider-side download hasn't been observed cached
yet, whatever the old one's was).

Found live: "first observed as `provider_completed`" has two different
paths — a row that *transitions* into that state (`UpdateDownloadStatus`
sets `cached_at` there) and a row that's *born* there (TorBox's common
instant-cache case: already cached the instant it's added, so its very
first status ever is already "done" — no transition ever happens).
`InsertDownload` only started handling the second case recently; every row
inserted before that fix shipped stayed permanently `cached_at: null` no
matter how long it sat at `provider_completed`, since nothing ever revisits
a row whose state/progress/size/error genuinely never change again. A
one-time migration backfilled every such existing row from its `added_at`
— the closest true answer available, since a row born already cached was,
definitionally, cached at or before the moment it was added.

`retry_count` and `next_retry_at` are omitted entirely (not just zero/null) until
a download has failed at least once — see
[Providers](providers.md#completed-download-handling-internalimporter) for what sets them.
`added_via` (`arr` or `manual`) is permanent from the moment a download is
added — see [Providers](providers.md#managed-vs-manual) for what it means and
how a `manual` download can also show up without ever being added through
AcerviNode at all.

`has_source` reports whether `POST .../readd` could actually resubmit this
download if it's in `error` state — true for a link-based add (magnet/NZB
URL/hoster link, including one backfilled after the fact for a *discovered*
download, if the provider still knows it — see
[Providers](providers.md#re-add-for-a-discovered-download)) or a usenet
download added via an uploaded `.nzb` file (the raw bytes are stored for
exactly this — see
[Providers](providers.md#re-add-for-a-file-uploaded-nzb-not-discovered)).
False for a discovered download with nothing known, or a torrent/webdl added
via an uploaded `.torrent` file. The web UI's Re-add button is gated on this
rather than `added_via`, since it works for Managed and Manual alike.
`eta_seconds`/`seeders`/`leechers`/`download_speed_bytes`/`phase`/
`airlocked` are fast-moving, provider-reported fields deliberately never
persisted to the database — read from an in-memory cache (`database.DB.LiveStatus`) populated
as a side effect of whichever poller last refreshed this download (either
compat shim's own reactive refresh, or `internal/importer`'s bulk/fast
polls), not a synchronous provider call this endpoint makes itself. Zero
(and `phase` empty) whenever nothing's polled this download yet, or for a
provider/kind with no such concept — `seeders`/`leechers`/
`download_speed_bytes` are torrent-only; `phase` is usenet-only
(`"verifying"`/`"repairing"`/`"extracting"`/`"processing"`, or `""` for
plain transfer — see
[Providers](providers.md#usenet-post-processing-states)). `airlocked`
reports whether the provider is keeping this download in permanent storage,
exempt from the retention policy that would otherwise eventually remove it
(TorBox calls this AirLock); it is set from the provider's own side, never
by AcerviNode, and reads `false` until this download has been polled once.
Same "0 might mean unknown, not necessarily zero" tradeoff both compat shims already
accept for their own equivalent fields.

`GET /api/v1/downloads/{id}` additionally embeds a `files` array
(`[{"path": "...", "size_bytes": ..., "provider_file_id": "..."}]`), which the
list endpoint omits since it would mean an extra provider query per row for
something the table view doesn't show. `provider_file_id` is what
`.../files/{fileId}/link` needs to resolve a direct download URL for that
specific file — see [Direct file downloads](#direct-file-downloads) below. This is a
live query against the provider on every call, not a local cache — a queued
or still-processing download simply has an empty `files` array, not an error.

An empty `files` array can mean two different things, and `files_error`
(present only when the live query actually failed, omitted entirely
otherwise) is what tells them apart: absent means "not processed yet, ask
again later"; present means the provider query itself failed — e.g. the
provider genuinely no longer has this download at all, which is a real,
observed case for a Manual/discovered download (deleted directly through the
provider's own site — nothing else ever detects this for a Manual download,
since it's never in `internal/importer`'s fetch-retry path, which is the
only other place a "provider forgot about this" error would normally
surface). The web UI shows `files_error` directly instead of a generic
"no files yet" when present.

## Adding downloads directly

`POST /api/v1/downloads/torrent`, `POST /api/v1/downloads/usenet`, and
`POST /api/v1/downloads/webdl` let you add a download without going through
Sonarr/Radarr or faking being one against a compat shim — this is what the web
UI's "+ Add" button uses. Lands as `added_via: "manual"` by default (shown in
the Manual tab, never auto-fetched to local disk) — see
[Providers](providers.md#managed-vs-manual). An optional `category` field is
accepted by all three, but only has any effect on a Manual add if `added_via`
is also set — see below; on its own it's accepted but ignored, since category
has no meaning for a Manual download otherwise (see
[Providers](providers.md#managed-vs-manual) for why). The `webdl` endpoint is
genuinely link-only — a plain `application/x-www-form-urlencoded` body, not
`multipart/form-data` — since TorBox's own Web Downloads service has no
file-upload variant either. Errors: `400` if neither a link
(`magnet`/`url`/`link`) nor a `file` is given (`webdl` only ever accepts
`link`, never a `file`), `503` if the relevant provider isn't configured yet,
`429` if the provider rate-limited the add (retryable — TorBox meters
`createtorrent` at 60/hour for *uncached* torrents, and counts limits per
API key across its servers, so anything else sharing the key draws from the
same bucket), `502` for any other provider-side failure (e.g. an invalid
magnet, an unsupported hoster, or a real upstream error).

**`provider` (optional): choose which configured provider gets the
download.** Names an entry from `GET /api/v1/providers`; omitted, the
download goes to the configured default (`default_provider` in
[Configuration](configuration.md), which falls back to the only registered
provider when unset). A name that isn't configured is rejected with `400`
rather than quietly falling back — the caller asked for a specific account,
and silently using another would put the download somewhere they didn't
choose. Not admin-gated, unlike `added_via`: choosing between providers
already configured on this instance grants nothing a member couldn't
otherwise reach. Neither compat shim has an equivalent, since neither the
qBittorrent nor the SABnzbd protocol has a field to carry it — they always
use the default.

`POST /api/v1/downloads/{id}/readd` deliberately ignores this and always
resubmits to the provider the download already belongs to: sending a retry
elsewhere would silently migrate it, keeping the row's identity while its
files moved accounts.

**`added_via` (optional, admin-only): add straight into the Managed
pipeline.** Set to `"arr"` (any other value, or omitting it, keeps the
default `"manual"`) to have the new download behave exactly as if an *arr app
had added it — auto-fetched to `download_dir` (or `category`'s own override,
if one is set — see [Configuration](configuration.md#categories-and-save-paths))
by `internal/importer`, and shown in the Managed tab from then on. Requires
an admin session or the API key; a member requesting `"arr"` gets `403`
outright — the request is rejected before it ever reaches the provider, not
silently downgraded to Manual, since the web UI never sends this for a member
in the first place (the option isn't shown at all) and a script hitting the
endpoint directly deserves a clear error rather than a silent behavior
change. See [Providers](providers.md#managed-vs-manual).

Debrid providers dedupe by content: adding a magnet whose hash the provider
already has cached under an earlier add returns the *existing* tracked
download (`200`) instead of creating a duplicate row — the provider handed
back the same `torrent_id`/`usenetdownload_id` it gave out before, and
AcerviNode's schema has a uniqueness constraint on `(provider,
provider_download_id)` for exactly this reason. The existing row's original
category is kept; the new request's `category` is ignored in that case.

Both endpoints try `Status` on the just-added ID immediately, so the response
usually has the provider's real name/hash/size right away — same
provider-status-not-indexed-yet fallback (using the magnet/URL/filename
instead) as both compat shims already do on their own adds; see
[qBittorrent API](qbittorrent-api.md) and [SABnzbd API](sabnzbd-api.md).

## Cached & metadata previews

Three read-only, side-effect-free endpoints let the "+ Add" form show
whether something is worth adding *before* actually committing to it —
built on TorBox capabilities that existed at the client layer but were
never actually wired up anywhere until requested directly ("finish up
checkcached for all torrent/usenet/webdl. add torrent info because why
not"):

- `GET /api/v1/downloads/torrent/check-cached?magnet=...`
- `GET /api/v1/downloads/usenet/check-cached?url=...`
- `GET /api/v1/downloads/webdl/check-cached?link=...`

All three return `{"cached": true|false}` — whether the provider already
has this content cached (instantly available) rather than needing a real
download. `magnet`/`url`/`link` match the corresponding add endpoint's own
field name exactly. For torrent, the hash checked is the magnet's own
`xt=urn:btih:` infohash; for usenet/webdl, per TorBox's own (undocumented
in practice — see [Providers](providers.md#cached--metadata-previews))
convention, it's an MD5 of the link itself, computed server-side — the
caller never needs to know this. `400` if the value given doesn't parse
into a checkable hash at all (e.g. a magnet with no `btih`), `503` if the
relevant provider isn't configured.

`GET /api/v1/downloads/torrent/info?magnet=...` (or `?hash=...` directly)
previews a torrent's metadata — name, size, seeders/peers, and full file
list — straight from the BitTorrent network, by hash alone, before it's
ever added to the account at all. Response shape:

```json
{
  "available": true,
  "name": "...",
  "hash": "...",
  "size_bytes": 123,
  "seeds": 10,
  "peers": 12,
  "files": [{"path": "...", "size_bytes": 456}]
}
```

`available: false` (with just an `error` string, everything else omitted)
covers both a torrent TorBox couldn't find enough peers for within its own
search window, and a configured provider that doesn't support this kind of
preview at all — routine either way, matching `GET
/api/v1/settings/account`'s own `available: false` convention, never a
hard error status the UI would need special handling for. No usenet/webdl
equivalent exists — TorBox has no by-link metadata-preview endpoint for
either.

## Direct file downloads

`GET /api/v1/downloads/{id}/files/{fileId}/link` resolves a direct,
provider-hosted URL for one file — for downloading straight through a
browser instead of (or in addition to) `internal/importer` fetching it to
AcerviNode's own local disk. It's the exact same call `internal/importer`
itself makes when fetching a file (see
[Providers](providers.md#completed-download-handling-internalimporter)) — AcerviNode doesn't
proxy, cache, or otherwise sit in the middle of the actual transfer, it just
hands back what the provider gave it. The web UI's detail view shows a
"Download" button per file once `provider_file_id` is available — only for
a Manual download, though; a Managed one is already being auto-fetched to
local disk, so the UI doesn't offer a redundant manual grab for it (see
[Providers](providers.md#managed-vs-manual)). The endpoint itself doesn't
care either way — it works for any download's id, this is purely a web UI
choice about which buttons to show.

Two auth models meet at this boundary, deliberately: the `link` call itself
needs AcerviNode's own `Authorization: Bearer <api_key>` like every other
endpoint here, but the URL it returns is the provider's own — a plain
browser navigation to *that* URL needs no header at all. A raw `<a href>`
pointing straight at `.../link` wouldn't work (a browser navigation can't
attach a custom header), so a client has to `fetch` it first, then navigate
to the URL in the response.

`GET /api/v1/downloads/{id}/zip-link` is the "download everything" version
— one URL for the whole download, zipped provider-side, rather than
resolving and downloading each file separately. It's an explicit opt-in,
not the default: the Manual tab's per-row "Download all" button downloads
files individually (calling the per-file `link` endpoint once per file),
and `zip-link` is instead offered as a "Download all (zip)" button in the
detail view, for whoever wants one archive instead of several browser
downloads. Neither button appears for a Managed download — see above.
TorBox supports this via an
undocumented `zip_link=true` parameter on the same `requestdl` endpoint
(confirmed live, not found in any published docs — see
[Providers](providers.md#torbox-internaldebridtorbox)).

## What's thin here (see [Providers](providers.md) for why)

Beyond adding and observing/managing what's tracked, this API stays thin —
there's no bulk operations, no pause/resume, no priority. Sonarr/Radarr never
call this API directly regardless (they only know the compat shims); this
surface is for the web UI and anyone scripting against AcerviNode directly.
