# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **Systematic code-review pass over Phases 1–6** (TorBox client, both
  compat shims, `internal/importer`, the native API), done proactively
  while the user was at work — everything shipped in this session's recent
  streak had already been re-checked; this covered the older code that
  hadn't had a fresh look since Phase 6's own QA pass. Read through every
  file in `internal/debrid/torbox`, `internal/qbittorrent`,
  `internal/sabnzbd`, `internal/importer/importer.go`, and `internal/api`
  (add/downloads/settings/server) looking specifically for logic bugs,
  race conditions, and inconsistencies between the two compat shims (which
  are supposed to mirror each other's behavior).

  **Found and fixed one real, confirmed issue**: the native API's own auth
  check (`internal/api/auth.go`) deliberately uses
  `crypto/subtle.ConstantTimeCompare` for its API-key comparison, explicitly
  documented as "matching LibriNode's credential-check convention" — but
  the qBittorrent shim's login (`password` field) and the SABnzbd shim's
  `apikey` check both used a plain `!=` string comparison instead, a timing
  side-channel the native API deliberately guards against. Brought both in
  line with the same constant-time convention. Verified live: the real key
  still authenticates correctly on the qBittorrent shim's login endpoint,
  a wrong one is still rejected.

  Everything else held up: TorBox client/provider/adapters, both compat
  shims' request handling, `internal/importer`'s concurrency and retry
  logic, and the native API's add/re-add/settings handlers. One thing
  noted but deliberately not changed — `internal/qbittorrent/torrents.go`'s
  `handleFiles` reports every file's progress as a flat 100%, unconditional
  on the torrent's own actual progress; left alone since Sonarr/Radarr only
  really consult this endpoint for file-name matching once a download is
  already complete, where that's correct anyway, and there's no live
  evidence it causes a real problem.

- **Live-account audit** (the technique that found the torrent hash-backfill
  bug earlier, applied again): checked the 4 currently-tracked downloads
  for anomalies. No new data bugs found. One inconclusive result worth
  recording rather than silently dropping: a real usenet download existed
  on the account (a chance to finally verify `RequestUsenetZipDownloadLink`,
  unverified since it was written), but it had already expired from TorBox's
  actual `mylist` (confirmed directly against TorBox's own API — 0 usenet
  items) by the time it was tested, so the zip-link call correctly errored
  with "not found" rather than confirming or refuting the zip-link path
  itself. Still unverified; watch for another live usenet download to test
  against. The expiration itself is a second, live confirmation of the
  already-documented "vanished Manual download" ROADMAP item — this time
  on a usenet download rather than a torrent — which continues to be
  handled reactively (a clear `files_error` once someone clicks in) but not
  proactively, exactly as already documented there.

- **Fixed a real bug found during a self-review pass** (done proactively
  while the user was away, specifically to catch issues in code shipped
  overnight without live testing): a file that genuinely failed partway
  through a batch, followed by the user clicking Stop, silently vanished —
  `filesDone` counts every file *attempted* (success or failure), not just
  successes, since `processBatch` only skips incrementing it when a file
  is interrupted by the abort itself. That meant three things went wrong
  together: `reportComplete` unconditionally sent an empty failed list to
  every main window whenever a batch was stopped (so the earlier real
  failure raised no alert), the popup's own render only showed the failed-
  file list for `status === 'error'`, never `'stopped'` (so it was
  invisible there too), and `retryStopped`'s `files.slice(filesDone)` slice
  treated the failed file as "done," so it would never be retried — a real
  failure, permanently and silently dropped. Fixed by: always reporting
  the true `failed` array regardless of whether the batch was stopped;
  showing the failed-file list under a `'stopped'` status too, not just
  `'error'`; and having `retryStopped` retry both the not-yet-attempted
  suffix *and* anything in the failed list, rather than only the suffix.
  Also restored a "clear the path and save to remove it" hint to the new
  Save path overrides help text, dropped by mistake during last night's
  redesign even though `CategoryPathRow` still supports it unchanged.

- **Simplified Settings' Categories section into "Save path overrides."**
  Built overnight, continuing the conversation from the placeholder-category
  removal above: once that was gone, the remaining category list's only
  real job was being a prerequisite — a save-path override could only be
  set for a category already "known" (declared by Sonarr/Radarr, or
  manually pre-added), a backwards, confusing gate for a feature the
  backend never actually required (`PUT /api/v1/settings/categories/path`
  already accepted any category name directly — confirmed by reading
  `SetCategoryPath`, which does no existence check at all). The section is
  now just "Save path overrides": existing overrides listed and editable
  as before, plus a two-field "Add override" form (category name + path)
  that works for any name, declared or not. The old two-column Torrent/
  Usenet category list and its separate "+ Add category" form are gone;
  what Sonarr/Radarr have actually declared, if anything, now shows as one
  small line at the bottom instead of its own management UI. The now-dead
  frontend `addCategory` API helper and its CSS were removed along with it
  — the backend's `POST /api/v1/settings/categories` endpoint is untouched
  and still callable directly, just no longer surfaced in the web UI.
  Verified live via the API directly (the part that can be checked without
  a browser): set a save-path override for a category name that had never
  been seen before, confirmed it saved and reported back correctly, then
  cleared it — exactly the mechanism the new "Add override" form relies on.
  **Not verified**: how the new layout actually looks — built and deployed
  overnight without the ability to click through the real UI, so this
  needs a look once you're back.
- **Removed the seeded "AcerviNode" placeholder category.** The user asked
  whether it overrode Sonarr/Radarr's real category — confirmed it never
  did (`internal/importer.categoryPath` is a plain exact-string lookup, no
  fallback), but it was inert clutter in the Settings UI's category list
  either way. Both compat shims' `categoryStore` used to seed one default
  entry (`"AcerviNode"`) purely so the list/`/api/v2/torrents/categories`
  response was never empty before Sonarr/Radarr had declared anything; now
  starts empty and fills in with real categories as Sonarr/Radarr actually
  declare them, same as a real qBittorrent/SABnzbd install. The web UI
  already rendered an empty category list as "None yet", so no frontend
  change was needed. SABnzbd's protocol-mandated `"*"` category (a real
  requirement, not the placeholder) is untouched.
