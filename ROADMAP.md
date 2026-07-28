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
- **Retry/backoff** (added as a follow-on): a fetch failure no longer retries on
  every tick forever with no limit — `Importer.handleFailure` schedules the next
  attempt with exponential backoff (`retry_count`/`next_retry_at` on the row,
  capped at one hour between attempts) and gives up after `import_max_retries`
  (default 5), moving the download to `error` instead. Surfaced on
  `GET /api/v1/downloads/{id}` and in the UI's detail view. Verified with real
  timing: drove three simulated failures and watched the wait grow each time,
  and separately confirmed a row stops retrying and lands in `error` once the
  limit is hit.
- **Proactive status refresh** (added as a follow-on): status sync from provider
  to local state used to happen *only* reactively — when an *arr app polled
  `GET /api/v2/torrents/info` or `mode=queue`. Watching only the native API/web
  UI (neither touches a provider) could leave a finished download looking stuck
  at "queued" indefinitely. `Importer.refreshStatuses` now runs this same sync
  proactively on every tick for both kinds, so a download is picked up within
  one `import_interval_seconds` tick of the provider reflecting it, regardless
  of whether anything is polling. The sync logic itself (`RefreshFromProvider`,
  `LocalStateFromProvider`) moved from being duplicated per compat shim to one
  shared home in `internal/database`, so all three callers agree on how a
  provider's state maps to AcerviNode's own. Verified live end to end with zero
  *arr-shim polling: added a real magnet, watched only the read-only native API,
  and saw it reach `ready_for_import` entirely on the importer's own tick — also
  confirmed via direct TorBox API calls that the *remaining* delay is TorBox's
  own `mylist` taking a few minutes to index a brand-new torrent, not anything
  on AcerviNode's side.

## Phase 3 — Native API & UI ✅

- Versioned REST API (`/api/v1`) beyond health/status: `GET /downloads`,
  `GET /downloads/{id}` (detail + files), `DELETE /downloads/{id}` — kind-agnostic,
  backed by the same `database`/`debrid` layers everything else uses
- Embedded web UI: React 19 + Vite, matching LibriNode's own stack exactly, built
  into `web/dist` and embedded into the binary via `go:embed` (`web/webui.go`) — one
  binary, one port, SPA-fallback routing
- Two dashboard views (Downloads, Settings): downloads table (name, protocol,
  category, state badge, progress bar, size, added-when, delete), provider status
  badges — deliberately kept small rather than a sprawling multi-page app
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
- **Per-download detail view** (added as a follow-on): clicking a row in the
  downloads table opens a panel with full metadata (protocol, provider, hash,
  save path, size, added/updated/completed timestamps, retry status) and the
  file list — `DownloadDetail.tsx`, built entirely on the `files` array
  `GET /api/v1/downloads/{id}` already returned, so this was pure frontend work
- **General settings + live API key regeneration** (added as a follow-on): the
  Settings tab now shows AcerviNode's own configuration (port, data/download
  dirs, log level, import settings) and its own API key — copyable from the UI
  instead of digging through server logs or `config.yaml` — plus a "Regenerate
  API key" button. Required a small refactor: the native API and both compat
  shims previously each captured a static copy of `cfg.APIKey` at startup: they
  now all check a shared `apiKeySource`/`Settings.APIKey()` live on every
  request (the same `liveSettings` instance already used for the TorBox key),
  so a regenerated key takes effect everywhere at once, no restart. Verified
  live and in an integration test: the old key stops authenticating the native
  API *and* the qBittorrent shim's login immediately after regenerating, and
  the new key works on both right away
