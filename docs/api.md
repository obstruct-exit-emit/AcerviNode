# Native API (`/api/v1`)

This is AcerviNode's own REST API — versioned, API-key authenticated, and the
exact same API the embedded web UI is built on. Nothing here is *arr-compat;
see [qBittorrent API](qbittorrent-api.md) and [SABnzbd API](sabnzbd-api.md) for
that surface.

## Auth

Every endpoint except `/health` requires `Authorization: Bearer <api_key>` (see
[Configuration](configuration.md)). Unlike a provider credential (see
`GET /api/v1/settings/providers`, which never echoes the actual TorBox key back),
`GET /api/v1/settings/general` does return AcerviNode's own `api_key` in
plaintext — there's nothing to protect by hiding it from a caller who already
had to present it to reach the endpoint, and the whole point of exposing it is
so a human can find and copy it from the web UI instead of digging through
server logs or `config.yaml`.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/health` | Unauthenticated liveness check |
| `GET` | `/api/v1/version` | Build version string |
| `GET` | `/api/v1/providers` | Configured providers and their capabilities (`torrent_capable`/`usenet_capable`/`webdl_capable`) |
| `GET` | `/api/v1/downloads` | Every download — torrent, usenet, or web download — most recently added first. Optional `?added_via=arr\|manual` scopes to just the web UI's Managed or Manual tab (see [Providers](providers.md#managed-vs-manual)); omitted or unrecognized returns everything |
| `POST` | `/api/v1/downloads/torrent` | Adds a torrent directly — `multipart/form-data` with either `magnet` or an uploaded `file` (a `.torrent`), plus optional `category`. Returns the created download, 201 (or 200 if the provider deduped it to one already tracked — see below) |
| `POST` | `/api/v1/downloads/usenet` | Adds an NZB directly — `multipart/form-data` with either `url` or an uploaded `file` (a `.nzb`), plus optional `category`. Same response shape/status codes as the torrent endpoint |
| `POST` | `/api/v1/downloads/webdl` | Adds a direct hoster link (Mega, 1Fichier, Mediafire, and ~160 others — see [Providers](providers.md#web-downloads)) — `application/x-www-form-urlencoded` body with `link` (required) and optional `category`. Link-only, no file-upload variant. Same response shape/status codes as the other two add endpoints |
| `GET` | `/api/v1/downloads/{id}` | One download's detail plus its file list — backs the web UI's per-download detail view. Files are queried live from the provider on every call, not cached locally (see below) |
| `GET` | `/api/v1/downloads/{id}/files/{fileId}/link` | Resolves a direct, provider-hosted download URL for one file — `fileId` is a file's `provider_file_id` from the download's `files` array. Fresh on every call, not cached; the URL is the provider's own CDN link, good for a browser to download straight from (no `Authorization` header needed for that second request — it's not one of ours). `503` if the relevant provider isn't configured; `502` for any other provider-side failure |
| `GET` | `/api/v1/downloads/{id}/zip-link` | Same idea, but one URL for every file at once, zipped provider-side — an explicit opt-in for a single archive instead of downloading files individually (see [Direct file downloads](#direct-file-downloads)). Same error shape as the per-file endpoint above |
| `DELETE` | `/api/v1/downloads/{id}?deleteFiles=true` | Deletes a download — provider call is best-effort, the local row is always cleaned up even if the provider call fails (matches the behavior already proven against a real upstream error, see ROADMAP.md Phase 1) |
| `POST` | `/api/v1/downloads/{id}/retry` | Manually retries a download that gave up after exhausting `import_max_retries` — resets `state` back to `provider_completed` and clears `retry_count`/`error_message`, so `internal/importer`'s very next tick attempts the fetch again from scratch. `409` if the download isn't currently in `error` state |
| `POST` | `/api/v1/downloads/{id}/readd` | Stronger sibling of `retry`, for when the *original* provider-side download is gone (e.g. expired from the provider's own list) rather than a transient fetch failure. Resubmits the download's stored original magnet/NZB URL/hoster link to the provider as a brand new add — a torrent's is always a magnet reconstructed from just its hash, so this works regardless of whether it was originally added by magnet or `.torrent` file upload — or, for a usenet download added via an uploaded `.nzb` file rather than a URL, the stored file bytes instead — then points the local row at the new `provider_download_id` (best-effort delete of the old one first). Works for any `protocol`/`added_via`, not just Managed torrents — see `has_source` below and [Providers](providers.md#re-add-for-a-discovered-download). `400` if nothing is stored to resubmit (a discovered download the provider no longer knows the source of, or a webdl added via a link the provider never recorded); `409` if not in `error` state, or if the fresh add happens to dedupe back to a different already-tracked download |
| `GET` | `/api/v1/settings/providers` | `{"torbox": {"configured": bool}}` — never the actual key, only whether one is set |
| `PUT` | `/api/v1/settings/providers/torbox` | Body `{"api_key": "..."}` — sets or replaces the TorBox key. Takes effect immediately (no restart) and is persisted to `config.yaml`; see [Providers](providers.md#live-settings) |
| `POST` | `/api/v1/settings/providers/torbox/test` | Makes one real, live call to TorBox with the currently configured key — a genuine connectivity+auth check, not just "is a key set." Returns `{"ok": true, "latency_ms": N}` or `{"ok": false, "error": "..."}` — always HTTP 200; the failure is in the body, not the status code |
| `GET` | `/api/v1/settings/general` | AcerviNode's own current configuration, including its own `api_key` in plaintext — see [Auth](#auth) for why that's not a secrecy problem here |
| `PUT` | `/api/v1/settings/general` | Body: `port`, `data_dir`, `download_dir`, `log_level`, `import_interval_seconds`, `import_max_retries`, `max_concurrent_downloads`, `import_fetch_timeout_seconds` (all required — send the full set, not a partial patch). Everything except `port`/`data_dir` applies immediately; those two are persisted but only take effect after a restart. Returns `{"restart_required": bool}` reflecting whether either of those two changed. Rejected (400) if any value fails the same validation `config.Load` applies at startup |
| `POST` | `/api/v1/settings/api-key/regenerate` | Replaces AcerviNode's own API key with a fresh random one. Takes effect immediately (every route, both compat shims included) and is persisted to `config.yaml`. Returns `{"api_key": "..."}` — the caller must switch to it right away, since the key it just authenticated with is now invalid everywhere, including for this same request's own credentials going forward |
| `GET` | `/api/v1/settings/categories` | `{"torrent": [...], "usenet": [...], "paths": {"category": "override-dir", ...}}` — every category name each compat shim currently knows about (populated reactively as *arr apps declare them), plus any per-category save-path overrides currently set |
| `POST` | `/api/v1/settings/categories` | Body `{"protocol": "torrent"\|"usenet", "name": "..."}` — manually registers a category, the same way an *arr app declaring one does. Not exposed in the web UI (a save-path override can be set for any category name directly, with no need to pre-declare it — see `PUT .../categories/path` below) but still available directly |
| `PUT` | `/api/v1/settings/categories/path` | Body `{"category": "...", "path": "..."}` — sets category's override destination directory, used by Completed Download Handling instead of `download_dir`/`<category>` (see [Configuration](configuration.md#categories-and-save-paths)). An empty `path` clears a previously set override. Takes effect immediately (no restart) and is persisted to `config.yaml`. 400 if `category` is empty |
| `GET` | `/api/v1/settings/account` | The configured provider's own account status (plan tier, subscription state, premium expiry, lifetime bytes downloaded) — a live call, not a cached snapshot. Always HTTP 200: `{"available": false, "error": "..."}` if nothing's configured yet or the provider doesn't support this; `{"available": true, "plan_name": "...", "is_subscribed": bool, "premium_expires_at": "...", "total_bytes_downloaded": N}` otherwise. See [Providers](providers.md#accountprovider) |

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
  "added_via": "arr",
  "has_source": true
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
UI's "+ Add" button uses. Always lands as `added_via: "manual"` (shown in the
Manual tab, never auto-fetched to local disk) — see
[Providers](providers.md#managed-vs-manual). All three endpoints still accept
an optional `category` field for programmatic callers, but the web UI's "+
Add" form doesn't offer it — category has no effect on a Manual download (see
[Providers](providers.md#managed-vs-manual) for why it was deliberately left
out). The `webdl` endpoint is genuinely link-only — a plain
`application/x-www-form-urlencoded` body, not `multipart/form-data` — since
TorBox's own Web Downloads service has no file-upload variant either. Errors:
`400` if neither a link (`magnet`/`url`/`link`) nor a `file` is given (`webdl`
only ever accepts `link`, never a `file`), `503` if the relevant provider isn't
configured yet, `502` for any other provider-side failure (e.g. an invalid
magnet, an unsupported hoster, or a real upstream error).

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
