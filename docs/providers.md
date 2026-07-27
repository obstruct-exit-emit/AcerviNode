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
genuinely supports NZB downloads. `cmd/acervinode`'s `buildProviders` function is
the one place that knows which concrete constructors exist for the configured
provider name — TorBox exports both `torbox.NewProvider` (torrents) and
`torbox.NewUsenetProvider` (usenet), so both compat shims mount. A torrent-only
provider like Real-Debrid would only export the first, and `buildHandler` simply
skips mounting the SABnzbd shim when the usenet constructor is absent, rather than
erroring.

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

Exact field names and error envelope are cross-checked against the official
[`torbox-sdk-go`](https://github.com/TorBox-App/torbox-sdk-go) rather than
guessed — see `internal/debrid/torbox/types.go` for the structs actually in use.

## Adding a new provider

1. New subpackage under `internal/debrid/<name>/`.
2. Implement `debrid.TorrentProvider`. Implement `debrid.UsenetProvider` too, only
   if the service has a real usenet backend.
3. Register it in `cmd/acervinode/main.go`'s provider construction — that's the
   only place a concrete provider type is referenced outside its own package.

No changes are needed in `internal/qbittorrent`, `internal/sabnzbd`, or
`internal/database` to add a provider — that's the point of the interface seam.
