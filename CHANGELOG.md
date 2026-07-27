# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- Native API's `GET /api/v1/downloads[/{id}]` field `kind` is now `protocol`
  (`torrent`/`usenet`) — reads better to API consumers; the internal Go type
  stays `database.Kind` (matches `reflect.Kind`'s naming, avoids clashing with
  Go's `type` keyword). Frontend (`Download.protocol`) and the downloads table's
  column header updated to match.

### Added

- `internal/importer`: fetch failures now back off exponentially
  (`retry_count`/`next_retry_at` on the `downloads` row, base
  `import_interval_seconds`, capped at one hour) and give up after
  `import_max_retries` (default 5, new config key) instead of retrying forever
  on every tick. Surfaced on `GET /api/v1/downloads[/{id}]` and in the web UI.
- Web UI: a per-download detail view (`DownloadDetail.tsx`) — click a row to see
  full metadata, retry status, and the file list, backed by the `files` array
  `GET /api/v1/downloads/{id}` already returned
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
- `internal/debrid`'s `DynamicTorrentProvider`/`DynamicUsenetProvider`: delegate to
  a swappable inner provider, returning `debrid.ErrNoProvider` until one is set
- `internal/config.Save`: persists a config back to `config.yaml` (0600)
- `internal/api`: `GET /api/v1/settings/providers`, `PUT
  /api/v1/settings/providers/torbox` — set/replace the TorBox key live, no
  restart; the web UI's new Settings tab uses this
- Both compat shims are now always mounted (previously only when a provider was
  configured at startup), which is what makes setting a key through the
  settings API — not just at startup — actually take effect

### Fixed

- `internal/qbittorrent` and `internal/sabnzbd` no longer report a download as
  fully complete the instant the provider says so — only once
  `internal/importer` has actually fetched the files to local disk
  (`ready_for_import`), matching what Sonarr's import step actually needs
- TorBox's `mylist`/`usenet/mylist` responses are now requested with
  `bypass_cache=true` — without it, TorBox serves up to a 600-second-stale cache,
  making freshly added downloads invisible to polling
- `internal/database.UpdateDownloadStatus` now also backfills `size_bytes`, and
  both compat shims' `refreshFromProvider` no longer skip an update when only
  size changed — a magnet/NZB-URL-only add starts with `size_bytes=0` (neither
  carries size info), and it was staying 0 forever once state/progress settled,
  even though the provider had a real value all along. Found by pushing a real
  download through a running instance and watching the API report `size_bytes: 0`
  for a fully completed, correctly-downloaded file
- `internal/api`'s `NewServer` no longer lets a `nil` providers slice marshal to
  JSON `null` (found while manually verifying the new web UI — its
  `providers.length` check would have thrown on that)
- README/docs release and CI badges/links pointed at the Go module's vanity
  import path (`github.com/acervinode/acervinode`) instead of the actual repo
  (`github.com/obstruct-exit-emit/AcerviNode`) — the release badge and the
  documented `git clone` command were both broken
