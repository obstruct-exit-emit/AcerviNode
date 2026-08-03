# SABnzbd API compatibility

`internal/sabnzbd` implements enough of SABnzbd's real API (`/api?mode=...`) that
\*arr apps configured with a "SABnzbd" download client work against AcerviNode
without any special-casing on their end — the same integration decypharr offers
alongside its qBittorrent shim.

Every download added through this shim is `added_via: "arr"` — auto-fetched
to local disk by Completed Download Handling and shown in the web UI's
Managed tab — see [Providers](providers.md#managed-vs-manual).

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
| `queue`/`history` with `name=delete` | Removes one or more downloads by `nzo_id` (comma-separated in `value`) — layered onto the same mode as the list it removes from, matching SABnzbd's real API shape rather than a separate delete mode. `del_files=1` also deletes the provider-side download and, since it was found not to previously (see docs/providers.md#local-file-deletion), the local files too. Every delete also records a tombstone (see docs/providers.md#managed-vs-manual) so a download an *arr app just removed isn't rediscovered as a fresh Manual download on the very next tick |

## How NZB-shaped adds map onto TorBox

TorBox has a real usenet service (create/list/request-download-link/delete,
mirroring its torrent API), so `addfile`/`addurl` calls translate directly onto
TorBox's usenet endpoints via `debrid.UsenetProvider` — there's no fabrication or
protocol-bridging trick here, TorBox genuinely does usenet downloads.

## State mapping

Same approach as the [qBittorrent shim](qbittorrent-api.md): the internal
`downloads.state` machine is translated to SABnzbd's queue/history vocabulary only
at the HTTP boundary in `internal/sabnzbd/queue.go` and `history.go`. `queued`,
`downloading`, and `provider_completed` all stay in `/queue`. Only
`ready_for_import` moves to `/history` as `Completed`; `error` moves there as
`Failed` — either because the provider itself reported a failure (e.g.
TorBox's own "Error" state, or a stalled/no-seeds download — see
[Providers](providers.md#state-mapping)) or because Completed Download Handling
gave up after exhausting its own fetch retries.

`/queue`'s `status` field reports real SABnzbd's actual sub-phase strings, not
just a flat `Downloading` for everything still in progress — TorBox's usenet
service runs its own SABnzbd-style post-processing (par2 verify/repair,
archive extraction) server-side before a download is retrievable, and
Sonarr/Radarr's own `SabnzbdDownloadStatus` enum already has first-class
support for reporting it (see
[Usenet post-processing states](providers.md#usenet-post-processing-states)
for the full story, including a real bug in a comparable project this was
modeled on avoiding):

| Local state | `status` | Why |
|---|---|---|
| `queued` | `Queued` | Not yet accepted by the provider |
| `downloading` | `Downloading` / `Verifying` / `Repairing` / `Extracting` | Plain transfer, or one of TorBox's own post-processing sub-phases, if currently known — including TorBox's generic `"processing"` state (live-confirmed, held for several minutes on a real 6.8GB download), reported as `Verifying` since it has no exact real-SABnzbd equivalent |
| `provider_completed` | `Moving` | The provider itself is done; [Completed Download Handling](providers.md#completed-download-handling-internalimporter) hasn't fetched the files to local disk yet — Sonarr's import step needs them there first, so this deliberately still isn't reported as done |
| `ready_for_import` (moves to `/history`) | `Completed` | Files are actually on disk |
| `error` (moves to `/history`) | `Failed` | See above |

`mode=queue`'s `timeleft` field (`H:MM:SS`, matching real SABnzbd) reports the
provider's live ETA for the download — read fresh from the same provider call
that refreshes state/progress on every poll, not persisted to the database
(see `internal/sabnzbd`'s `refreshFromProvider`/`formatTimeLeft`).

`mode=queue` also reports a top-level `kbpersec` — real SABnzbd's own
aggregate download speed, summed across every active download (confirmed
against SABnzbd's real API docs: there's no per-slot speed field to match
even if AcerviNode wanted one — speed is queue-wide there). Found missing
entirely while auditing what information AcerviNode passes through: TorBox
reports `download_speed` on every usenet download too, but nothing captured
or summed it before.

`mode=history`'s slots report `bytes` (the download's total size) —
confirmed against Sonarr's real source (`SabnzbdHistoryItem`/`Sabnzbd.cs`'s
`GetHistory`) that it's read directly into the download item's own
`TotalSize`, not just cosmetic. Found missing during an API-parity audit:
every completed/failed item was reporting size `0` in Sonarr/Radarr's
Activity view. `nzb_name` and `download_time` are also real fields on this
response but confirmed unused by Sonarr's parsing — not added.

## Categories

Unlike qBittorrent's `createCategory`, **real SABnzbd has no API to create a
category on the fly** — categories can only be pre-configured through
SABnzbd's own admin interface, and Sonarr/Radarr's own `TestCategory()`
(confirmed against their real source) explicitly rejects a category that
doesn't already exist there, with a `DownloadClientValidationCategoryMissing`
validation error. AcerviNode's SABnzbd shim faithfully has the same
limitation — reactive category tracking (`addfile`/`addurl`'s own `cat=`
parameter registers a category the moment something uses it) can't help
here, since Sonarr/Radarr's Test step runs *before* anything is ever added.

Found live: a user configuring a brand new category in Radarr's SABnzbd
client got rejected outright on a fresh instance, since nothing had ever
told AcerviNode about that category name yet. The fix is the web UI's
Settings → Categories section: registering a category there (even with no
save-path override — the path field is optional) calls
`POST /api/v1/settings/categories` under the hood for *both* compat shims,
so it's immediately visible to `mode=get_config` and passes Radarr/Sonarr's
Test — the AcerviNode-side equivalent of pre-creating the category in real
SABnzbd's own admin UI first.

The qBittorrent shim doesn't have this problem: Sonarr/Radarr's own
`TestCategory()` for qBittorrent calls `createCategory` automatically for a
missing category (confirmed against their real source) before failing — see
[qBittorrent API compatibility](qbittorrent-api.md).

## What's not emulated

RSS, server switching/priorities, speed limits, and the rest of SABnzbd's full
surface aren't implemented and aren't planned unless a real integration need shows
up — the goal is \*arr compatibility, not a full SABnzbd reimplementation.
