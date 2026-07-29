# Providers

AcerviNode talks to debrid services through two small interfaces in
`internal/debrid`. Every add, status check, and link resolution a compat shim
performs goes through one of these — never through a concrete provider package
directly.

## `TorrentProvider`

Backs the [qBittorrent shim](qbittorrent-api.md). Methods:

- `Name() string`
- `AddMagnet(ctx, magnetURI string, opts AddOptions) (ProviderDownloadID, error)`
- `AddTorrentFile(ctx, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error)`
- `Status(ctx, id ProviderDownloadID) (DownloadStatus, error)`
- `List(ctx) ([]DownloadStatus, error)`
- `Files(ctx, id ProviderDownloadID) ([]DownloadFile, error)`
- `RequestDownloadLink(ctx, id ProviderDownloadID, fileID string) (url string, error)`
- `Delete(ctx, id ProviderDownloadID, deleteFiles bool) error`
- `CheckCached(ctx, hashes []string) (map[string]bool, error)` — providers without
  a cache-check endpoint may return all-`false` rather than implement real logic

## `UsenetProvider`

Backs the [SABnzbd shim](sabnzbd-api.md). Same shape, NZB-flavored:

- `Name() string`
- `AddNZBFile(ctx, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error)`
- `AddNZBURL(ctx, url string, opts AddOptions) (ProviderDownloadID, error)`
- `Status`, `List`, `Files`, `RequestDownloadLink`, `Delete` — identical semantics
  to `TorrentProvider`'s versions

