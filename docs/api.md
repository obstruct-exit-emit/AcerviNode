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
| `GET` | `/api/v1/providers` | Configured providers and their capabilities (`torrent_capable`/`usenet_capable`) |
| `GET` | `/api/v1/downloads` | Every download, torrent or usenet, most recently added first |
| `POST` | `/api/v1/downloads/torrent` | Adds a torrent directly — `multipart/form-data` with either `magnet` or an uploaded `file` (a `.torrent`), plus optional `category`. Returns the created download, 201 (or 200 if the provider deduped it to one already tracked — see below) |
| `POST` | `/api/v1/downloads/usenet` | Adds an NZB directly — `multipart/form-data` with either `url` or an uploaded `file` (a `.nzb`), plus optional `category`. Same response shape/status codes as the torrent endpoint |
| `GET` | `/api/v1/downloads/{id}` | One download's detail plus its file list — backs the web UI's per-download detail view |
| `DELETE` | `/api/v1/downloads/{id}?deleteFiles=true` | Deletes a download — provider call is best-effort, the local row is always cleaned up even if the provider call fails (matches the behavior already proven against a real upstream error, see ROADMAP.md Phase 1) |
| `POST` | `/api/v1/downloads/{id}/retry` | Manually retries a download that gave up after exhausting `import_max_retries` — resets `state` back to `provider_completed` and clears `retry_count`/`error_message`, so `internal/importer`'s very next tick attempts the fetch again from scratch. `409` if the download isn't currently in `error` state |
| `POST` | `/api/v1/downloads/{id}/readd` | Stronger sibling of `retry`, for when the *original* provider-side download is gone (e.g. expired from the provider's own list) rather than a transient fetch failure. Resubmits the download's stored original magnet/NZB URL to the provider as a brand new add, then points the local row at the new `provider_download_id` (best-effort delete of the old one first). `400` if no source was stored (added via file upload — nothing to resubmit); `409` if not in `error` state, or if the fresh add happens to dedupe back to a different already-tracked download |
| `GET` | `/api/v1/settings/providers` | `{"torbox": {"configured": bool}}` — never the actual key, only whether one is set |
| `PUT` | `/api/v1/settings/providers/torbox` | Body `{"api_key": "..."}` — sets or replaces the TorBox key. Takes effect immediately (no restart) and is persisted to `config.yaml`; see [Providers](providers.md#live-settings) |
| `POST` | `/api/v1/settings/providers/torbox/test` | Makes one real, live call to TorBox with the currently configured key — a genuine connectivity+auth check, not just "is a key set." Returns `{"ok": true, "latency_ms": N}` or `{"ok": false, "error": "..."}` — always HTTP 200; the failure is in the body, not the status code |
| `GET` | `/api/v1/settings/general` | AcerviNode's own current configuration, including its own `api_key` in plaintext — see [Auth](#auth) for why that's not a secrecy problem here |
| `PUT` | `/api/v1/settings/general` | Body: `port`, `data_dir`, `download_dir`, `log_level`, `import_interval_seconds`, `import_max_retries` (all required — send the full set, not a partial patch). `download_dir`/`log_level`/`import_interval_seconds`/`import_max_retries` apply immediately; `port`/`data_dir` are persisted but only take effect after a restart. Returns `{"restart_required": bool}` reflecting whether either of those two changed. Rejected (400) if any value fails the same validation `config.Load` applies at startup |
| `POST` | `/api/v1/settings/api-key/regenerate` | Replaces AcerviNode's own API key with a fresh random one. Takes effect immediately (every route, both compat shims included) and is persisted to `config.yaml`. Returns `{"api_key": "..."}` — the caller must switch to it right away, since the key it just authenticated with is now invalid everywhere, including for this same request's own credentials going forward |
| `GET` | `/api/v1/settings/categories` | `{"torrent": [...], "usenet": [...]}` — every category name each compat shim currently knows about, populated reactively as *arr apps declare them |
| `POST` | `/api/v1/settings/categories` | Body `{"protocol": "torrent"\|"usenet", "name": "..."}` — manually registers a category, the same way an *arr app declaring one does. Useful for pre-populating the "Add Download" form's category field |

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
  "completed_at": "2026-07-27T05:16:17Z"
}
```

`state` is AcerviNode's own vocabulary (`queued`, `downloading`,
`provider_completed`, `ready_for_import`, `error`) — never either compat shim's
own state strings (qBittorrent's `downloading`/`uploading`/etc., or SABnzbd's
`Queued`/`Downloading`/etc.). `protocol` (`torrent` or `usenet`) is which of the
two compat shims' worlds this download belongs to — internally this is
`database.Kind`; it's named `protocol` here because that reads better to API
consumers than the Go-internal name (`Kind` avoids a clash with Go's own `type`
keyword, and matches the standard library's own `reflect.Kind` naming
convention for "which variant of a thing this is"). `id` is AcerviNode's own
identifier,
not the provider's — use it for `/downloads/{id}` calls, not `hash` or a
provider ID.

`retry_count` and `next_retry_at` are omitted entirely (not just zero/null) until
a download has failed at least once — see
[Providers](providers.md#completed-download-handling) for what sets them.
`GET /api/v1/downloads/{id}` additionally embeds a `files` array
(`[{"path": "...", "size_bytes": ...}]`), which the list endpoint omits since it
would mean an extra query per row for something the table view doesn't show.

## Adding downloads directly

`POST /api/v1/downloads/torrent` and `POST /api/v1/downloads/usenet` let you add
a download without going through Sonarr/Radarr or faking being one against a
compat shim — this is what the web UI's "+ Add" button uses. Errors:
`400` if neither a link (`magnet`/`url`) nor a `file` is given, `503` if the
relevant provider isn't configured yet, `502` for any other provider-side
failure (e.g. an invalid magnet or a real upstream error).

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

## What's thin here (see [Providers](providers.md) for why)

Beyond adding and observing/managing what's tracked, this API stays thin —
there's no bulk operations, no pause/resume, no priority. Sonarr/Radarr never
call this API directly regardless (they only know the compat shims); this
surface is for the web UI and anyone scripting against AcerviNode directly.
