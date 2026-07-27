# SABnzbd API compatibility

`internal/sabnzbd` implements enough of SABnzbd's real API (`/api?mode=...`) that
\*arr apps configured with a "SABnzbd" download client work against AcerviNode
without any special-casing on their end — the same integration decypharr offers
alongside its qBittorrent shim.

## Why offer this alongside the qBittorrent shim

Some \*arr setups are already standardized on a SABnzbd-shaped client, or use
SABnzbd-specific features in their import logic. Offering both compat shims means
AcerviNode is a drop-in replacement either way a user already has things
configured, rather than forcing a client-type migration just to adopt it.

## Auth model

Unlike the qBittorrent shim's cookie-based session, SABnzbd's real API checks an
`apikey` query parameter on every single request. AcerviNode checks it against the
configured `api_key` (see [Configuration](configuration.md)) on every call, no
login step required.

## Endpoints implemented

| `mode=` | Purpose |
|---|---|
| `version` | Probed by \*arr apps when you click "Test" |
| `get_config` | Category listing |
| `addfile` | Multipart NZB file upload, plus a `cat` (category) |
| `addurl` | Add by NZB URL |
| `queue` | Active/pending downloads — polled repeatedly while a download is active |
| `history` | Completed/failed downloads |
| `fullstatus` | Basic server status |

## How NZB-shaped adds map onto TorBox

TorBox has a real usenet service (create/list/request-download-link/delete,
mirroring its torrent API), so `addfile`/`addurl` calls translate directly onto
TorBox's usenet endpoints via `debrid.UsenetProvider` — there's no fabrication or
protocol-bridging trick here, TorBox genuinely does usenet downloads.

## State mapping

Same approach as the [qBittorrent shim](qbittorrent-api.md): the internal
`downloads.state` machine is translated to SABnzbd's queue/history vocabulary only
at the HTTP boundary in `internal/sabnzbd/queue.go` and `history.go`. `queued`,
`downloading`, and `provider_completed` all stay in `/queue` (the latter as
`Downloading`, since [Completed Download Handling](providers.md#completed-download-handling)
hasn't fetched the files to local disk yet, and Sonarr's import step needs them
there first). Only `ready_for_import` moves to `/history` as `Completed`; `error`
moves there as `Failed`.

## What's not emulated

RSS, server switching/priorities, speed limits, and the rest of SABnzbd's full
surface aren't implemented and aren't planned unless a real integration need shows
up — the goal is \*arr compatibility, not a full SABnzbd reimplementation.
