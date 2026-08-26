<div align="center">

# 📦 AcerviNode

**A debrid download client for Sonarr, Radarr, and LibriNode.**

AcerviNode speaks the qBittorrent Web API and the SABnzbd API, so your *arr apps add it as a normal download client and never know the download isn't real — it resolves everything through a debrid provider instead of doing actual P2P or NNTP work.

[![Release](https://img.shields.io/github/v/release/obstruct-exit-emit/AcerviNode?include_prereleases&label=release)](https://github.com/obstruct-exit-emit/AcerviNode/releases)
[![CI](https://github.com/obstruct-exit-emit/AcerviNode/actions/workflows/ci.yml/badge.svg)](https://github.com/obstruct-exit-emit/AcerviNode/actions/workflows/ci.yml)
[![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)

</div>

> 🚧 **Pre-1.0.** TorBox and AllDebrid are wired, and you can configure more
> than one account on either. The full pipeline works end to end: point
> Sonarr's qBittorrent client or its SABnzbd client at AcerviNode and it adds,
> tracks, resolves, and downloads real files to disk where Sonarr's own import
> step expects them — and there's a web UI to watch it happen. Real-Debrid and
> the rest aren't built yet. See the
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
| AllDebrid | ✅ | — *(no usenet service)* | ✅ | working |
| Others (Real-Debrid, Debrid-Link, Premiumize) | — | — | — | planned |

"Web Downloads" debrids a direct link from a hoster (Mega, 1Fichier, Mediafire,
and ~160 others TorBox currently supports) — no torrent or NZB involved, just a
plain URL.

AllDebrid has no usenet service at all, so it never appears as an option for a
usenet add — a provider simply doesn't show up for a kind it can't do. Its
hoster debriding *is* supported: unlocking is synchronous with nothing to poll,
so AcerviNode saves the link to the account, which gives it something durable to
list, track and delete like any other download.

**More than one account** is supported, on the same service or different ones —
give each entry its own name in `providers`, set `type` when the name isn't the
service's, and pick which one new downloads go to:

```yaml
providers:
  torbox:
    api_key: ...
  alldebrid:
    api_key: ...
  torbox-shared:
    type: torbox
    api_key: ...
default_provider: torbox
```

Every download records which account it came from, so deletes, retries and file
fetches always go back to the right one.

The provider interface is deliberately thin: a new torrent-only provider (like
Real-Debrid) is a pure addition — no changes to either compat shim, no changes to
the storage layer.

**📥 Completed Download Handling**

- Once a download finishes on the provider side, `internal/importer` fetches the
  actual files over plain HTTP and writes them to `save_path` — the same thing a
  normal download client does, just sourced from a debrid CDN link instead of
  BitTorrent/NNTP. No FUSE, no Linux-only mount, so it builds and runs on Windows too — though the packaged, supported deployment is Linux with systemd.
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
  **Manual** (added directly, or discovered already sitting in your provider
  account — never auto-fetched, browse and grab files on demand instead) —
  live state/progress, provider status, one-click delete, a "+ Add" button
  to push a magnet/torrent file/NZB/hoster link straight in, a per-download
  detail view (full metadata, streamed/zip/per-file downloads, retry
  status), and a Settings tab to add/change any provider's key without
  touching `config.yaml` — takes effect immediately, no restart
- A Manual download whose provider item vanishes entirely (deleted directly
  through the provider's own site, or genuinely expired) is detected proactively
  and flagged, instead of sitting stuck looking "Available" forever. Re-add
  works for it too, not just a Managed download — it resubmits the original
  magnet/NZB URL/hoster link, or a stored NZB file for one added by upload,
  whenever the provider (or AcerviNode itself) still knows it
- The Settings tab also surfaces AcerviNode's own configuration (port, data/download
  dirs, log level, import settings), its own API key — copyable straight from
  the UI instead of digging through server logs or `config.yaml`, with a
  one-click regenerate that applies immediately across the native API and both
  compat shims — and each provider account's own plan/subscription/usage status

**🗄️ Storage**

- SQLite (pure Go, no cgo) — one `downloads` table shared by both shims, tracking
  every add from `queued` through `ready_for_import`
- Embedded, ordered migrations
- **Automatic backups, on by default** — everything AcerviNode knows (config,
  history, categories, login accounts) lives in that one file, so it snapshots
  itself daily and keeps the last 7. Taken with SQLite's own consistent-snapshot
  support, so they're safe to take while it's running and open cleanly on their
  own — see [Backups and restore](docs/installation.md#backups-and-restore)

## What AcerviNode is, and isn't

Worth reading before you install it, so nothing here is a surprise later.

**What it does**

- Takes torrent, NZB and hoster-link adds from Sonarr/Radarr/Lidarr/Readarr by
  pretending to be qBittorrent and SABnzbd, hands them to a debrid provider,
  and downloads the finished result to local disk for the \*arr app to import.
- Runs as **one static Go binary** with an embedded SQLite database and an
  embedded web UI. No external database, no runtime dependencies.
- Supports **TorBox and AllDebrid**, several accounts per service, with
  per-provider control over which kinds each one handles.

**What it deliberately doesn't do**

- **No mount.** Every download is fetched in full to local disk before an
  \*arr app imports it. There is no FUSE/rclone layer presenting the provider
  as a filesystem, so **you need disk space for everything you download**, and
  an import waits for the whole file rather than starting instantly. This is
  the single biggest difference from decypharr, and it is a design choice —
  the cost is disk and latency, the benefit is no `/dev/fuse`, no `SYS_ADMIN`,
  no mount propagation, and nothing to unwedge when a mount goes stale.
- **No Docker image.** Packaged as a Linux tarball with a systemd unit. The
  binary itself is portable, but the supported install story is
  Linux + systemd.
- **Not a BitTorrent or NNTP client.** It speaks to debrid providers only. It
  never joins a swarm or talks to a news server, which is also why it needs no
  ports open and no VPN.
- **Single instance, single machine.** No clustering, no shared-state
  deployment, no multi-node coordination. One binary, one SQLite file.
- **Not a media manager.** It has no library, no renaming, no metadata. That
  is the \*arr app's job, and AcerviNode hands off to it.

**Things worth knowing before you rely on it**

- **A member account is trusted with the provider account.** Anyone who can
  sign in as a member can obtain a download link that carries your provider
  API key — see [API](docs/api.md#direct-file-downloads) for exactly why and
  what was measured. Create member accounts only for people you'd trust with
  the provider credentials themselves.
- **Retention is off by default.** `cleanup_after_days`,
  `cleanup_error_after_days` and `stuck_download_timeout_minutes` all default
  to `0`, meaning nothing is ever removed automatically. That is deliberate —
  every one of them deletes things, and a default that quietly discards data
  is a bad default — but it does mean history and local files accumulate
  until you turn them on. Backups are the exception and default to **on**,
  since doing nothing there is the only case that costs you something.

  Sensible starting values, if you want them on: `cleanup_after_days: 7`
  (a week to notice a bad import before a handed-off download is tidied),
  `cleanup_error_after_days: 14` (longer, because an error row is diagnostic),
  and `stuck_download_timeout_minutes: 120` (a download reporting no change
  at all for two hours is genuinely stuck; this only marks it `error`, it
  deletes nothing, and `retry` puts it back).
- **It has been run in earnest on one machine.** Fresh install, upgrade,
  backup/restore and full Sonarr/Radarr integration are all verified, but by
  one operator on one distro. Treat early releases accordingly.

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
| [Providers](docs/providers.md) | The provider interfaces, the multi-provider registry, TorBox and AllDebrid specifics, adding a new provider |
| [API](docs/api.md) | The native `/api/v1` — everything the web UI does is scriptable |
| [qBittorrent API](docs/qbittorrent-api.md) | Which qBittorrent Web API surface is emulated, and why |
| [SABnzbd API](docs/sabnzbd-api.md) | Which SABnzbd API surface is emulated, and how NZB adds map onto a provider's usenet service |
| [Development](docs/development.md) | Building, layout, contributing |
| [Roadmap](ROADMAP.md) | Development history and what's next |

## Architecture

- **Backend:** Go — one self-contained binary per OS, no runtime dependencies
- **Database:** SQLite (pure Go, no cgo), embedded migrations
- **Provider layer:** `internal/debrid` defines the `TorrentProvider`,
  `UsenetProvider`, `WebDownloadProvider` and `AccountProvider` interfaces plus
  the `Registry` that routes between configured accounts per kind;
  `internal/debrid/torbox` and `internal/debrid/alldebrid` implement them
- **Compat shims:** `internal/qbittorrent` and `internal/sabnzbd` each translate a
  real \*arr download-client protocol onto the provider interfaces
- **Completed Download Handling:** `internal/importer` fetches finished downloads'
  files to local disk over plain HTTP — no FUSE, no Linux-only mount
- **Native API + UI:** `internal/api` (`/api/v1`) backs `web/`, a React (Vite)
  single-page app embedded into the binary via `go:embed`
- **Default port:** `7846` · **License:** GPL-3.0

## Security

- **API key** on the native API and both compat shims — what Sonarr/Radarr and
  scripts use, since they can't do cookie logins.
- **Login accounts are mandatory for the web UI**, with `admin` and `member`
  roles; there's no API-key-only way into the dashboard. A fresh instance shows
  a first-run setup wizard to claim it. Every `/api/v1/settings/*` endpoint is
  admin-only, with one exception: any account may change its own password.
- **Built-in HTTPS** (`tls_enabled`) on a second port, with a self-signed
  certificate generated on first need, or point it at a real cert/key pair. The
  plain-HTTP listener keeps running unchanged either way. A TLS reverse proxy
  in front is still perfectly reasonable.
- **Know what a `member` account grants.** Members are refused every endpoint
  that could reveal a provider credential — but a provider download link
  carries the API key inside the URL (TorBox authorizes its CDN that way, and
  the link is useless without it), and resolving those links is exactly what
  the member tier is for. So a member account is effectively trusted with the
  provider account behind it. See
  [Direct file downloads](docs/api.md#direct-file-downloads).

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
