# 📦 AcerviNode Roadmap

Where the project has been and where it's going. Phase 0 is in progress — this is
the very first vertical slice. The fine-grained record of every change lives in the
[CHANGELOG](CHANGELOG.md).

**Legend:** ✅ complete · 🔄 in progress · 💡 under consideration

## At a glance

| Phase | Scope | Status |
|---|---|---|
| [0 — Foundation](#phase-0--foundation-) | Repo, config, database, CI | 🔄 |
| [1 — TorBox vertical slice](#phase-1--torbox-vertical-slice-) | TorBox provider, qBittorrent shim, SABnzbd shim | 🔄 |
| [2 — Native API & UI](#phase-2--native-api--ui-) | Richer `/api/v1`, embedded web UI | 💡 |
| [3 — Multi-provider](#phase-3--multi-provider-) | Real-Debrid, Debrid-Link, AllDebrid, Premiumize | 💡 |
| [4 — Local import](#phase-4--local-import-) | FUSE-style Linux mount, completed-download handling | 💡 |
| [5 — Hardening & release](#phase-5--hardening--release-) | Packaging, systemd unit, Docker, release automation | 💡 |

---

## Phase 0 — Foundation 🔄

- Go backend, single self-contained binary, no runtime dependencies
- SQLite (pure Go, no cgo) with an embedded migrations framework
- Config file (`config.yaml`) + env overrides
- CI building and testing on every push; GPL-3.0

## Phase 1 — TorBox vertical slice 🔄

- `internal/debrid`: `TorrentProvider` and `UsenetProvider` interfaces — the seam
  that makes every later provider a pure addition, never a rewrite
- TorBox provider implementing both interfaces (torrents + usenet)
- qBittorrent-compat shim (`/api/v2/...`): auth, add, info, properties, files,
  delete, categories — enough for Sonarr/Radarr's own "Test" flow and normal use
- SABnzbd-compat shim (`/api?mode=...`): version, auth, addfile/addurl, queue,
  history — same coverage, NZB-shaped
- One `downloads` table shared by both shims, tracking every add from `queued`
  through `ready_for_import`

## Phase 2 — Native API & UI 💡

- Versioned REST API (`/api/v1`) beyond health/status: provider config, download
  listing/management, same API a future UI would use
- Embedded web UI (framework TBD — LibriNode uses React/Vite; likely the same for
  family consistency), one binary, one port
- API-key auth on the native API, matching LibriNode's convention

## Phase 3 — Multi-provider 💡

- Real-Debrid provider (`TorrentProvider` only — no native usenet service)
- Debrid-Link, AllDebrid, Premiumize as they become worth the maintenance cost
- Per-provider cached-availability checks where the provider supports them

## Phase 4 — Local import 💡

- FUSE-style Linux mount exposing debrid-resolved files as local paths, the way
  decypharr does, so \*arr apps can complete their normal import step
- Completed-download handling: rename/organize/cleanup once a download is ready

## Phase 5 — Hardening & release 💡

- systemd unit, packaged Linux binaries (amd64/arm64) attached to tagged releases
- Docker image (Linux) — deferred behind the mount work, since the mount is the
  part that actually needs `/dev/fuse` + `SYS_ADMIN`
- Windows builds: no committed timeline, same posture as LibriNode
