# Native API (`/api/v1`)

This is AcerviNode's own REST API — versioned, API-key authenticated, and the
exact same API the embedded web UI is built on. Nothing here is *arr-compat;
see [qBittorrent API](qbittorrent-api.md) and [SABnzbd API](sabnzbd-api.md) for
that surface.

## Auth

Every endpoint except `/health` requires `Authorization: Bearer <api_key>` (see
[Configuration](configuration.md)).

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/health` | Unauthenticated liveness check |
| `GET` | `/api/v1/version` | Build version string |
| `GET` | `/api/v1/providers` | Configured providers and their capabilities (`torrent_capable`/`usenet_capable`) |
| `GET` | `/api/v1/downloads` | Every download, torrent or usenet, most recently added first |
| `GET` | `/api/v1/downloads/{id}` | One download's detail plus its file list |
| `DELETE` | `/api/v1/downloads/{id}?deleteFiles=true` | Deletes a download — provider call is best-effort, the local row is always cleaned up even if the provider call fails (matches the behavior already proven against a real upstream error, see ROADMAP.md Phase 1) |
| `GET` | `/api/v1/settings/providers` | `{"torbox": {"configured": bool}}` — never the actual key, only whether one is set |
| `PUT` | `/api/v1/settings/providers/torbox` | Body `{"api_key": "..."}` — sets or replaces the TorBox key. Takes effect immediately (no restart) and is persisted to `config.yaml`; see [Providers](providers.md#live-settings) |

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

## What's thin here (see [Providers](providers.md) for why)

There's no `POST /api/v1/downloads` to add a download directly — adds go through
whichever compat shim (or *arr app) is already in use. This API is for observing
and managing what's already tracked, which is exactly what the embedded UI needs.