This is a **separate, optional** interface on purpose: not every debrid service has
a real usenet backend. A provider package implements it only if the service
genuinely supports NZB downloads. `cmd/acervinode`'s `newTorBoxProviders` function is
the one place that knows which concrete constructors exist for the configured
provider name — TorBox exports both `torbox.NewProvider` (torrents) and
`torbox.NewUsenetProvider` (usenet). A torrent-only provider like Real-Debrid would
only export the first — see [Live settings](#live-settings) below for what happens
to the SABnzbd shim in that case (it stays mounted either way, now — it just
answers with an error until a usenet-capable provider is configured).

TorBox's torrent and usenet capabilities are two separate types on purpose, not one
type satisfying both interfaces at once, even though their method sets could
technically overlap (`Status`, `List`, `Files`, etc. have identical signatures
across both interfaces): TorBox's torrent IDs and usenet download IDs are separate,
provider-assigned numeric spaces with no guarantee against collision, so a single
`Status(ctx, id)` couldn't safely tell which service `id` belongs to. Keeping them
as `torbox.Provider` and `torbox.UsenetProvider`, each wrapping the same underlying
`torbox.Client`, avoids that ambiguity.

## What the interfaces deliberately leave out

Categories and save paths are a compat-shim/database concern, not a provider
concern — \*arr apps set them purely to know which local path to watch for
completed imports, and AcerviNode stores them directly on the `downloads` row. The
provider interfaces stay protocol-agnostic.

## Live settings

Provider credentials can be set or changed two ways: hand-editing
`providers.torbox.api_key` in `config.yaml` (restart required, the original
mechanism), or `PUT /api/v1/settings/providers/torbox` (the web UI's Settings tab
uses this) — no restart needed. Both end up in the same place.

This works because `cmd/acervinode` never hands a concrete provider directly to
the compat shims, the importer, or the native API — it hands each of them the
same `*debrid.DynamicTorrentProvider`/`*debrid.DynamicUsenetProvider` instance
(`internal/debrid/dynamic.go`), which implements the real provider interfaces by
delegating to whatever's currently `Set()`. Both compat shims are **always**
mounted now (not conditionally on a provider existing at startup) — before a key
is set, every provider-backed call just returns `debrid.ErrNoProvider` instead of
the route not existing, which is what makes configuring TorBox for the first time
through the settings API (not just at startup) possible at all. `cmd/acervinode`'s
`liveSettings` type is what `PUT` actually calls: it swaps the Dynamic wrappers'
inner provider and calls `config.Save` to persist the change, all under one mutex
so concurrent settings changes don't race.

The settings API is deliberately narrow — `SetTorBoxAPIKey`, not a generic
"configure any provider" endpoint — since TorBox is the only provider that exists.
Generalize this (and `Settings` in `internal/api`) when a second provider is added.

## Completed Download Handling (`internal/importer`)

Neither interface has a "download the bytes to disk" method — that's deliberately
one level up, in `internal/importer`, built entirely on `List`, `Files`, and
`RequestDownloadLink`, which both interfaces already provide. A background loop
(`Importer.Run`, ticking every `import_interval_seconds`) does two things every
tick: refreshes every tracked download's state from its provider
(`refreshStatuses`, see below), then finds every `downloads` row now in
`provider_completed` state and resolves each file's real link, streaming it over
plain HTTP to `save_path` (or `download_dir` as a fallback) — the same thing a
normal download client does, just sourced from a debrid CDN link instead of
BitTorrent/NNTP. A row only reaches `ready_for_import` once its files are actually
on disk; both compat shims report `provider_completed` as still "downloading" to
\*arr apps for exactly this reason — see [qBittorrent API](qbittorrent-api.md) and
[SABnzbd API](sabnzbd-api.md).

This works identically for any future provider, torrent or usenet, with zero
changes — it only depends on `List`/`Files`/`RequestDownloadLink`, which every
provider already has to implement.

### Proactive status refresh

Both compat shims sync a download's local state against the provider
*reactively* — only when an \*arr app happens to call `GET /api/v2/torrents/info`
or `mode=queue`. On its own, that meant a download's state only ever advanced
when something external polled one of those endpoints; watching only the native
API or web UI (neither of which touches a provider at all) could leave a
finished download looking permanently "queued", and even an actively-polling
\*arr app only caught up on its own poll cadence.

`Importer.refreshStatuses` closes that gap: every tick, it calls each configured
provider's `List` for both kinds and applies the result via
`database.RefreshFromProvider` — the exact same sync logic both compat shims'
`refreshFromProvider` call, now shared in one place (`internal/database`) instead
of duplicated per shim, so all three interpret a provider's state identically.
Because this runs on `import_interval_seconds` regardless of external polling, a
download that finishes between polls — or with nothing polling at all — is
picked up within one tick, and if that same tick moves it into
`provider_completed`, its files get fetched immediately after, in the same
`Tick` call. `List` errors are logged, except `debrid.ErrNoProvider` (no key
configured yet), which is expected and would otherwise spam the log every tick.

This does **not** shrink whatever delay exists on the provider's own side — TorBox's
`mylist` (even with `bypass_cache=true`, see below) has been observed taking a few
minutes to index a brand-new torrent, independent of how it's polled. What this
closes is AcerviNode's own contribution to the delay: previously a finished
download could sit unnoticed indefinitely with nothing polling; now it's picked
up within one `import_interval_seconds` tick of the provider actually reflecting
it, guaranteed.

A fetch that fails (a `Files`/`RequestDownloadLink` call error, or the HTTP
download itself failing) doesn't retry on every subsequent tick forever, and
doesn't retry instantly either: `Importer.handleFailure` records the failure and
schedules the next attempt with exponential backoff — attempt *N* waits
`import_interval_seconds`×2^*N*, capped at one hour — stored on the row as
`retry_count`/`next_retry_at` (`Tick` only picks up rows whose `next_retry_at`
has passed). Once `retry_count` reaches `import_max_retries`, the download is
moved to `error` instead of scheduled again, so a permanently-broken link stops
occupying a retry slot forever rather than silently never finishing. Both fields
are surfaced on `GET /api/v1/downloads/{id}` — see [API](api.md) — and shown in
the web UI's detail view. This give-up is sticky by design —
`database.RefreshFromProvider` won't silently resurrect it back to
`provider_completed` just because the provider still reports its old
"completed" state on a later poll (`retry_count > 0` is what distinguishes a
local give-up like this one from a provider-reported error — see
[State mapping](#state-mapping) below, where the opposite is true: those
recover automatically).

A download that gave up isn't stuck forever, though: `POST
/api/v1/downloads/{id}/retry` resets it back to `provider_completed` with
`retry_count` cleared, so the next tick attempts it again from scratch — the
manual counterpart to the automatic backoff above, for when the underlying
cause (a transient rate limit, the provider being briefly down) has since
cleared. The web UI's detail view shows a "Retry" button once a download is
in `error` state.

Sometimes retry alone isn't enough, though — confirmed live: a torrent that
kept failing with "not found" turned out to have expired from TorBox's own
`mylist` entirely, not a transient fetch problem retry could recover from.
For that, `POST /api/v1/downloads/{id}/readd` resubmits the download's
original magnet/NZB URL (stored as `Download.Source` at add time, for
link-based adds only — nothing's kept for an uploaded file) to the provider
as a genuinely new add, then points the local row at the new
`provider_download_id`. The web UI shows both "Retry" and "Re-add" side by
side once a download is in `error` state.

A download's files don't need to be fetched to local disk at all to be
usable: `GET /api/v1/downloads/{id}/files/{fileId}/link` resolves a direct,
provider-hosted URL for one file — the exact same `RequestDownloadLink` call
`fetchFile` above makes, just handed straight back to the caller instead of
being streamed to disk. Always a live provider call, never cached — see
[API](api.md#direct-file-downloads). This also meant `GET /api/v1/downloads/{id}`
needed a real file list to attach a link to, which surfaced a genuine
pre-existing bug: the local `download_files` table it read from was defined
but nothing ever populated it, so `files` was always `[]` in practice, even
for a fully completed download. Fixed by having it query the provider live
too, the same way `internal/qbittorrent`'s own file listing already did —
see CHANGELOG.

### Managed vs. Manual

Not every download should be auto-fetched to local disk. An *arr app strictly
needs that — its own import step scans `save_path` and finds nothing if the
files aren't actually there — but a download added directly (through the web
UI's own "+ Add" form, or sitting in the provider's account entirely outside
AcerviNode) has no such requirement; the point of adding it that way is
usually to browse/grab files on demand, the way TorBox's own web UI works,
not to have it silently land on disk.

`database.Download.AddedVia` is the permanent, immutable record of which of
the two a given download is — set once at insert time, from *how* it was
added, never changed afterward:

- **`arr`**: added through the qBittorrent or SABnzbd compat shim — i.e. by
  an *arr app. Auto-fetched by Completed Download Handling like always. Shown
  in the web UI's **Managed** tab.
- **`manual`**: added directly via the native API's add endpoints (the web
  UI's own "+ Add" form — an *arr app has no way to reach that endpoint, it
  only knows the compat shims), or *discovered* — see below. Never
  auto-fetched; `ListDownloadsDueForRetry` filters to `arr` only, so a manual
  download sitting in `provider_completed` just stays there, and the user
  grabs files on demand via the same per-file/zip-link endpoints Managed
  downloads use. Shown in the web UI's **Manual** tab. Retry/Re-add aren't
  offered for a manual download in `error` state either — there's no local
  fetch attempt to retry, the row is just reflecting the provider's own live
  state (see [State mapping](#state-mapping) above for how it gets there).

**Discovery** is what makes an item added directly through TorBox's own
site/app — not through AcerviNode at all — show up in Manual too, not just
items added through AcerviNode's own "+ Add" form. Every tick,
`Importer.discoverManual` diffs the same provider `List()` call
`refreshStatuses` already makes against what's locally tracked (by
`provider_download_id`); anything present at the provider with no local row
at all gets adopted as a fresh `manual` download.

The one wrinkle: the very first time this runs for a given provider+kind,
nothing is adopted. Every currently-unmatched item is instead recorded into
`discovery_baseline` (with `discovery_seeded` as the per-provider-per-kind
marker that seeding has already happened) and permanently ignored — this is
what stops the feature from flooding the Manual tab with an account's entire
pre-existing history the moment it ships. Only items that show up
*afterward* — added to TorBox at any time from then on, whether through
AcerviNode or directly — are ever adopted. A discovered download has no
`Source` (there's no original magnet/NZB URL to know), so Re-add is never
available for one even in the (currently impossible, since manual downloads
never reach `error` via a local fetch attempt) hypothetical case Retry/Re-add
were shown for it.

## TorBox (`internal/debrid/torbox`)

The first, and so far only, concrete provider. TorBox exposes both a torrent
service and a usenet service under `https://api.torbox.app/v1/api`, authenticated
with `Authorization: Bearer <key>` (the `requestdl` endpoint also accepts
`token=<key>` as a query parameter, since the resulting URL is meant to be handed
directly to a downloader).

Torrent endpoints used: `POST /torrents/createtorrent` (magnet or multipart file),
`GET /torrents/mylist`, `GET /torrents/checkcached`,
`POST /torrents/controltorrent`, `GET /torrents/requestdl`.

Usenet endpoints follow the same shape under a `/usenet/...` path family (add,
list, request-download-link, control/delete).

### State mapping

TorBox reports a `download_state` string (shared across both services) that
`internal/debrid/torbox/provider.go`'s `mapDownloadState` translates into
AcerviNode's provider-agnostic `debrid.DownloadState`. The vocabulary itself
isn't published as an exhaustive list anywhere in TorBox's own docs, so it was
ported from [decypharr](https://github.com/sirrobot01/decypharr)'s own
production mapping (`pkg/debrid/providers/torbox/torbox.go`'s
`getTorboxStatus`) rather than guessed — the reference implementation this
project benchmarks against, and one that's actually running against real
TorBox accounts. A qualifier TorBox appends to some states (e.g. `"stalled
(no seeds)"`) is stripped before matching, same as decypharr's own regex.

The important part: **anything unmatched is treated as an error**, not "still
downloading" — this includes a stalled/no-seeds torrent. TorBox's own [help
center](https://support.torbox.app/en/articles/9928977-download-statuses)
independently confirms an explicit `"Error"` state exists (server error,
missing encryption key, missing par2 files, etc.), which previously had
nowhere to go but the same bucket as genuinely-still-downloading states —
found while auditing the whole state machine, not from a specific bug report,
and confirmed against the real account's own data (`mapDownloadState` is
tested directly against `"stalled (no seeds)"`, the exact raw string a real
torrent on the test account had at the time).

A local `error` state reached this way is *not* sticky — if the provider
later reports genuine progress again (e.g. a stalled torrent finds a seed),
it recovers automatically on the next refresh. Contrast with
[Completed Download Handling](#completed-download-handling)'s own retry
exhaustion below, which *is* sticky by design.

Both also fall back to `GET /queued/getqueued?type=torrent|usenet` — a
separate pre-processing queue TorBox holds a download in (e.g. an account
concurrency limit, or backend load) *before* it appears in `mylist`/
`usenet/mylist` at all. Found by inspecting a comparable open-source debrid
client's own polling code ([RDT-Client](https://github.com/rogerfar/rdt-client)),
which checks both endpoints where AcerviNode previously only checked the
`mylist` family — see `Provider.List`/`Status` and `UsenetProvider.List`/
`Status`, which merge a queued-only entry in (or fall back to it) so a
backlogged download shows as genuinely `queued` instead of "not found."
`queued/getqueued` carries no progress/state/size — only proof the download
exists and is pending — so this closes a narrow visibility gap, not a speed
gap: once something's actually downloading, both AcerviNode and RDT-Client
read progress from the same `mylist` endpoint the same way.

**Confirmed live (not just from docs):** `mylist`/`usenet/mylist` is cached
server-side for up to 600 seconds by default — a freshly added torrent was simply
absent from the response until `bypass_cache=true` was passed. Both `ListTorrents`
and `ListUsenetDownloads` always set it, since AcerviNode's whole polling model
(both compat shims' `refreshFromProvider`, and `internal/importer`'s own ticks)
depends on this endpoint reflecting current state promptly, not on a 10-minute
delay.

Exact field names and error envelope are cross-checked against the official
[`torbox-sdk-go`](https://github.com/TorBox-App/torbox-sdk-go) rather than
guessed — see `internal/debrid/torbox/types.go` for the structs actually in use.
That said, the SDK's own docs aren't always right either: `createusenetdownload`'s
`usenetdownload_id` is documented there as a string, but a real account's response
sends it as a JSON number (found via a real NZB upload failing with `json: cannot
unmarshal number into Go struct field ...usenetdownload_id of type string`) — same
numeric shape as a torrent's `torrent_id`, and now decoded/formatted the same way
(`CreateUsenetDownload`).

`requestdl` also has an undocumented `zip_link=true` parameter — omit
`file_id` and add it, and TorBox returns a single URL that zips every file
in the torrent/usenet download server-side. Not mentioned in the official
SDK or public docs; found by testing directly against a real account, then
confirmed the returned URL actually serves a real `.zip`
(`Content-Type: application/zip`, correct total size) via `curl -I`. Backs
`RequestZipDownloadLink` and `GET /api/v1/downloads/{id}/zip-link` — see
[API](api.md#direct-file-downloads). The torrent side is directly verified live;
the usenet side (`RequestUsenetZipDownloadLink`) mirrors the same shape but
wasn't independently confirmed — by the time it was written, every usenet
download on the test account had expired from `mylist` (0 items), leaving
nothing live to test `zip_link` against on that side specifically.

## Adding a new provider

1. New subpackage under `internal/debrid/<name>/`.
2. Implement `debrid.TorrentProvider`. Implement `debrid.UsenetProvider` too, only
   if the service has a real usenet backend.
3. Register it in `cmd/acervinode`'s provider construction (`newTorBoxProviders`
   and `liveSettings`, or their equivalents for the new provider) — that's the
   only place a concrete provider type is referenced outside its own package.

No changes are needed in `internal/qbittorrent`, `internal/sabnzbd`, or
`internal/database` to add a provider — that's the point of the interface seam.
