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
| `GET` | `/api/v1/downloads/{id}` | One download's detail plus its file list — backs the web UI's per-download detail view |
| `DELETE` | `/api/v1/downloads/{id}?deleteFiles=true` | Deletes a download — provider call is best-effort, the local row is always cleaned up even if the provider call fails (matches the behavior already proven against a real upstream error, see ROADMAP.md Phase 1) |
| `GET` | `/api/v1/settings/providers` | `{"torbox": {"configured": bool}}` — never the actual key, only whether one is set |
| `PUT` | `/api/v1/settings/providers/torbox` | Body `{"api_key": "..."}` — sets or replaces the TorBox key. Takes effect immediately (no restart) and is persisted to `config.yaml`; see [Providers](providers.md#live-settings) |
| `GET` | `/api/v1/settings/general` | AcerviNode's own current configuration, including its own `api_key` in plaintext — see [Auth](#auth) for why that's not a secrecy problem here |
| `POST` | `/api/v1/settings/api-key/regenerate` | Replaces AcerviNode's own API key with a fresh random one. Takes effect immediately (every route, both compat shims included) and is persisted to `config.yaml`. Returns `{"api_key": "..."}` — the caller must switch to it right away, since the key it just authenticated with is now invalid everywhere, including for this same request's own credentials going forward |

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

## What's thin here (see [Providers](providers.md) for why)

There's no `POST /api/v1/downloads` to add a download directly — adds go through
whichever compat shim (or *arr app) is already in use. This API is for observing
and managing what's already tracked, which is exactly what the embedded UI needs.
