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
internal/debrid/           TorrentProvider / UsenetProvider interfaces + shared types
    torbox/                  TorBox client + both provider adapters
internal/importer/        Completed Download Handling: fetches provider_completed
                            downloads' files to local disk over plain HTTP
internal/qbittorrent/     qBittorrent Web API compat shim (torrent-shaped adds)
internal/sabnzbd/          SABnzbd API compat shim (NZB-shaped adds)
internal/api/               native versioned REST API (/api/v1), what the UI runs on
web/                          React SPA (embedded via go:embed — see web/webui.go)
docs/                        this documentation
```

## Adding a provider

See [Providers](providers.md) for the interface shapes. The short version: a new
provider is a new subpackage under `internal/debrid/`, implementing
`debrid.TorrentProvider` and, only if the service genuinely supports it,
`debrid.UsenetProvider`. Neither compat shim nor the database layer needs to change
— they only ever depend on the interfaces, never on a concrete provider package.

## Releases

Cut by tagging `v*` — `.github/workflows/release.yml` builds the frontend, then
cross-compiles version-stamped (`-ldflags "-X main.version=..."`) Linux amd64/arm64
binaries and attaches them, each bundled with
[packaging/acervinode.service](../packaging/acervinode.service), to a GitHub
release. No Docker image and no packaged Windows build — not currently planned
(see the [roadmap](../ROADMAP.md)).
