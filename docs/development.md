# Development

Go 1.25+ backend, React 19 + Vite frontend (Node 22+) — matching LibriNode's own
stack exactly, embedded into the same binary via `go:embed`.

```sh
cd web && npm install && npm run build && cd ..   # frontend — only needed after UI changes
go run ./cmd/acervinode                            # starts on http://localhost:7846
go test ./...
go vet ./...
go build ./cmd/acervinode                          # embeds web/dist if present
```

A committed `web/dist/.gitkeep` keeps `go build` working on a fresh clone even
before `npm run build` has ever run — you'll just get an empty `/` until it has.
That cuts the other way too, though: `go build` on its own never checks
whether `web/dist` is *current*, just that something's there — see
[Installation](installation.md#updating-an-existing-from-source-install) for
why that matters and reach for `make build` instead of a bare `go build`
whenever you actually want a real binary to run or deploy (not just the
`go run` loop above, which serves the frontend from `web/dist` the same way
either way).

Frontend-only iteration (Node 22+):

```sh
cd web
npm install
npm run dev      # Vite dev server on its own port, proxies /api to :7846
npm run build    # production build into web/dist, embedded on the next go build
```

> **Windows note:** developed on Windows day to day; the backend is plain Go and
> builds fine here, and every feature (including Completed Download Handling —
> plain HTTP, no FUSE) runs the same on Windows as on Linux. Production deployment
> still targets Linux (same posture as LibriNode) for packaging/systemd reasons,
> not because of any Windows-incompatible feature.

## Layout

```
cmd/acervinode/         entrypoint: config, database, provider registry, HTTP servers, importer
internal/config/         config.yaml + env overrides, defaults/validation
internal/database/        SQLite open + embedded migrations, downloads CRUD
internal/debrid/           provider interfaces (Torrent/Usenet/WebDownload/
                            Account/TorrentInfo), the Registry, shared types
    torbox/                  TorBox client + all three provider adapters
    alldebrid/               AllDebrid client + torrent and web-download adapters
internal/backup/          scheduled VACUUM INTO snapshots of the database
internal/tlscert/          self-signed certificate generation for the HTTPS listener
internal/importer/        Completed Download Handling: fetches provider_completed
                            downloads' files to local disk over plain HTTP
internal/qbittorrent/     qBittorrent Web API compat shim (torrent-shaped adds)
internal/sabnzbd/          SABnzbd API compat shim (NZB-shaped adds)
internal/api/               native versioned REST API (/api/v1), what the UI runs on
web/                          React SPA (embedded via go:embed — see web/webui.go)
docs/                        this documentation
```

## Adding a provider

See [Providers](providers.md#adding-a-new-provider) for the full guide and the
interface shapes. The short version: a new provider is a new subpackage under
`internal/debrid/`, implementing only the interfaces the service genuinely
supports, then registered in `knownProviders` and `knownProviderCapabilities` in
`cmd/acervinode/main.go`. Implementing an interface the service can't actually
back is worse than leaving it out — an unregistered kind is never routed to that
provider, while a stub gets routed adds it can only fail. Neither compat shim,
the database layer, nor `internal/api` needs to change: they only ever depend on
the interfaces, never on a concrete provider package. Adding AllDebrid touched
none of them.

## Releases

Cut by tagging `v*` — `.github/workflows/release.yml` builds the frontend, then
cross-compiles version-stamped (`-ldflags "-X main.version=..."`) Linux amd64/arm64
binaries and attaches them, each bundled with
[packaging/acervinode.service](../packaging/acervinode.service), to a GitHub
release. No Docker image and no packaged Windows build — not currently planned
(see the [roadmap](../ROADMAP.md)).
