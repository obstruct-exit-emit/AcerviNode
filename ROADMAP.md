# 📦 AcerviNode Roadmap

Where the project has been and where it's going. Phases 0–3 and 5–7 are
complete; Phase 4 (more debrid providers) is blocked for now. The
fine-grained record of every change lives in the [CHANGELOG](CHANGELOG.md).

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
| [6 — Full QA pass](#phase-6--full-qa-pass-) | Systematic review + live testing of every existing ability; 3 real bugs found and fixed | ✅ |
| [7 — Managed vs. Manual downloads](#phase-7--managed-vs-manual-downloads-) | Split the web UI into two tabs by how a download was added, plus discovering items added directly through TorBox | ✅ |

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
  [Providers](docs/providers.md#completed-download-handling-internalimporter).
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
- **Concurrent download limit + fetch timeout** (immediate follow-on, from
  the same settings conversation): `internal/importer` previously fetched
  provider_completed downloads strictly one at a time with a hardcoded
  10-minute-per-file timeout, neither configurable. Real motivation, not
  hypothetical: this same session's WSL log already showed live TorBox `rate
  limit exceeded (status 429)` errors from unthrottled adds. Added
  `max_concurrent_downloads` (bounded worker-pool concurrency in `Tick`, via
  a semaphore) and `import_fetch_timeout_seconds` (a per-request `context`
  deadline, replacing the old fixed `http.Client.Timeout` so it can change
  live) — both in Settings → General alongside the other importer knobs,
  both apply immediately. Verified with tests that force genuine overlap
  (not just serial timing luck) and genuine timeout enforcement (a CDN that
  never responds), not just that the config values round-trip.

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

## Phase 6 — Full QA pass ✅

A direct request to audit every existing ability and setting and fix whatever
turned up, rather than a specific bug report — a systematic code review of
every package plus live testing against the real WSL instance/TorBox account.
Found and fixed three real, independent bugs (full detail in the
[CHANGELOG](CHANGELOG.md)):

- **SABnzbd shim had no delete support at all** — `mode=queue`/`history` never
  handled `name=delete`; the qBittorrent shim's delete always worked fine,
  this gap was SABnzbd-specific. Fixed and verified live against the real
  running instance.
- **Provider ETA was silently dropped in both compat shims** — TorBox's real
  `eta` field made it as far as `debrid.DownloadStatus.ETASeconds` and no
  further; Sonarr/Radarr's queue view showed no ETA for any active download
  despite TorBox genuinely reporting one. Fixed by reading it fresh from the
  provider on every poll rather than trying to persist a fast-changing value.
- **A TorBox download the provider itself marked as failed could never reach
  local `error` state** — the state-mapping function's default case treated
  every unrecognized `download_state` (including a stalled/no-seeds torrent,
  and TorBox's own documented `"Error"` state) as "still downloading," so a
  download the provider had already given up on would show as perpetually
  downloading forever. Fixed by porting
  [decypharr](https://github.com/sirrobot01/decypharr)'s own
  production-proven mapping — the reference implementation this project
  benchmarks against — confirmed against the real account's own data
  (`"stalled (no seeds)"`, the exact raw state a real torrent on the test
  account had at the time). Surfaced and fixed a related, previously-dormant
  interaction along the way: `internal/importer`'s own retry-exhaustion
  give-up wasn't reliably distinguishable from a provider-reported error,
  which could have let a gave-up download get silently resurrected.
- Also removed one piece of confirmed-dead code
  (`database.ListDownloadsByState`) found during the review — unused
  anywhere including tests, and a footgun since it looked like it should back
  retry logic but was missing the actual backoff check.

Every fix has dedicated regression tests (unit tests using the real account's
own observed raw provider strings where relevant) and was verified live
against the running WSL instance — the SABnzbd delete and the state-mapping
fix's exact string were both confirmed against real data, not just reasoned
about. All existing abilities not touched by a fix were re-verified working
via the full test suite plus live spot checks; nothing else turned up.

## Phase 7 — Managed vs. Manual downloads ✅

Prompted directly by the user: not every download should be auto-fetched to
local disk — an *arr-added one needs to be (its import step scans `save_path`
and finds nothing otherwise), but a download added by hand is usually meant
to be browsed/grabbed on demand, TorBox's-own-web-UI-style, not silently
written to disk. Brainstormed with the user before building (naming, whether
the bucket should be convertible, backfill scope for discovery) rather than
designing it solo.

- **Two tabs, "Managed" and "Manual"**, replacing the single "Downloads" tab.
  Which bucket a download lands in is decided once, permanently, by *how* it
  was added — `database.Download.AddedVia` (`arr` or `manual`), set at
  insert time. The signal turned out to be free: an *arr app only ever talks
  to the qBittorrent/SABnzbd compat shims, and only a human (via the web
  UI's "+ Add" form) ever calls the native API's add endpoints directly —
  no new toggle needed, no ambiguity.
- **Manual downloads are never auto-fetched**: `ListDownloadsDueForRetry`
  (what feeds Completed Download Handling's fetch step) now filters to `arr`
  only. A Manual download sitting in `provider_completed` just stays there;
  the user grabs files via the same per-file/zip-link endpoints Managed
  downloads already use. Retry/Re-add are hidden for a Manual download in
  `error` state, since there's no local fetch attempt to retry.
- **Discovery**: an item added directly through TorBox's own website/app —
  not through AcerviNode at all — now shows up in Manual too, not just
  things added through AcerviNode's own "+ Add" form. `Importer
  .discoverManual` runs every tick, diffing the same provider `List()` call
  `refreshStatuses` already makes against what's locally tracked, and
  adopts anything unmatched as `manual`.
- **Backfill scope, explicitly decided with the user rather than assumed**:
  the very first time discovery runs for a given provider+kind, nothing is
  adopted — every currently-unmatched item is instead recorded into a new
  `discovery_baseline` table and permanently ignored, so shipping this
  doesn't flood the Manual tab with an account's entire pre-existing
  history. Only items that show up *afterward* are ever adopted — the
  user's own explicit choice ("only going forward") over the alternative of
  backfilling everything immediately.
- New migration (`0004_added_via.sql`): `downloads.added_via` defaults to
  `arr`, so every pre-existing row is correctly classified with no backfill
  step of its own, plus the `discovery_baseline`/`discovery_seeded` tables
  backing the point above.
- Covered by dedicated tests at every layer: `database` (filter behavior,
  baseline seed/lookup), `internal/importer` (first-run seeding adopts
  nothing, a later tick adopts a genuinely new item, no duplicate adoption,
  a Manual download is never auto-fetched even sitting in
  `provider_completed`), and `internal/api` (the `?added_via=` query filter).
- Immediate follow-on: removed the manual-download buttons (per-row
  "Download all", detail view's per-file "Download"/"Download all (zip)")
  from Managed rows entirely — a Managed download is already being
  auto-fetched, so they were redundant there. Manual-tab-only now; the
  underlying endpoints are unchanged, this was purely a web UI choice.
- Immediate follow-on: removed category from Manual entirely — no input in
  the "+ Add" form, no column in the table, no row in the detail view — see
  💡 **Manual categories** below.
- Immediate follow-on: the Manual tab's multi-file "Download all" no longer
  shows the folder picker on every download — the first pick is remembered
  (persisted via IndexedDB) and silently reused as long as permission is
  still live, with a "Change folder"/"Forget" control in Settings →
  Downloads. Deliberately scoped to that one multi-file path only — brainstormed
  with the user first, including the real constraint that streaming-to-disk
  requires the tab to stay open for the whole download (unlike a real
  browser download), and the conclusion that a *single* file (the per-file
  "Download" button, "Download all (zip)") is better served staying a plain
  browser download, not File System Access — it's tracked in the browser's
  own download manager and survives the tab closing, which streaming can't
  offer either way.
- Bug found live by the user: a Manual download whose provider item had
  vanished entirely (confirmed by querying the real TorBox account directly
  — genuinely gone from `mylist`, not just slow) showed the misleading "No
  files available to download yet" on click, instead of the real reason.
  Fixed: `GET /api/v1/downloads/{id}` now includes `files_error` (the
  underlying provider error, present only when the live query actually
  failed) and the web UI shows it directly. See the 💡 below for the deeper,
  deliberately-deferred half of this.
- Immediate follow-on: the row-level "Download all" button now shows a real
  progress bar (cumulative bytes across the whole batch, weighted by file
  size) while streaming to a folder, instead of a static "…" for however long
  the whole thing takes. `writeFileToDirectory` takes an optional per-chunk
  callback instead of a blind `pipeTo`. Scoped to that one path on purpose —
  the tab-per-file fallback and the per-file/zip buttons already get real
  progress for free from the browser's own download manager once handed off.
- Immediate follow-on, prompted by the user asking for a custom-button
  `beforeunload` prompt: that specific ask isn't implementable (every modern
  browser strips all customization from the native close/navigate dialog on
  purpose), so reframed around the real underlying goal instead — a shared
  "Downloads" popup window (`web/src/components/DownloadWindow.tsx`) that a
  streamed multi-file "Download all" hands its whole batch off to over
  `postMessage`, so the download keeps running even if the main tab closes.
  One window gathers every batch rather than one popup per download. See the
  [CHANGELOG](CHANGELOG.md) for the full design and its several
  not-yet-verified-by-a-real-click caveats.
- Immediate follow-on: fixed live, right after the popup feature's first
  real use — every file failed silently with no folder prompt. Root cause:
  a directory handle's write permission is checked per browsing context,
  not just per origin, so the popup's postMessage-cloned handle wasn't
  actually granted access there even though the main window already had
  it. The popup now asks for its own permission (a real "Grant folder
  access" button click) before writing, and surfaces the real per-file
  error instead of a bare failure count.
- Immediate follow-on: a confirm dialog (`DownloadOptionsDialog.tsx`) now
  shows before a streamed "Download all" starts, showing the destination
  folder (with a change-folder control) and a checkbox for whether to use
  the Downloads window at all, rather than both being decided silently.
- Docs cleanup: found and fixed a broken cross-reference anchor
  (`#completed-download-handling` vs. the real
  `#completed-download-handling-internalimporter`) affecting 7 links across
  6 files, using a link/anchor checker written for the occasion rather than
  by inspection — worth remembering as a technique for a future docs pass.
  Also updated README.md/quickstart.md's web UI descriptions, stale since
  before the Managed/Manual split.

💡 **Manual categories**: brainstormed with the user and deliberately left
out for now. Category only drives real behavior for Managed downloads (it's
what `category_paths` save-path overrides key on); for Manual it would be a
purely cosmetic label, and Manual downloads are meant to mirror TorBox's own
web UI, which has no categorization concept at all — adding one would be a
real divergence, not a faithful mirror. Revisit if the Manual tab actually
becomes unwieldy to navigate once discovery has been adopting things for a
while — a simple client-side search/filter-by-name is the lighter-weight
alternative worth trying first, before reaching for full categorization
(which would also need an edit-after-the-fact story, since a discovered
download starts with no category and TorBox gives it none to inherit).

💡 **Proactively detect a vanished Manual download**: right now nothing
notices a Manual download's provider item is gone until the user actually
clicks download and hits `files_error` live — `RefreshFromProvider` only
updates rows it finds a matching status for, and a Manual download is never
in `internal/importer`'s fetch-retry path (the thing that catches this for a
Managed download within a few ticks, since the fetch attempt itself fails
and eventually gives up with a clear reason). Not fixed proactively yet on
purpose: doing it safely needs a debounce — mark a row as gone only after
it's been missing from a *successful* provider listing for several
consecutive ticks, not the first miss, since a brand-new add can legitimately
take a while to be indexed anywhere (mylist or the pre-processing queue) and
a single-miss rule would wrongly flag it "gone" while it's still just new.
That's real design work (a missing-since timestamp or counter, tuned against
how long TorBox actually takes to index something), not a one-line fix.