- **Add downloads directly** (added as a follow-on): `POST
  /api/v1/downloads/torrent` and `/usenet` add a magnet/`.torrent`/NZB
  URL/`.nzb` file without needing Sonarr/Radarr or faking being one against a
  compat shim — the web UI's new "+ Add" button uses this. Server struct's
  provider fields widened from a delete-only interface to
  `torrentAdder`/`usenetAdder` (add, status, delete); no change needed in
  `cmd/acervinode` since the Dynamic\*Provider wrappers already satisfied the
  wider shape. Found and fixed a real bug during manual verification: TorBox
  dedupes by content and can hand back a `torrent_id` already tracked locally
  (e.g. re-adding an already-cached magnet), which tripped the `(provider,
  provider_download_id)` UNIQUE constraint as a raw 500 — added
  `database.GetDownloadByProviderID` and now return the existing row (`200`)
  instead of attempting a duplicate insert. Verified live against the real
  TorBox account: the exact failing scenario now returns the existing tracked
  download cleanly. A second real bug turned up moments later from an actual
  NZB upload through the web UI: TorBox's `createusenetdownload` sends
  `usenetdownload_id` back as a JSON number, not the string the official SDK's
  docs describe — see [Providers](docs/providers.md#torbox-internaldebridtorbox).
- **Status-update speed investigation** (follow-on): a user report that a
  comparable debrid client showed status updates faster than AcerviNode led
  to actually cloning that client's source (RDT-Client) rather than
  speculating. Ruled out: `reannounce` and TorBox's undocumented "Request
  Update On Torrent" mechanism are unrelated things (confirmed against the
  SDK docs); `GetHashInfoAsync`/`GetIdInfoAsync` turned out to be thin
  wrappers around the same `mylist` endpoint AcerviNode already calls, not a
  faster path. Found one real, narrow gap: RDT-Client also polls
  `queued/getqueued`, a separate pre-processing queue AcerviNode never
  checked — fixed, see Providers doc. The broader "faster" observation itself
  most likely traced to TorBox (or the network path to it) genuinely being
  slow at that moment — the WSL instance's own log showed sustained real
  `mylist` timeouts around the same time, unrelated to any AcerviNode code.
- **Settings expansion** (follow-on): general config editable live
  (`download_dir`/`log_level`/`import_interval_seconds`/`import_max_retries`
  apply with no restart; `port`/`data_dir` persist but need one), a real
  TorBox connection test, and category visibility/management for both compat
  shims — see [Configuration](docs/configuration.md) and
  [API](docs/api.md). Surfaced a pre-existing bug along the way: `log_level`
  was validated at startup but never actually wired into anything — the
  default logger always ran at its own default level regardless of what
  config said. Fixed as part of making it live-editable; the log line format
  changed as a visible side effect (see CHANGELOG). Also caught a real bug
  in review before it shipped: `GeneralUpdate`'s JSON struct tags were
  missing entirely, so every field except `port` would have silently
  decoded to its zero value on every real request — a test asserting the
  decoded values matched what was sent caught it immediately.
- **Manual retry** (follow-on): a download that gave up after exhausting
  `import_max_retries` had no way back except deleting and re-adding it.
  `POST /api/v1/downloads/{id}/retry` + a "Retry" button in the web UI's
  detail view. Verified live against a real download that had failed on an
  actual TorBox rate limit (`429`) — retrying reset it and the importer
  picked it up on its very next tick.
- **Manual re-add** (immediate follow-on to the above): retry alone can't
  help when the *original* provider-side download is gone, not just
  temporarily unfetchable — confirmed as a real, live scenario minutes
  earlier when the exact download used to verify retry failed again with
  "not found" after TorBox's own list stopped tracking it. Added a `source`
  column (the original magnet/NZB URL, link-adds only) and `POST
  /api/v1/downloads/{id}/readd`, which resubmits it as a brand new add and
  repoints the local row. Guards against the fresh add deduping back to a
  different already-tracked row (409, no mutation) rather than corrupting
  either — verified live, since re-adding a well-cached test magnet did
  exactly that on the real account.
