# qBittorrent API compatibility

`internal/qbittorrent` implements enough of qBittorrent's real Web API
(`/api/v2/...`) that Sonarr, Radarr, and other \*arr apps configured with a
"qBittorrent" download client work against AcerviNode without any special-casing
on their end. This is the same integration point
[decypharr](https://github.com/sirrobot01/decypharr) uses.

Every download added through this shim is `added_via: "arr"` — auto-fetched
to local disk by Completed Download Handling and shown in the web UI's
Managed tab — see [Providers](providers.md#managed-vs-manual).

## Why emulate qBittorrent specifically

\*arr apps don't have an "AcerviNode" client type — they only know how to talk to
download clients whose protocol they recognize. qBittorrent's Web API is one of the
most widely supported client types across the \*arr ecosystem, so emulating it
gives AcerviNode a drop-in integration path with zero changes required on the
\*arr side.

## Endpoints implemented

| Endpoint | Purpose |
|---|---|
| `POST /api/v2/auth/login` / `logout` | Cookie-based session, matching qBt's own auth flow |
| `GET /api/v2/app/version` / `webapiVersion` | Probed by \*arr apps when you click "Test" |
| `GET /api/v2/app/preferences` | Reports `save_path` (AcerviNode's `download_dir`) plus fixed "disabled" values for every seeding/ratio/queueing field AcerviNode has no concept of. **Not optional** — confirmed against Sonarr's own source (`QBittorrentProxyV2.GetConfig`, called by `TestConnection`), this is the *first* request a real Sonarr/Radarr "Test" makes, before anything else. Missing entirely (a plain 404) until found live — every "Test" failed outright, regardless of how correctly everything else was configured |
| `POST /api/v2/torrents/add` | Accepts a magnet URL or a multipart `.torrent` file upload, plus a `category` |
| `GET /api/v2/torrents/info` | Lists tracked torrents, optionally filtered by hash(es) — polled repeatedly while a download is active |
| `GET /api/v2/torrents/properties` | Per-torrent detail (save path, size, ...) |
| `GET /api/v2/torrents/files` | Per-file listing, used by \*arr apps to map imports |
| `POST /api/v2/torrents/delete` | Removes a torrent, optionally deleting its files |
| `GET /api/v2/torrents/categories` / `POST createCategory` | Category bookkeeping — categories are stored on the AcerviNode side and echoed back, not interpreted |

## State mapping

AcerviNode's internal `downloads.state` column (`queued` → `downloading` →
`provider_completed` → `ready_for_import` → `error`) is translated to the specific
qBittorrent state strings \*arr apps pattern-match on at the HTTP boundary in
`internal/qbittorrent/torrents.go` — the internal state machine stays
provider-agnostic, and only this translation layer needs to know qBittorrent's
specific vocabulary:

| Local state | qBittorrent state | Why |
|---|---|---|
| `queued` | `queuedDL` | Not yet accepted by the provider |
| `downloading` | `downloading` | Provider is fetching it |
| `provider_completed` | `downloading` | Provider is done, but [Completed Download Handling](providers.md#completed-download-handling-internalimporter) hasn't fetched the files to local disk yet — reporting `uploading` here would send Sonarr's import step looking for files that don't exist yet |
| `ready_for_import` | `uploading` | Files are actually on disk; safe to report as complete/seeding |
| `error` | `error` | Either the provider itself reported a failure (e.g. TorBox's own "Error" state, or a stalled/no-seeds torrent — see [Providers](providers.md#state-mapping)) or Completed Download Handling gave up after exhausting its own fetch retries |

`GET /api/v2/torrents/info`'s `eta` field reports the provider's live ETA
(seconds) for the download — read fresh from the same provider call that
refreshes state/progress on every poll, not persisted to the database (it's a
fast-moving, purely informational value; see `internal/qbittorrent`'s
`refreshFromProvider`).

## What's not emulated

Anything not needed for add/track/resolve/delete and the \*arr "Test" flow — RSS,
search, peer/tracker detail, speed limits, and the rest of qBittorrent's full
surface aren't implemented and aren't planned unless a real integration need shows
up.
