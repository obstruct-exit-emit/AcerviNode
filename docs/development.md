# Development

Go 1.25+, no other runtime dependencies. SQLite via `modernc.org/sqlite` (pure Go,
no cgo).

```sh
go run ./cmd/acervinode     # starts on http://localhost:7846
go test ./...
go vet ./...
go build ./cmd/acervinode
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
internal/api/               native versioned REST API (/api/v1)
docs/                        this documentation
```

## Adding a provider

See [Providers](providers.md) for the interface shapes. The short version: a new
provider is a new subpackage under `internal/debrid/`, implementing
`debrid.TorrentProvider` and, only if the service genuinely supports it,
`debrid.UsenetProvider`. Neither compat shim nor the database layer needs to change
— they only ever depend on the interfaces, never on a concrete provider package.

## Releases

Not yet automated — see [Phase 5](../ROADMAP.md#phase-5--hardening--release-) on the
roadmap.