- **Manual download links**: `GET /api/v1/downloads/{id}/files/{fileId}/link`
  resolves a direct provider-hosted URL for one file, so it can be
  downloaded straight through a browser without AcerviNode fetching it to
  local disk at all. Building it required a real, working file list first —
  which surfaced a genuine, previously-unnoticed bug: `GET
  /api/v1/downloads/{id}`'s `files` array had been `[]` this entire session,
  every single time, because the local `download_files` table it read from
  was defined but never actually populated by anything. Fixed by querying
  the provider live instead (matching `internal/qbittorrent`'s own file
  listing), rather than fixing the dead local cache. Verified live twice: the
  files bug fix (a real completed download's file list went from `[]` to all
  3 real files) and the link itself (resolved a real TorBox CDN URL and
  confirmed via `curl -I` that its `Content-Length` matched the file's known
  size exactly).
- **Batch/zip download** (immediate follow-on): a per-row "Download all"
  button in the downloads table resolves and opens every file in a download
  individually — no new endpoint needed, just looping the per-file `link`
  call from the previous item. Zip was explicitly scoped out as the
  *default* per user feedback ("zip should be optional opt in... should
  just batch download the files") and added instead as a separate opt-in:
  `GET /api/v1/downloads/{id}/zip-link` + a "Download all (zip)" button in
  the detail view. Discovered TorBox supports this via an undocumented
  `zip_link=true` parameter on the same `requestdl` endpoint (not in the
  SDK or public docs anywhere) by testing directly against the real
  account, then confirmed live that the resulting URL serves a real
  `.zip` with the correct `Content-Type` and total size. The usenet side of
  this (`RequestUsenetZipDownloadLink`) mirrors the same shape but isn't
  independently confirmed — every usenet download on the test account had
  expired from `mylist` by the time this shipped, leaving nothing live to
  test it against.
- **Batch download writes to a folder** (immediate follow-on): per user
  feedback that opening one browser tab per file was unwieldy, "Download
  all" now streams every file straight into a local folder the user picks
  (`window.showDirectoryPicker()` + `FileSystemWritableFileStream`,
  `web/src/fsAccess.ts`), instead of opening a tab per file. Chromium-only
  API — Firefox/Safari fall back to the original one-tab-per-file behavior.
  Confirmed via `curl -sI -H 'Origin: ...'` against a resolved TorBox CDN
  URL that `access-control-allow-origin` echoes the request's Origin, which
  is what makes fetching those URLs from the web UI's own JS legal. Purely
  client-side browser behavior requiring a real user click and OS folder
  picker, so — unlike the rest of this session's features — it can't be
  verified headlessly via `curl`; confirmed only that the app serves the
  updated bundle and builds/tests pass clean.
- 💡 **Streamline the download UX**: four download paths now exist (per-file
  link, per-row "download all" individually/to-folder, per-download zip),
  added incrementally as separate follow-ons rather than designed together.
  Needs a pass to consolidate them into one coherent, discoverable flow (e.g.
  a single "Download" control with a small set of clear choices instead of
  several separate buttons scattered across the table row and detail view),
  and to make the folder-write path (currently Chromium-only) feel first-class
  rather than a fallback-having bolt-on. Partial progress: a client-side
  "default 'Download all' behavior" preference (Settings → Downloads) now
  lets the per-row button default to either individual files or zip, so at
  least the row's *default* action matches what a given user actually wants
  — the full consolidation into one control is still outstanding.
- **Settings surfaced/reorganized** (follow-on, from user feedback that
  Settings should expose everything that makes sense to expose): audited
  every `config.yaml` field against the Settings UI and found all of them
  already surfaced — nothing was hidden. Added two settings that didn't
  exist as config before: per-category save-path overrides
  (`category_paths`, Settings → Categories → "Save path overrides") and the
  download-mode preference above. Explicitly kept as sections rather than
  reorganizing into tabs — three-and-a-bit sections still fits one screen
  without much scrolling; revisit if Settings keeps growing.

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
