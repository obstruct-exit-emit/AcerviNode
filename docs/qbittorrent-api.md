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
| `POST /api/v2/torrents/delete` | Removes a torrent, optionally deleting its files (`deleteFiles=true` — see docs/providers.md#local-file-deletion). Also records a delete tombstone (see docs/providers.md#managed-vs-manual) so a download an *arr app just removed isn't rediscovered as a fresh Manual download on the very next tick |
| `GET /api/v2/torrents/categories` / `POST createCategory` | Category bookkeeping — categories are stored on the AcerviNode side and echoed back, not interpreted |
| `POST /api/v2/torrents/setCategory` | Changes an already-tracked torrent's category — called by Sonarr/Radarr's `MarkItemAsImported` when a separate "post-import category" setting differs from the add-time one (confirmed against their real source: an optional setting, not part of the default add flow). Auto-registers the category the same permissive way `createCategory` does, rather than replicating real qBittorrent's stricter "category must already exist" 409 |
| `POST /api/v2/torrents/setShareLimits` / `topPrio` / `setForceStart` | Accepted as no-ops — called by Sonarr/Radarr only when specific optional client settings are enabled (seed ratio/time limits, "First" queue priority, "Force Start" initial state; confirmed against their real source), and AcerviNode has no seeding, priority-queue, or paused-state concept to actually apply them to. Returning success (rather than 404) is what lets an add complete normally for a user who has one of these turned on |

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

`progress` while `state` is `downloading` *and* the local state is
actually `provider_completed` reports internal/importer's own live local-
transfer progress (files being fetched to disk), not the provider's own
download progress — already `1.0` by that point — see
[Providers](providers.md#live-fetch-progress).

`GET /api/v2/torrents/info` also reports `num_seeds`/`num_leechs`/`dlspeed`
— real qBittorrent's own field names for swarm visibility — the same fresh,
never-persisted treatment as `eta`. Found missing live: TorBox reports
`seeds`/`peers`/`download_speed` on every torrent, but nothing anywhere in
AcerviNode ever captured or surfaced them before, which only became obvious
watching a real, genuinely uncached torrent download (TorBox's own
instant-cache path never shows meaningful swarm data at all). Sonarr/
Radarr's own `QBittorrentTorrent` model doesn't read these fields, so this
is for direct API inspection or a real qBittorrent-compatible client, not
something that changes *arr behavior.

`GET /api/v2/torrents/info`'s `save_path` and `content_path` are deliberately
different values, not a typo — this is real qBittorrent's own split (`save_path`
is the shared per-category base directory, `content_path` is one torrent's own
content root beneath it), and Sonarr/Radarr's own source (`QBittorrent.cs`'s
`GetItems`, confirmed directly, identical in both apps) only ever resolves a
completed download's import location from `content_path`, first checking it's
*not equal* to `save_path` as a sanity guard — for a real qBittorrent, a match
there means something's misconfigured, and Sonarr/Radarr refuse to import
rather than risk the wrong directory. AcerviNode's own `save_path` (the
per-download database column) is already the real content root a completed
download's files live in, i.e. exactly what real qBittorrent calls
`content_path` — reported as such, with the API response's own `save_path`
synthesized as its parent directory purely so the two are never equal. Found
live: `content_path` wasn't sent at all before this, which doesn't trigger the
"paths match" warning either — Sonarr/Radarr's own `ContentPath` property
just decodes to `null`, which isn't equal to `save_path`, so `GetItems` used
that `null` to resolve the import path anyway, meaning **no completed Managed
torrent could ever actually be imported through this shim** until this was
fixed (`toTorrentInfo`).

## What's not emulated

Anything not needed for add/track/resolve/delete and the \*arr "Test" flow — RSS,
search, peer/tracker detail, speed limits, and the rest of qBittorrent's full
surface aren't implemented and aren't planned unless a real integration need shows
up.
