# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Repo bootstrap: README, LICENSE (GPL-3.0), ROADMAP, CI workflow, docs skeleton
- `internal/config` and `internal/database` (SQLite, embedded migrations)
- `internal/debrid`: `TorrentProvider` / `UsenetProvider` interfaces, TorBox
  implementation of both
- `internal/qbittorrent`: qBittorrent Web API compat shim
- `internal/sabnzbd`: SABnzbd API compat shim
- `cmd/acervinode`: entrypoint wiring config, database, TorBox provider, and both
  compat shims behind an HTTP server on port 7846
- `internal/importer`: Completed Download Handling — fetches a provider-completed
  download's files over plain HTTP to `save_path`/`download_dir`, so *arr apps'
  import step has real files to find; `download_dir` and
  `import_interval_seconds` config keys
- `internal/api`: `GET /api/v1/downloads`, `GET /api/v1/downloads/{id}`,
  `DELETE /api/v1/downloads/{id}` — kind-agnostic download listing/management
- `web/`: React 19 + Vite single-page dashboard (downloads table, provider status,
  API-key gate), embedded into the binary via `go:embed` (`web/webui.go`), served
  at `/` alongside the API and both compat shims
- `packaging/acervinode.service`: hardened systemd unit (verified with
  `systemd-analyze verify`), and `.github/workflows/release.yml`, which builds the
  frontend and cross-compiles version-stamped Linux amd64/arm64 binaries on `v*`
  tags, attaching each to a GitHub release bundled with the unit file
- `main.version` is now stamped at build time via `-ldflags`, defaulting to
  `0.0.0-dev` for a plain `go build`

### Fixed

- `internal/qbittorrent` and `internal/sabnzbd` no longer report a download as
  fully complete the instant the provider says so — only once
  `internal/importer` has actually fetched the files to local disk
  (`ready_for_import`), matching what Sonarr's import step actually needs
- TorBox's `mylist`/`usenet/mylist` responses are now requested with
  `bypass_cache=true` — without it, TorBox serves up to a 600-second-stale cache,
  making freshly added downloads invisible to polling
- `internal/api`'s `NewServer` no longer lets a `nil` providers slice marshal to
  JSON `null` (found while manually verifying the new web UI — its
  `providers.length` check would have thrown on that)
- README/docs release and CI badges/links pointed at the Go module's vanity
  import path (`github.com/acervinode/acervinode`) instead of the actual repo
  (`github.com/obstruct-exit-emit/AcerviNode`) — the release badge and the
  documented `git clone` command were both broken