- **Stop, retry-failed, and dismiss controls in the Downloads popup**
  (`web/src/components/DownloadWindow.tsx`): each batch's header now shows
  a small icon button — ⏹ Stop while it's actively downloading, ✕ Dismiss
  once it's done, failed, stopped, or waiting on a permission grant.
  Stopping aborts cleanly via a per-batch `AbortController` wired into the
  `fetch()` call, rather than just letting the transfer run to completion —
  a deliberate Stop is reported to every main window as a no-op completion
  (empty failed list) rather than a failure, so nothing shows a scary "N
  files failed" alert for it. The partially-written file is deliberately
  left on disk rather than deleted: it costs nothing extra now and doubles
  as the starting point for the real pause/resume feature already scoped
  on the roadmap. A new "stopped" status (shown in muted text, distinct
  from the red "error" state) reflects how many files actually finished
  before the stop.

  A failed batch gets a "↻ Retry failed (N)" button below its per-file
  error list — rebuilds a batch containing just the files that didn't make
  it (matched by path against the last add-batch message, kept around for
  exactly this) and reprocesses it under the same download id, going
  through the same permission check as a fresh add rather than assuming
  access is still granted.

  Fixed a real, previously-documented bug as a side effect: the
  "already processing" guard (`processing` — prevents handling the same
  add-batch twice) was never cleared once a batch finished, so re-clicking
  "Download all" for anything already tracked in the popup silently did
  nothing. It's now cleared the moment a batch finishes, however it
  finishes (done, error, or stopped), so re-downloading — from the main
  app or the popup's own Retry button — always works.
  - Immediate follow-on, requested by the user after trying it live: a
    stopped batch gets its own "↻ Retry" icon button too, in the header
    next to Dismiss — `processBatch` always processes files in array order
    and breaks the instant it's aborted, so `filesDone` at that moment is
    exactly how many files at the start of the array actually finished.
    Retrying a stop reprocesses everything from there on (`files.slice
    (filesDone)`), including the one interrupted mid-write, redone from
    scratch rather than resumed byte-for-byte (no offset-tracking yet —
    that's the pause/resume roadmap item).
- **Fixed a real bug found live: some torrents never got a real hash.** The
  user noticed some Manual torrents had a hash and some didn't and asked
  why. Traced against their real TorBox account: a torrent's provider-side
  listing is provisional right after it's added — a placeholder name (the
  raw uploaded filename) and no hash yet — until TorBox finishes indexing
  it. `internal/importer`'s discovery step (`discoverManual`) captures
  whatever the provider reports at that exact moment; if that happened to
  be mid-indexing, the incomplete snapshot was stuck permanently, since
  `RefreshFromProvider` — the function that runs on every later poll — only
  ever updated `state`/`progress`/`size_bytes`/`error_message`, never
  `hash` or `name`. Confirmed directly against TorBox's raw API: two of the
  user's adopted torrents had an empty hash locally while TorBox's own
  `mylist` already had the real one. `database.RefreshFromProvider` now
  backfills `hash` and `name` from the provider whenever the local hash is
  still empty — deliberately gated on that (never overwrites a hash a row
  already has) and deliberately unconditional on state (runs even for rows
  the state-transition guards elsewhere in the function would otherwise
  skip). Two new tests: the backfill firing for an empty-hash row, and
  proving it leaves an already-hashed row untouched.
