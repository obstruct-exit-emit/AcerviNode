<div align="center">

# 📦 AcerviNode

**A debrid download client for Sonarr, Radarr, and LibriNode.**

AcerviNode speaks the qBittorrent Web API and the SABnzbd API, so your *arr apps add it as a normal download client and never know the download isn't real — it resolves everything through a debrid provider instead of doing actual P2P or NNTP work.

[![Release](https://img.shields.io/github/v/release/obstruct-exit-emit/AcerviNode?include_prereleases&label=release)](https://github.com/obstruct-exit-emit/AcerviNode/releases)
[![CI](https://github.com/obstruct-exit-emit/AcerviNode/actions/workflows/ci.yml/badge.svg)](https://github.com/obstruct-exit-emit/AcerviNode/actions/workflows/ci.yml)
[![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)

</div>

> 🚧 **Pre-1.0.** TorBox is the only wired provider so far, but the full pipeline
> works end to end against it: point Sonarr's qBittorrent client or its SABnzbd
> client at AcerviNode and it adds, tracks, resolves, and downloads real files to
> disk where Sonarr's own import step expects them — and there's a web UI to watch
> it happen. Real-Debrid and other providers aren't built yet. See the
> [roadmap](ROADMAP.md).

---

## Why AcerviNode?

AcerviNode is a self-contained debrid download client. It replaces
[decypharr](https://github.com/sirrobot01/decypharr) for people who want a single
static Go binary with an embedded SQLite store instead of a Docker container that
needs `/dev/fuse`, `SYS_ADMIN`, and `rshared` mount propagation just to start. If you
already run [LibriNode](https://github.com/obstruct-exit-emit/LibriNode) for your
library, AcerviNode is its sibling for your download pipeline: same binary
philosophy, same API conventions, same operator experience — built by the same
author, in the same style, on purpose.

## Features

**⬇️ Two compat shims, one provider layer**

| Shim | Protocol it emulates | Status |
|---|---|---|
| qBittorrent | qBittorrent Web API (`/api/v2/...`) — torrent-shaped adds | ✅ working |
| SABnzbd | SABnzbd API (`/api?mode=...`) — NZB-shaped adds | ✅ working |

Both shims sit on top of the same provider interfaces, so a Sonarr/Radarr instance
can be configured either way and land on the same download pipeline underneath.

**🔌 Debrid providers**

| Provider | Torrents | Usenet | Web Downloads | Status |
|---|---|---|---|---|
| TorBox | ✅ | ✅ | ✅ | working |
| Real-Debrid | — | — | — | planned |
| Others (Debrid-Link, AllDebrid, Premiumize) | — | — | — | planned |

"Web Downloads" debrids a direct link from a hoster (Mega, 1Fichier, Mediafire,
and ~160 others TorBox currently supports) — no torrent or NZB involved, just a
plain URL.

The provider interface is deliberately thin: a new torrent-only provider (like
Real-Debrid) is a pure addition — no changes to either compat shim, no changes to
the storage layer.

**📥 Completed Download Handling**

- Once a download finishes on the provider side, `internal/importer` fetches the
  actual files over plain HTTP and writes them to `save_path` — the same thing a
  normal download client does, just sourced from a debrid CDN link instead of
  BitTorrent/NNTP. No FUSE, no Linux-only mount — this runs the same on Windows.
- A fetch that fails is retried with exponential backoff, not forever and not
  instantly — `import_max_retries` (default 5) caps how many attempts a download
  gets before it's moved to `error` instead of retried again.
- Status is synced from the provider proactively on every tick, not just when
  Sonarr/Radarr happens to poll — a download progresses even if nothing but the
  web UI is watching it.

**🖥️ Native API + web UI**

- Versioned REST API (`/api/v1`): health, version, provider status, download
  listing/management/**adding** (magnet, .torrent, NZB URL/.nzb file, or a
  direct hoster link — no need to go through Sonarr/Radarr or fake being
  one), settings — API-key authenticated, the exact API the UI itself uses
- A React (Vite) single-page dashboard, embedded into the binary, split into
  **Managed** (added through Sonarr/Radarr, auto-fetched to disk) and
  **Manual** (added directly, or discovered already sitting in your TorBox
  account — never auto-fetched, browse and grab files on demand instead) —
  live state/progress, provider status, one-click delete, a "+ Add" button
  to push a magnet/torrent file/NZB/hoster link straight in, a per-download
  detail view (full metadata, file list, retry status), and a Settings tab
  to add/change your TorBox key without touching `config.yaml` — takes
  effect immediately, no restart
- The Settings tab also surfaces AcerviNode's own configuration (port, data/download
  dirs, log level, import settings), its own API key — copyable straight from
  the UI instead of digging through server logs or `config.yaml`, with a
  one-click regenerate that applies immediately across the native API and both
  compat shims — and your TorBox account's own plan/subscription/usage status

**🗄️ Storage**

- SQLite (pure Go, no cgo) — one `downloads` table shared by both shims, tracking
  every add from `queued` through `ready_for_import`
- Embedded, ordered migrations

## Quick start

```sh
go build ./cmd/acervinode
./acervinode
```

Then open `http://localhost:7846` for the dashboard. Full steps, including
pointing Sonarr at AcerviNode as either a qBittorrent or a SABnzbd client:
[Installation](docs/installation.md) · [Quickstart](docs/quickstart.md).

> Tagged releases publish Linux amd64/arm64 binaries with a systemd unit — see
> [Installation](docs/installation.md). No Docker image, no packaged Windows
> build — not currently planned (see the [roadmap](ROADMAP.md)); production
> deployment targets Linux, same as LibriNode, though every feature runs the same
> on Windows for local development.

## Documentation

| | |
|---|---|
| [Installation](docs/installation.md) | Linux, from source |
| [Quickstart](docs/quickstart.md) | First-run walkthrough, both compat shims |
| [Configuration](docs/configuration.md) | config.yaml, providers, ports |
| [Providers](docs/providers.md) | The provider interfaces, TorBox specifics, adding a new provider |
| [API](docs/api.md) | The native `/api/v1` — everything the web UI does is scriptable |
| [qBittorrent API](docs/qbittorrent-api.md) | Which qBittorrent Web API surface is emulated, and why |
| [SABnzbd API](docs/sabnzbd-api.md) | Which SABnzbd API surface is emulated, and how NZB adds map onto TorBox's usenet service |
| [Development](docs/development.md) | Building, layout, contributing |
| [Roadmap](ROADMAP.md) | Development history and what's next |

## Architecture

- **Backend:** Go — one self-contained binary per OS, no runtime dependencies
- **Database:** SQLite (pure Go, no cgo), embedded migrations
- **Provider layer:** `internal/debrid` defines `TorrentProvider` and `UsenetProvider`
  interfaces; `internal/debrid/torbox` is the first concrete implementation
- **Compat shims:** `internal/qbittorrent` and `internal/sabnzbd` each translate a
  real \*arr download-client protocol onto the provider interfaces
- **Completed Download Handling:** `internal/importer` fetches finished downloads'
  files to local disk over plain HTTP — no FUSE, no Linux-only mount
- **Native API + UI:** `internal/api` (`/api/v1`) backs `web/`, a React (Vite)
  single-page app embedded into the binary via `go:embed`
- **Default port:** `7846` · **License:** GPL-3.0

## Security

API-key auth on the native API (and, by default, on both compat shims — the same
key). For remote access, run behind a TLS reverse proxy.

## Development

```sh
cd web && npm install && npm run build && cd ..   # frontend (Node 22+)
go build ./cmd/acervinode                          # backend (Go 1.25+)
./acervinode                                       # http://localhost:7846
go test ./...
go vet ./...
```

See [Development](docs/development.md) for the full package layout.

## License

[GPL-3.0](LICENSE) — the same family as Sonarr, Radarr, Prowlarr, and
[LibriNode](https://github.com/obstruct-exit-emit/LibriNode). (decypharr itself is
MIT; this is a deliberate departure to match the *arr-ecosystem convention.)
