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
the web UI's detail view.

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

## Adding a new provider

1. New subpackage under `internal/debrid/<name>/`.
2. Implement `debrid.TorrentProvider`. Implement `debrid.UsenetProvider` too, only
   if the service has a real usenet backend.
3. Register it in `cmd/acervinode`'s provider construction (`newTorBoxProviders`
   and `liveSettings`, or their equivalents for the new provider) — that's the
   only place a concrete provider type is referenced outside its own package.

No changes are needed in `internal/qbittorrent`, `internal/sabnzbd`, or
`internal/database` to add a provider — that's the point of the interface seam.