- **Documented a real browser restriction found live**: Chrome refuses to
  grant *any* site read/write access to certain "well-known" folders
  directly — Desktop, Documents, Downloads, Pictures, Music, Videos, plus
  actual OS directories — showing a generic "can't open this folder
  because it contains system files" dialog even though a personal
  Downloads folder obviously has no such thing. Deliberate anti-abuse
  policy (stops a site from getting broad standing access to everywhere a
  user's files land), enforced entirely inside the browser's own picker
  dialog before any AcerviNode code runs — nothing to catch or work around
  in JS. Added a note in both `DownloadOptionsDialog.tsx` and Settings'
  folder picker explaining it and the fix: pick a subfolder inside the
  blocked folder (e.g. `Downloads/AcerviNode`) instead of the folder
  itself, which isn't restricted.
- **Per-row download progress no longer shares one global slot.** Found
  live right after two downloads genuinely overlapped: the row progress
  indicator visibly flickered/glitched, jumping between two different
  downloads' numbers. Root cause: `downloadingAllId`/`downloadProgress`
  were single shared values, correct back when only one row could ever be
  mid-download at a time — no longer true once the Downloads popup could
  run several batches at once (see the popup entries below). Replaced with
  `busyIds` (a `Set<string>`) and `downloadProgress` keyed by download id,
  in both `App.tsx` and `DownloadsTable.tsx`, so each row now reads only
  its own entry and stops fighting over one value with every other
  in-flight row.
- **The Downloads popup tries to bring itself to the front when a download
  is added.** `window.focus()` called on itself from a background
  BroadcastChannel handler (no direct user gesture in that context) is
  exactly the "popup keeps stealing focus" pattern Chromium deliberately
  blocks — confirmed live: it does not actually work. The tab title change
  ("🔴 New download — …" while unfocused, clearing on refocus) is the part
  that's guaranteed to work regardless, since it needs no permission and
  can't be blocked the same way.
- **Fixed a real bug found live: the Downloads popup window could split
  across two separate windows.** Reported by the user opening two
  downloads that landed one each in two different "Downloads" popups
  instead of the same shared one. Root cause: `window.open(url, name)`'s
  "reuse an existing window with this name" behavior only works within the
  same browsing-context group — two independently-opened AcerviNode tabs
  (not one opened from the other, e.g. two separate browser windows) can
  land in different groups, so each one's `openDownloadWindow()` call
  spawned its own popup, and every subsequent download from either tab kept
  going to whichever popup that tab happened to know about. Direct
  `window.postMessage`/`window.opener` coordination has no way to see past
  that boundary at all. Replaced with two Chromium primitives that don't
  care about browsing-context topology: every main window and the popup now
  talk over a shared `BroadcastChannel('acervinode-downloads')`
  (`downloadWindowProtocol.ts`) instead of direct window references, and the
  popup itself claims a singleton `navigator.locks` lock
  (`acervinode-downloads-singleton`) on load — if a second physical popup
  ever does open (the browsing-context-group case can still make
  `window.open()` create one), it loses the lock race, shows "Another
  Downloads window is already open — safe to close", and never touches a
  batch. `sendBatchToDownloadWindow`/`listenForDownloadWindowMessages` no
  longer take a `Window` reference at all — they ping-and-wait for a
  `popup-ready` reply and broadcast from there, so whichever popup actually
  holds the lock receives every batch and progress/completion report,
  regardless of which tab triggered it or which popup object that tab's own
  `window.open()` call happened to return.
  - Immediate follow-on: a duplicate that loses the lock race now calls
    `window.close()` on itself right away, instead of only offering a
    manual "Close this window" button. `window.close()` silently does
    nothing in some browsers/settings even for a window opened by script
    (no error thrown either way), so the explanatory text and the manual
    button both stay as the fallback if the automatic close doesn't work.
- **Confirm dialog before a streamed "Download all" starts**
  (`web/src/components/DownloadOptionsDialog.tsx`): clicking the row button
  used to immediately start fetching — folder and Downloads-popup hand-off
  were both decided silently underneath it (remembered default folder,
  always try the popup). Now, in any browser that supports streaming to a
  folder, it opens a small dialog first showing the folder it's about to
  use (with a "Change folder" button, same picker as Settings → Downloads)
  and a checkbox for whether to send the batch to the shared Downloads
  window at all — unchecking it streams in the current tab instead, for
  someone who'd rather not have that window open. Nothing starts until
  "Download" is clicked. Split the old combined `handleDownloadAllIndividual`
  into two: it now only covers the plain tab-per-file fallback (browsers
  without File System Access — nothing to choose there, so it never opens
  the dialog), and a new `startStreamedDownload(d, opts)` — triggered by the
  dialog's confirm — takes the already-chosen folder and Downloads-window
  choice directly instead of resolving either itself. `openDownloadWindow()`
  still has to be the confirm button's very first synchronous statement
  (before any `await`) for the same user-activation reason as before — the
  dialog's "Change folder" button already ran its own picker earlier, as
  its own separate gesture, so by the time "Download" is clicked the folder
  is already resolved and only the popup itself still needs a live gesture.
- **Shared "Downloads" popup window for streamed multi-file downloads**: a
  streamed-to-folder "Download all" (individual files) previously died the
  moment its tab was closed or navigated away from — there's no browser-level
  background process carrying a File System Access stream the way there is
  for a native browser download. The underlying request was for a real
  three-option `beforeunload` prompt ("stay" / "abort and leave" / "open a
  window for downloads"), but that's not implementable: every modern browser
  deliberately strips all customization from the native close/navigate
  dialog (anti-abuse restriction, no exceptions), so a custom-button
  `beforeunload` prompt simply doesn't exist as an API surface. Reframed
  instead around what the user actually wanted — not losing the download —
  via a small, separate "Downloads" popup window
  (`web/src/components/DownloadWindow.tsx`, served from the same bundle via
  a `?popup=downloads` query param checked in `main.tsx`, so no second Vite
  build target or Go route was needed) that the row button hands the whole
  batch off to over `postMessage` (`web/src/downloadWindowProtocol.ts`) —
  including the `FileSystemDirectoryHandle` itself, which travels as an
  ordinary structured-clone property. Once the popup owns a batch, it keeps
  fetching and streaming files to disk independent of whatever happens to the
  main tab afterward. One shared window gathers every batch rather than
  spawning one popup per download (`openDownloadWindow()` in
  `web/src/fsAccess.ts` reuses an already-open popup via `window.open`'s
  named-window behavior); a ready-handshake (`popup-ready` message + a
  `ready` promise the sender awaits) avoids a race where a batch is posted
  before the popup's own listener has registered. `handleDownloadAllIndividual`
  now opens/focuses the popup synchronously (no `await` before it, same
  user-activation requirement as the folder picker) before resolving the
  download directory, hands off to the popup if both succeed, and falls back
  to the previous in-tab streaming loop if the popup was blocked, or the
  tab-per-file fallback if this browser doesn't support File System Access at
  all. **Known caveats, none of them verified by a real click in a real
  browser yet**: (1) `FileSystemDirectoryHandle`'s cross-window
  structured-clone transfer is asserted from spec knowledge, not observed;
  (2) the ordering that's supposed to preserve user-activation for both
  `window.open()` and the folder picker within one click is reasoned about,
  not measured; (3) the popup can pop up/focus even when the user then
  cancels the folder picker, since opening it has to happen before we know
  whether picking will succeed — deliberately not auto-closed on cancel,
  since it may already be gathering other, unrelated downloads; (4) a
  completed or failed batch has no way to be dismissed from the popup's list
  — it stays until the popup itself is closed; (5) re-triggering "Download
  all" for the same download a second time (e.g. to retry failed files)
  while its first batch is still tracked in the popup's `processing` set is
  silently a no-op there. The main window's own row-level progress
  indicator also only ever reflects the most recently active batch if more
  than one is running in the popup at once — the popup's own list is the
  real, always-accurate source of truth for concurrent downloads regardless.
  - **Fixed live, immediately after first real use**: every file failed
    silently the first time this was tried against a real download, with no
    folder prompt at all. Root cause confirmed: a `FileSystemDirectoryHandle`'s
    write permission is checked per top-level browsing context, not just per
    origin — the main window already had it granted (via the earlier
    remembered-default-folder feature, which is why no prompt appeared; that
    part was working as designed, not a bug), but that grant doesn't carry
    over to the popup automatically even though it's the very same
    postMessage-cloned handle. The popup now calls `queryWritePermission()`
    (`web/src/fsAccess.ts`) on a batch's handle before touching it; if not
    already granted there, processing pauses on a "Grant folder access"
    button in the popup itself instead of failing every file — clicking it
    calls `requestWritePermission()` with the popup's own real user gesture,
    which is what a cross-context grant actually requires, then resumes.
    Also surfaced the previously-swallowed per-file error: `failed` is now
    `{path, error}[]` end to end (`downloadWindowProtocol.ts`,
    `DownloadWindow.tsx`, the `App.tsx` relay's alert) instead of bare
    filenames, and the popup lists each failure's real reason inline —
    closes the gap that made this specific bug invisible without opening
    DevTools in the first place. This resolves caveat (1) from above (the
    handle itself clones and travels fine; it was the permission grant, not
    the handle, that didn't carry over) but caveats (2)–(5) remain unverified
    by a real click.
- **Progress bar for the Manual tab's multi-file "Download all"**: the
  streamed-to-folder path (File System Access) previously just showed a
  static "…" for the whole batch, however long it took. `writeFileToDirectory`
  now takes an optional per-chunk callback instead of a blind `pipeTo`, and
  the row button swaps for a small live progress bar (cumulative bytes
  written across every file in the batch, weighted by size) while a download
  is in flight. Deliberately scoped to that one path — the tab-per-file
  fallback and the per-file/zip buttons hand off to the browser immediately
  with nothing left to track, and already get real progress for free from
  the browser's own download manager.
- **Remembered default download folder** (`web/src/fsAccess.ts`): the Manual
  tab's "Download all" (individual files) button no longer shows the folder
  picker on every single download — the first pick is persisted (via
  IndexedDB, since a `FileSystemDirectoryHandle` can't live in localStorage)
  and silently reused as long as this browser still has permission for it,
  falling back to the picker again if not. A new "Change folder"/"Choose
  folder" control in Settings → Downloads shows the current default (by name
  only — browsers don't expose a handle's full path) and lets you switch or
  forget it. Deliberately scoped to the multi-file "individual files" path
  only: the per-file "Download" button and "Download all (zip)" in the
  detail view stay plain browser downloads on purpose, since for a *single*
  file that's strictly better than File System Access — it shows up in the
  browser's own download manager and survives the tab closing, neither of
  which streaming-to-a-picked-folder can offer. Brainstormed with the user
  first, including the real constraint that the whole streamed-download
  approach requires the tab to stay open for the duration, unlike a native
  browser download.
- **Managed vs. Manual downloads**: the web UI's single "Downloads" tab is
  now two — **Managed** (added through the qBittorrent/SABnzbd compat shims,
  i.e. by Sonarr/Radarr — auto-fetched to local disk exactly as before) and
  **Manual** (added directly via the web UI's own "+ Add" form, or
  discovered — see below — never auto-fetched; the user grabs files on
  demand via the existing per-file/zip-link endpoints, the same way TorBox's
  own web UI works). Which bucket a download lands in is permanent, decided
  once at add time by *which door it came through* (`database.Download
  .AddedVia`, `arr` or `manual`) — an *arr app has no way to reach the
  native API's add endpoints, and nothing but a human uses the web UI's "+
  Add" form, so the signal is unambiguous and needed no new toggle.
  `GET /api/v1/downloads` gained `?added_via=arr|manual` to back the two
  tabs; `ListDownloadsDueForRetry` (the query that feeds Completed Download
  Handling's fetch step) now filters to `arr` only, so a Manual download
  sitting in `provider_completed` just stays there instead of getting
  silently written to disk. Retry/Re-add are hidden for a Manual download in
  `error` state, since there's no local fetch attempt to retry — the row is
  just reflecting the provider's own live state.
  - **Discovery**: a download added directly through TorBox's own
    website/app — not through AcerviNode at all — now shows up in Manual
    too. Every tick, `Importer.discoverManual` diffs the same `List()` call
    `refreshStatuses` already makes against what's locally tracked; anything
    present at the provider with no local row gets adopted as `manual`. The
    very first time this runs for a given provider+kind, nothing is
    adopted — every currently-unmatched item is instead recorded into a new
    `discovery_baseline` table and permanently ignored, so shipping this
    doesn't flood the Manual tab with an account's entire pre-existing
    history; only items that show up *afterward* ever are.
  - New migration (`0004_added_via.sql`): `downloads.added_via` (default
    `arr`, so every existing row is correctly classified without a backfill
    step) plus the `discovery_baseline`/`discovery_seeded` tables.
  - Follow-on, immediately after shipping: the manual-download actions (the
    downloads table's per-row "Download all" button, and the detail view's
    per-file "Download"/"Download all (zip)" buttons) no longer appear for a
    Managed download at all — it's already being auto-fetched to local disk,
    so a manual grab is redundant there; those buttons are Manual-tab-only
    now. The underlying endpoints (`.../files/{fileId}/link`, `.../zip-link`)
    are unchanged and still work for any download's id — this is purely a
    web UI choice about which buttons to show, not a new restriction.
  - Follow-on: removed category from Manual entirely, after weighing it with
    the user — no category input in the "+ Add" form, no Category column in
    the Manual tab's table, no Category row in the detail view for a Manual
    download. Category only drives real behavior for Managed downloads (via
    `category_paths` save-path overrides); for Manual it would've been a
    purely cosmetic label, at odds with mirroring TorBox's own web UI, which
    has no categorization concept. Noted in ROADMAP.md as a 💡 candidate to
    revisit if the Manual tab gets hard to navigate once discovery's been
    running a while.

### Fixed

- Broken cross-reference links across the docs, found with a link/anchor
  checker written for the occasion rather than by inspection: `providers.md`'s
  "Completed Download Handling" header has a parenthetical
  (`` (`internal/importer`) ``) that changes its real GitHub anchor to
  `#completed-download-handling-internalimporter`, but every cross-reference
  to it — 7 of them, across `api.md`, `configuration.md`, `qbittorrent-api.md`,
  `sabnzbd-api.md`, `ROADMAP.md`, and a self-reference within `providers.md`
  itself — linked to the shorter `#completed-download-handling`, which GitHub
  has never actually recognized. Also updated `README.md` and
  `docs/quickstart.md`'s web UI descriptions, which still described a single
  downloads table with no mention of the Managed/Manual split.
- Clicking download on a Manual download whose provider item had vanished
  entirely (deleted directly through TorBox's own site — spotted live by the
  user, confirmed by querying the real account directly: the torrent was
  genuinely gone from TorBox's own list, not just slow to load) showed "No
  files available to download yet" — technically true but actively
  misleading, since it reads as "still processing," not "gone for good."
  `GET /api/v1/downloads/{id}` gained `files_error` (present only when the
  live provider query actually failed, omitted entirely on the ordinary
  empty-because-not-processed-yet path), and the web UI now shows that real
  reason instead of the generic message, in both the row-level alert and the
  detail view. Also documented (`docs/providers.md#managed-vs-manual`) a
  related, deliberately-not-yet-fixed gap this surfaced: nothing proactively
  detects a vanished Manual download ahead of time — it only shows up when
  the user actually tries to download — since Manual downloads are never in
  `internal/importer`'s fetch-retry path, which is what catches this for a
  Managed download within a few ticks. Left as a 💡 in ROADMAP.md; fixing it
  properly needs a debounce so a download that's merely slow to be freshly
  indexed doesn't get wrongly flagged "gone" after one missed poll.
- Manual downloads sitting at `provider_completed` showed a "Fetching" state
  badge — spotted by the user directly in the Manual tab right after
  shipping it. That label was accurate for a Managed download (where
  `provider_completed` really does mean `internal/importer` is about to
  fetch it) but a leftover from before Manual downloads existed: a Manual
  download is never auto-fetched at all, so "Fetching" was an outright lie,
  even though nothing was actually wrong on the backend (confirmed live: the
  two affected downloads were untouched, still sitting at `provider_completed`
  with no fetch attempt against either). `StateBadge` now takes `added_via`
  and shows "Available" instead, for that one state/added_via combination
  only — every other label is unchanged.

A full QA pass over every existing ability and setting, prompted by a direct
request to audit and fix rather than a specific bug report. Found via a
systematic code review of every package plus live testing against the real
WSL instance/TorBox account, not from user reports:

- **SABnzbd shim had no delete support at all** — `mode=queue`/`mode=history`
  never handled `name=delete`, meaning Sonarr/Radarr configured against
  AcerviNode's SABnzbd shim had no way to remove a download at all (the
  qBittorrent shim's `POST /torrents/delete` always worked fine; this gap was
  SABnzbd-specific). Fixed by adding `name=delete` support layered onto both
  modes, matching real SABnzbd's API shape (no separate delete mode) —
  `internal/sabnzbd/delete.go`. Verified live: inserted a real row, deleted it
  over an actual HTTP request against the running instance, confirmed removal
  without touching the account's other real downloads.
- **Provider ETA was silently dropped in both compat shims** —
  `debrid.DownloadStatus.ETASeconds` was populated correctly by the TorBox
  provider (confirmed: it reads TorBox's real `eta` field) but discarded in
  `database.RefreshFromProvider`, which has no column for it, and never
  reached qBittorrent's `torrentInfo.Eta` (always `0`) or SABnzbd's queue
  slots (no `timeleft` field existed at all) — Sonarr/Radarr's queue view
  showed no ETA for any active download despite TorBox genuinely reporting
  one. Fixed by reading it fresh from the same provider `List()` call that
  already refreshes state/progress on every poll, rather than trying to
  persist a fast-changing value — `eta` (qBittorrent) and `timeleft`
  (SABnzbd, formatted `H:MM:SS` matching real SABnzbd).
- **A TorBox torrent/NZB the provider itself marked as failed could never
  reach local `error` state** — `mapDownloadState`'s default case treated
  every unrecognized `download_state` as "still downloading," including a
  stalled/no-seeds torrent (explicitly called out as such in the old code's
  own comment) and TorBox's own documented `"Error"` state (confirmed via
  TorBox's help center: server error, missing encryption key, missing par2
  files, etc.) — meaning a download the provider had already given up on
  would show as perpetually "downloading" to Sonarr/Radarr forever, since
  nothing in AcerviNode could ever detect it. Fixed by porting
  [decypharr](https://github.com/sirrobot01/decypharr)'s own
  production-proven state mapping (the reference implementation this project
  benchmarks against): anything not explicitly a known downloading/completed
  state — including a stalled torrent — is now treated as an error. Verified
  the exact fix against the real account's own data: `mapDownloadState` is
  tested directly against `"stalled (no seeds)"`, the literal raw state a
  real torrent on the test account had at the time. See
  [Providers](docs/providers.md#state-mapping).
  - A provider-reported error isn't sticky — it recovers automatically if the
    provider later reports progress again (e.g. a stalled torrent finds a
    seed), unlike `internal/importer`'s own retry-exhaustion give-up, which
    *is* sticky by design. Distinguishing the two exposed a related,
    previously-dormant bug: `internal/importer.handleFailure`'s give-up path
    never persisted the final `retry_count`, so `database.RefreshFromProvider`
    had no reliable way to tell a local give-up apart from a provider error and
    could have silently resurrected a gave-up download back to
    `provider_completed` the next time the provider happened to report its
    unchanged old state. Fixed alongside the state-mapping change since the
    two are directly related.
  - The provider's raw failure reason (e.g. `"stalled (no seeds)"`) is now
    surfaced as the download's `error_message` — previously documented as
    "never surfaced through either compat shim directly," but that stance
    only made sense back when the provider could never actually produce a
    local `error` state at all; leaving `error_message` blank for a detected
    provider failure would be worse UX than showing the raw reason.
- Removed `database.ListDownloadsByState` — dead code, unused anywhere
  including tests, and a footgun for a future caller: it looked like it
  should back `internal/importer`'s due-for-retry logic but was missing the
  `next_retry_at` backoff check that `ListDownloadsDueForRetry` (the function
  actually used) applies.

### Changed

- Native API's `GET /api/v1/downloads[/{id}]` field `kind` is now `protocol`
  (`torrent`/`usenet`) — reads better to API consumers; the internal Go type
  stays `database.Kind` (matches `reflect.Kind`'s naming, avoids clashing with
  Go's `type` keyword). Frontend (`Download.protocol`) and the downloads table's
  column header updated to match.
- `internal/api`, `internal/qbittorrent`, and `internal/sabnzbd` no longer
  capture a static copy of `cfg.APIKey` at startup — every auth check now
  reads it live from a shared `Settings.APIKey()`/`apiKeySource`, which is what
  makes regenerating the key (see below) apply everywhere immediately.
- `internal/qbittorrent` and `internal/sabnzbd`'s near-identical
  `refreshFromProvider`/`localState` logic moved to `database.RefreshFromProvider`
  and `database.LocalStateFromProvider`, one shared implementation both shims
  (and now `internal/importer`, see below) call instead of duplicating.

### Changed

- Web UI's per-row "Download all" button now writes files straight into a
  local folder the user picks (via the File System Access API's
  `showDirectoryPicker()`), instead of opening one new browser tab per file.
  Each file is streamed straight to disk (`fetch` response body piped into a
  `FileSystemWritableFileStream`), never buffered whole in memory, which
  matters for multi-gigabyte video files. Chromium-only API (not Firefox/
  Safari) — unsupported browsers fall back to the previous one-tab-per-file
  behavior automatically. Confirmed via `curl -sI -H 'Origin: ...'` against a
  resolved TorBox CDN URL that it echoes `access-control-allow-origin` back
  to match, which is what makes fetching those URLs from the web UI's own
  JS legal in the first place.

### Added

- Two new settings addressing a real, observed problem — TorBox `rate limit
  exceeded (status 429)` errors seen live earlier this session from every
  add going straight to the provider with no local throttling:
  `max_concurrent_downloads` (default 3) bounds how many `provider_completed`
  downloads `internal/importer` fetches to local disk at once — previously
  hardcoded to strictly one at a time, with no way to change it, regardless
  of how many finished around the same time. `import_fetch_timeout_seconds`
  (default 600, matching the previous hardcoded value) bounds how long a
  single file's whole transfer may run before it's treated as a failed
  attempt — now enforced per-request via a context deadline instead of a
  fixed `http.Client.Timeout`, since it needed to change live. Both apply
  immediately (no restart) via the existing `PUT /api/v1/settings/general`
  alongside the other importer-tuning fields, and both are covered by tests
  proving real parallelism (not just serial timing luck) and real timeout
  enforcement (a CDN that never responds is given up on around the
  configured deadline, not left to hang).
- Per-category save-path overrides: `config.yaml`'s new `category_paths` map
  lets a category (e.g. "movies") route its Completed Download Handling
  output to a specific directory — a different disk or mount, say — instead
  of the default `download_dir/<category>`. New `PUT
  /api/v1/settings/categories/path` (body `{"category", "path"}`, empty path
  clears it), applied live to `internal/importer` with no restart and
  persisted to `config.yaml`. `GET /api/v1/settings/categories`'s response
  gained a `paths` field reporting current overrides. Web UI: a "Save path
  overrides" list in Settings → Categories, one row per known category.
- A "Default 'Download all' behavior" preference (individual files vs. a
  single zip archive) in Settings → Downloads, controlling the downloads
  table's per-row button — both options remain available per-download in the
  detail view regardless of this default. Purely client-side (localStorage,
  `web/src/preferences.ts`), not a server setting: it's about how this
  browser behaves, not AcerviNode itself, and the underlying zip vs.
  individual-files behavior already existed (see the previous two entries in
  this file) — this just lets the row button's default match whichever the
  user actually prefers instead of always defaulting to individual files.
- `GET /api/v1/downloads/{id}/files/{fileId}/link` — resolves a direct,
  provider-hosted download URL for one file, for downloading straight
  through a browser instead of (or alongside) AcerviNode fetching it to
  local disk. The exact same call `internal/importer` itself makes, not
  cached or proxied. Web UI: a "Download" button per file in the detail
  view's file table, plus a per-row "Download all" button in the downloads
  table that resolves and opens every file in a download individually.
  Building this surfaced a real, previously-unnoticed bug — see Fixed below
  — since it needed `GET /api/v1/downloads/{id}`'s file list to actually
  work first. Verified live: resolved a real TorBox CDN link and confirmed
  it served the exact file (`Content-Length` matched the known size
  exactly).
- `GET /api/v1/downloads/{id}/zip-link` — "download all" as a single
  provider-zipped archive, an explicit opt-in alongside the individual-files
  default above (a "Download all (zip)" button in the detail view). Backed
  by an undocumented TorBox `zip_link=true` parameter on `requestdl`, not
  mentioned in the SDK or public docs — found by testing directly, then
  confirmed live: the returned URL served a real `.zip`
  (`Content-Type: application/zip`, correct total size).
- `POST /api/v1/downloads/{id}/readd` — retry's stronger sibling, for when
  the *original* provider-side download is gone entirely (expired from the
  provider's own list), not just a transient fetch failure. New `downloads.
  source` column stores the original magnet/NZB URL at add time (link-based
  adds only, nothing kept for uploaded files); readd resubmits it as a brand
  new add, best-effort deletes the old provider-side entry, and points the
  local row at the new `provider_download_id` — same local id/name/category/
  hash, everything else reset as freshly added. `400` if no source was
  stored; `409` if not in `error` state, or if the fresh add dedupes back to
  a different already-tracked download (guarded, not silently corrupted).
  Web UI: a "Re-add" button next to "Retry" in the detail view. Verified
  live against the real TorBox account both ways: rejected cleanly with no
  stored source on a download that predates this feature, and correctly
  caught (409, no mutation) when a real re-add attempt deduped back to an
  already-tracked row.
- `POST /api/v1/downloads/{id}/retry` — manually retries a download that gave
  up after exhausting `import_max_retries`, resetting it back to
  `provider_completed` with `retry_count` cleared so `internal/importer`'s
  next tick attempts it again from scratch. `409` if the download isn't
  actually in `error` state. Web UI: a "Retry" button in the detail view.
  Verified live against a real download that had failed on a genuine TorBox
  rate limit — retrying picked it back up on the very next importer tick.
- `internal/api`: `POST /api/v1/downloads/torrent` and `POST
  /api/v1/downloads/usenet` add a magnet/`.torrent`/NZB URL/`.nzb` file
  directly, without needing Sonarr/Radarr or faking being one against a
  compat shim. Web UI: a new "+ Add" button opens a form for either protocol,
  either a link or a file upload, plus an optional category. Adding a magnet
  the provider dedupes to an already-tracked download (TorBox can hand back
  the same `torrent_id` for an already-cached hash) returns the existing row
  (`200`) instead of erroring on a duplicate — found and fixed during manual
  verification against the real TorBox account, which previously 500'd on
  this exact case.
- `internal/importer`: a new `refreshStatuses` step runs on every tick,
  proactively syncing every tracked download's status from its provider —
  previously this only ever happened reactively, when an *arr app polled one
  of the compat shims, so a download watched only through the native API or
  web UI could sit looking "queued" indefinitely even after the provider
  finished it. Verified live: added a real magnet, watched only the read-only
  native API (no shim polling at all), and saw it reach `ready_for_import` on
  the importer's own tick.
- `internal/debrid/torbox`: `Provider`/`UsenetProvider`'s `List`/`Status` now
  also check `GET /queued/getqueued` — a separate pre-processing queue TorBox
  holds a download in before it ever appears in `mylist`. Previously a
  backlogged download was indistinguishable from one TorBox had never heard
  of. Found by inspecting a comparable open-source debrid client's polling
  code (RDT-Client), which checks both endpoints where AcerviNode only
  checked one — see [Providers](docs/providers.md#torbox-internaldebridtorbox).
- Settings, expanded on three fronts:
  - `PUT /api/v1/settings/general` makes `download_dir`, `log_level`,
    `import_interval_seconds`, and `import_max_retries` editable live, no
    restart — `internal/importer.SetConfig` (mutex-guarded fields, and the
    running ticker resets to a new interval immediately rather than waiting
    out the old one) and a `*slog.LevelVar` swapped in for `log_level`.
    `port`/`data_dir` are also editable and persisted, but only take effect
    after a restart (rebinding the listener / reopening the database live is
    out of scope) — the response's `restart_required` reflects that. Along
    the way, `log_level` went from validated-but-never-actually-applied (a
    pre-existing bug — nothing ever read `cfg.LogLevel` after startup) to
    genuinely wired up; the default logger is now an explicit
    `slog.NewTextHandler`, which changes the on-disk log line format
    (`time=... level=... msg=...` instead of the old bare `2026/07/27
    15:04:05 INFO ...`) — a visible side effect of actually fixing it.
  - `POST /api/v1/settings/providers/torbox/test` makes one real, live call
    to TorBox and reports latency or the actual failure — a genuine
    connectivity+auth check, not just "is a key set."
  - `GET`/`POST /api/v1/settings/categories` surface both compat shims'
    category stores (previously invisible outside their own in-memory state)
    and let one be pre-declared manually — `qbittorrent.Server`/
    `sabnzbd.Server` gained exported `Categories()`/`AddCategory()`.
  - Web UI: the Settings tab's General card is now an editable form (with a
    live/restart-required distinction shown per field), the TorBox card
    gained a "Test connection" button, and a new Categories card shows both
    protocols' lists plus an add form.
- `internal/api`: `GET /api/v1/settings/general` (AcerviNode's own port, data
  dir, download dir, log level, import settings, and its own `api_key` in
  plaintext) and `POST /api/v1/settings/api-key/regenerate` (replaces the key,
  applies immediately across the native API and both compat shims, persists to
  `config.yaml`). Web UI: the Settings tab's new "General" card shows all of
  this, with copy/reveal on the API key and a "Regenerate API key" button;
  regenerating keeps the UI's own session in sync automatically
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

- `GET /api/v1/downloads/{id}` always returned `files: []`, even for a fully
  `ready_for_import` download with real files on local disk — the
  `download_files` table it read from was defined but never actually
  populated by anything (`ReplaceDownloadFiles` had no real caller). Found
  while building manual download links, which needed a working file list to
  attach a per-file "Download" button to. Fixed by querying the provider
  live instead — the same approach `internal/qbittorrent`'s own file listing
  already used successfully — rather than fixing the unused local cache.
  Confirmed live: a real completed download that had shown `files: []` for
  this entire session now correctly lists all 3 of its actual files.
- `internal/debrid/torbox`: `createusenetdownload`'s `usenetdownload_id` is
  decoded as a JSON number now, not a string — the official SDK's docs say
  string, but a real account's response doesn't match that, and adding an NZB
  through the native API's new usenet endpoint failed every time with `json:
  cannot unmarshal number into Go struct field ...usenetdownload_id of type
  string`. Found via a real NZB upload through the web UI.
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
