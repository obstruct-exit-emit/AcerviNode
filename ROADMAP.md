# 📦 AcerviNode Roadmap

Where the project has been and where it's going. Phases 0–3 and 5 are complete;
Phase 4 (more debrid providers) is blocked for now. The fine-grained record of
every change lives in the [CHANGELOG](CHANGELOG.md).

**Legend:** ✅ complete · 🔄 in progress · 💡 under consideration · ⏳ blocked

## At a glance

| Phase | Scope | Status |
|---|---|---|
| [0 — Foundation](#phase-0--foundation-) | Repo, config, database, CI | ✅ |
| [1 — TorBox vertical slice](#phase-1--torbox-vertical-slice-) | TorBox provider, qBittorrent shim, SABnzbd shim | ✅ |
| [2 — Completed Download Handling](#phase-2--completed-download-handling-) | Fetch resolved files to local disk once a download is done | ✅ |
| [3 — Native API & UI](#phase-3--native-api--ui-) | Richer `/api/v1`, embedded web UI | ✅ |
| [4 — Multi-provider](#phase-4--multi-provider-) | Real-Debrid, Debrid-Link, AllDebrid, Premiumize | ⏳ |
| [5 — Hardening & release](#phase-5--hardening--release-) | systemd unit, packaged Linux binaries, release automation | ✅ |

---

## Phase 0 — Foundation ✅

- Go backend, single self-contained binary, no runtime dependencies
- SQLite (pure Go, no cgo) with an embedded migrations framework
- Config file (`config.yaml`) + env overrides
- CI building and testing on every push; GPL-3.0

## Phase 1 — TorBox vertical slice ✅

- `internal/debrid`: `TorrentProvider` and `UsenetProvider` interfaces — the seam
  that makes every later provider a pure addition, never a rewrite
- TorBox provider implementing both interfaces (torrents + usenet)
- qBittorrent-compat shim (`/api/v2/...`): auth, add, info, properties, files,
  delete, categories — enough for Sonarr/Radarr's own "Test" flow and normal use
- SABnzbd-compat shim (`/api?mode=...`): version, auth, addfile/addurl, queue,
  history — same coverage, NZB-shaped
- One `downloads` table shared by both shims, tracking every add from `queued`
  through `ready_for_import`
- Verified against the real TorBox API end to end: add → track through real state
  transitions → resolve a real CDN download link → delete, including a real
  upstream `500` on double-delete handled gracefully

## Phase 2 — Completed Download Handling ✅

The actual gap standing between "protocol-compatible demo" and a real download
client: AcerviNode could already tell Sonarr a download finished, but nothing
placed the bytes where Sonarr expects them on disk. Closed the simple way —
direct download over HTTP from the provider's resolved link, not a FUSE mount —
same mechanism a normal download client already uses, just sourced from a debrid
CDN link instead of BitTorrent/NNTP.

- `internal/importer`: background loop (matches LibriNode's own package name and
  purpose for this) that finds `provider_completed` downloads, resolves each file's
  link, and streams it to `save_path` (or a configured fallback `download_dir`)
- Local state only reaches `ready_for_import` once files are actually on disk —
  before this, both compat shims reported completion the moment the *provider*
  said done, which was too early for Sonarr's import step to actually find anything
- Size-check idempotency (skip files already fully written) rather than true
  HTTP range-resume — deliberately the simpler option for now
- Verified against the real TorBox API end to end: added a real magnet, watched it
  reach `provider_completed` (correctly still reported as "downloading" to Sonarr),
  watched `internal/importer` fetch all 3 real files (subtitle, ~263MB video,
  poster) to local disk within one tick, verified the video file's actual magic
  bytes and the subtitle's actual content, watched the state move to
  `ready_for_import`/"uploading". Along the way, found and fixed a real bug: TorBox's
  `mylist`/`usenet/mylist` endpoints are server-side cached for up to 600 seconds
  unless `bypass_cache=true` is passed — without it, a freshly added torrent was
  invisible to every poll for as long as the cache window lasted. See
  [Providers](docs/providers.md#completed-download-handling).

## Phase 3 — Native API & UI ✅

- Versioned REST API (`/api/v1`) beyond health/status: `GET /downloads`,
  `GET /downloads/{id}` (detail + files), `DELETE /downloads/{id}` — kind-agnostic,
  backed by the same `database`/`debrid` layers everything else uses
- Embedded web UI: React 19 + Vite, matching LibriNode's own stack exactly, built
  into `web/dist` and embedded into the binary via `go:embed` (`web/webui.go`) — one
  binary, one port, SPA-fallback routing
- Two dashboard views (Downloads, Settings): downloads table (name, kind, category,
  state badge, progress bar, size, added-when, delete), provider status badges —
  deliberately kept small rather than a sprawling multi-page app
- **Live settings** (added as a follow-on): `internal/debrid`'s
  `Dynamic*Provider` wrappers let a TorBox key be set or changed through the web
  UI's Settings tab (`PUT /api/v1/settings/providers/torbox`) and take effect
  immediately, with both compat shims now always mounted rather than only when a
  provider exists at startup — no restart needed either way. Persisted to
  `config.yaml` so it survives one when it does happen. Verified live: set a real
  key through the running API with zero downtime, then used the qBittorrent shim
  against the real TorBox API on the very next request
- API-key auth on the native API (the UI prompts for it once, keeps it in
  `localStorage`), matching LibriNode's convention
- Built and design-judgment-called autonomously (UI has no live feedback loop from
  the user by nature — explicitly authorized rather than deferred)
- Found and fixed a real bug during manual verification: a `nil` `providers` slice
  marshaled to JSON `null` instead of `[]`, which would have thrown in the UI's
  `providers.length` check — `NewServer` now normalizes it, with a regression test

## Phase 4 — Multi-provider ⏳

- Real-Debrid provider (`TorrentProvider` only — no native usenet service).
  Blocked: no Real-Debrid account available to verify against, so this stays
  behind phases that can actually be tested — see Phase 1's TorBox verification
  for why that matters here.
- Debrid-Link, AllDebrid, Premiumize as they become worth the maintenance cost
- Per-provider cached-availability checks where the provider supports them

## Phase 5 — Hardening & release ✅

- `packaging/acervinode.service`: a hardened systemd unit (`ProtectSystem=strict`,
  `NoNewPrivileges`, dedicated user, write access scoped to
  `/var/lib/acervinode`) — verified with `systemd-analyze verify` in a real
  systemd environment (WSL2), not just written and assumed correct
- `.github/workflows/release.yml`: tags matching `v*` build the frontend, then
  cross-compile version-stamped (`-ldflags "-X main.version=..."`) Linux
  amd64/arm64 binaries, each bundled with the systemd unit into a `.tar.gz`
  attached to a GitHub release
- Verified for real, not just reasoned about: both cross-compiled binaries
  actually run (executed the linux/amd64 build inside WSL2 — real ELF execution,
  not just a successful `go build`), and the stamped version showed up correctly
  in `/api/v1/version`
- Docker and a packaged Windows install are explicitly out of scope for now (may
  be added later) — Linux binary + systemd is the whole packaging story today
- Fixed a real, pre-existing bug found while wiring this up: README/docs release
  and CI badges/links pointed at `github.com/acervinode/acervinode` (the Go
  module's vanity import path) instead of the actual repo,
  `github.com/obstruct-exit-emit/AcerviNode` — meant the release badge and the
  `git clone` instructions were both broken
