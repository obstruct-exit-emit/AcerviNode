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

### Fixed

- `internal/qbittorrent` and `internal/sabnzbd` no longer report a download as
  fully complete the instant the provider says so — only once
  `internal/importer` has actually fetched the files to local disk
  (`ready_for_import`), matching what Sonarr's import step actually needs
- TorBox's `mylist`/`usenet/mylist` responses are now requested with
  `bypass_cache=true` — without it, TorBox serves up to a 600-second-stale cache,
  making freshly added downloads invisible to polling
