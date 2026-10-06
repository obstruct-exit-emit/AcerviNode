# 📦 AcerviNode Roadmap

Where the project has been and where it's going. Every phase 0–10 is
complete; Phase 4 is where additional debrid providers get added as they
become worth the maintenance cost. The
fine-grained record of every change lives in the [CHANGELOG](CHANGELOG.md).

**Start here:** [Critical — do next](#critical--do-next) — all five
Sonarr/Radarr integration findings are now fixed, tested and live-verified.

**Legend:** ✅ complete · 🔴 critical, not started · 🟡 queued fix · 🔄 in progress · 💡 under consideration · ⏳ blocked

## At a glance

| Phase | Scope | Status |
|---|---|---|
| [0 — Foundation](#phase-0--foundation-) | Repo, config, database, CI | ✅ |
| [1 — TorBox vertical slice](#phase-1--torbox-vertical-slice-) | TorBox provider, qBittorrent shim, SABnzbd shim | ✅ |
| [2 — Completed Download Handling](#phase-2--completed-download-handling-) | Fetch resolved files to local disk once a download is done | ✅ |
| [3 — Native API & UI](#phase-3--native-api--ui-) | Richer `/api/v1`, embedded web UI | ✅ |
| [4 — Multi-provider](#phase-4--multi-provider-) | Provider-routing refactor, AllDebrid, two accounts per service | ✅ |
| [5 — Hardening & release](#phase-5--hardening--release-) | systemd unit, packaged Linux binaries, release automation | ✅ |
| [6 — Full QA pass](#phase-6--full-qa-pass-) | Systematic review + live testing of every existing ability; 3 real bugs found and fixed | ✅ |
| [7 — Managed vs. Manual downloads](#phase-7--managed-vs-manual-downloads-) | Split the web UI into two tabs by how a download was added, plus discovering items added directly through TorBox | ✅ |
| [8 — Web Downloads & account status](#phase-8--web-downloads--account-status-) | TorBox's generic hoster-debrid service (Mega, 1Fichier, and ~160 others), plus a TorBox account status display | ✅ |
| [9 — Login, users & roles](#phase-9--login-users--roles-) | Optional login accounts on top of the API key, admin/member roles, first-run setup wizard | ✅ |
| [10 — Native HTTPS](#phase-10--native-https-) | Self-signed, auto-generated TLS alongside the existing plain-HTTP listener; one-click restart | ✅ |

---

## Path to daily-driver parity (replacing rdt-client / decypharr)

Requested directly: a prioritized, trackable punch list of what stands between
"works great for me" and genuinely replacing rdt-client/decypharr as a daily
driver — not just a feature diff, but ranked by what actually blocks trusting
this unattended. Recommendations first.

### Critical — do next

Five findings from reviewing both compat shims against
Sonarr's and Radarr's *actual* source (`QBittorrent.cs`, `QBittorrentProxyV2.cs`,
`Sabnzbd.cs`, `SabnzbdConfig`, `SabnzbdDownloadStatus` — fetched and read, not
recalled). Requested directly ("review code and find ways to make this work with
sonarr and radarr better", then "add all to the roadmap to do next critical").
Ordered by what actually costs downloads.

**All five are done**, each with a test written first and watched fail, and all
five verified live against a real TorBox account. What the live run showed, on
real data: the account held **29 Manual rows** (10 torrent, 19 usenet), every one
of which used to be reported to \*arr and none of which is now; a real `.torrent`
upload produced the infohash an independent implementation computes for the same
file; a queued download reported `eta: 8640000` rather than 0; `complete_dir`
came back rooted from both endpoints; and deleting a download added with an
explicit `savepath` removed only its own directory, leaving the shared parent and
everything else in it intact — which before this would have been deleted outright.

- ✅ **Both shims expose Manual downloads to \*arr apps, and \*arr never filters
  them out.** `handleInfo` (`internal/qbittorrent/torrents.go`), `handleQueue`
  and `handleHistory` (`internal/sabnzbd`) all call `ListDownloads(ctx, kind)`,
  which has no `added_via` filter. The assumption that category scoping saves us
  is **wrong, and was checked rather than assumed**: the `GetItems` loop in both
  Sonarr's and Radarr's `QBittorrent.cs` reads `Category` into the item and never
  filters on it, and SABnzbd's `GetQueue`/`GetHistory` do the same. Every row we
  report enters \*arr's queue — on a real account that is every discovered
  Manual download, i.e. the operator's whole personal library.

  Worst consequence first: **a Manual download can silently block a legitimate
  grab.** Sonarr's `QueueSpecification` rejects a release when a queued item
  already matches that episode, so a Manual copy of a show sitting in the queue
  forever stops Sonarr grabbing it — and presents as an indexer problem. Beyond
  that, Manual rows never leave `provider_completed` (that is what Manual
  *means*), which we report as `downloading`, so they sit in \*arr's queue as
  perpetually-downloading items feeding its stuck-download warnings; errored ones
  report `error` as permanent Warning noise; and \*arr's own "remove from queue"
  can delete a Manual download along with its files.

  The shims already *write* `AddedViaArr` on add, and their own comments say
  "this shim only exists for \*arr apps" — only the reads never got the same
  treatment. Fix is a Managed-only read path for both shims, with the native
  API/UI left unfiltered. Needs one decision: hard filter, or a setting.

- ✅ **`resolveDestDir` can `os.RemoveAll` a directory shared with other
  downloads.** `internal/importer`'s first branch returns `d.SavePath` bare,
  while the function's own doc comment promises the result is "always namespaced
  by the download's own name so sibling downloads in the same category never
  collide" — and three further comments build on "resolveDestDir ends the
  destination with the download's own name". Both `RemoveLocalFiles` and
  `cleanupDownload` then `os.RemoveAll` that path, guarded only against an empty
  `Name`, which does not help here: deleting or cleaning up one download **wipes
  every sibling sharing that save path**. Separately it makes `content_path` the
  shared directory and `save_path` an unrelated parent, so \*arr scans a folder
  full of other releases' files.

  Not reachable from Sonarr/Radarr — confirmed from `QBittorrentProxyV2`, whose
  add sends only urls/torrents, category, stopped, sequentialDownload,
  firstLastPiecePrio, contentLayout, ratioLimit and seedingTimeLimit, never
  `savepath`. It *is* reachable from LibriNode and any other client that sends
  one, and survives through `pending_arr_adds`. Why the suite misses it: every
  `internal/importer` test sets `SavePath` to a unique `t.TempDir()`, which makes
  the shared-directory case look correct. The fix is also the more faithful
  emulation — real qBittorrent puts content *into* savepath — so join the
  download's name onto it and report `save_path` as the requested path itself
  rather than synthesizing a parent from it.

- ✅ **Unknown ETA is reported as `0`, not qBittorrent's `8640000` sentinel.**
  `toTorrentInfo` sends `live.ETASeconds`, which is `0` whenever nothing has been
  cached for that row yet. Sonarr's `GetRemainingTime` is explicit about the
  convention — `if (torrent.Eta == 8640000) return null;`, commented "qBittorrent
  sends eta=8640000 if unknown such as queued" — so `0` is read as *zero seconds
  remaining* rather than unknown, and every queued download claims to be finishing
  immediately.

- ✅ **`complete_dir` is never reported, so \*arr cannot resolve the client's
  output root.** `internal/sabnzbd`'s `mode=get_config` sends
  `misc.complete_dir: ""` and `mode=fullstatus` omits the field entirely. Sonarr's
  `GetCategories` falls back to `fullstatus`'s `CompleteDir` precisely when
  `complete_dir` is not rooted, so both paths come back empty, `category.FullPath`
  is empty, and `status.OutputRootFolders` — what Sonarr's remote-path-mapping
  health check reads — is empty with it. Report the real download directory in
  both places.

- ✅ **A `.torrent` file add records no infohash.** `addTorrentFile` passes an
  empty hash to `recordPendingAdd`, so a lost add reply can only ever be
  reconciled by name. Sonarr and Radarr compute the infohash from the `.torrent`
  themselves and use it as `DownloadId`, so parsing the info dict at add time
  would make reconciliation exact instead of best-effort — the very limitation
  [CLAUDE.md](CLAUDE.md)'s "a pending \*arr add is claimed exactly once" invariant
  calls out — and guarantee the hash we report matches \*arr's `DownloadId`
  immediately rather than waiting a poll for the backfill.

  **Checked and deliberately *not* changed** — recorded so none of these gets
  "fixed" later on a hunch. `Ratio: 0`/`RatioLimit: 0` genuinely does satisfy
  `HasReachedSeedLimit` (verbatim: `if (RatioLimit >= 0) { if (RatioLimit - Ratio
  <= 0.001f) return true; }`), so `pausedUP` really unlocks
  `CanMoveFiles`/`CanBeRemoved`. All four qBittorrent states map as intended, and
  `error` maps to Warning rather than Failed, so it does not trigger \*arr's failed
  download handling. All eight SABnzbd status strings we emit are real
  `SabnzbdDownloadStatus` enum members, so none risks a deserialization failure.
  `config.Sorters` cannot throw on a missing key — `SabnzbdConfig`'s constructor
  initializes it. And the `content_path != save_path` requirement is satisfied.

### Faster fetching — plan first, not started

Requested directly ("can we increase our managed fetch speed?"), then
deliberately parked as a plan: "I want us to think and plan on this one as we
can easily break downloading instead of speed it up". Every Managed download
goes through this path, so the plan below is ordered around **not breaking it**
before making it faster.

**The full design — how fetching works today, the Stage 1–3 designs, the risk
register, the test plan and the Stage 0 measurement protocol — is in
[docs/design/faster-fetching.md](docs/design/faster-fetching.md).** The summary
below is kept short on purpose.

#### What we know (measured, not assumed)

Fetch speed on the production instance, from real downloads during the burn-in:

| Download | Files | Size | Fetch time | Speed |
|---|---|---|---|---|
| Night of the Living Dead (usenet) | 1 | 19.3 GB | 244 s | ~79 MB/s |
| Amphibia S01E37 (usenet) | 1 | 321 MB | ~5 s | ~64 MB/s |
| Pink Floyd, The Wall (torrent, FLAC) | 26 | 434 MB | ~56 s | ~7.7 MB/s |
| Avantasia 2CD (torrent, MP3) | ~24 | 261 MB | ~40 s | ~6.5 MB/s |

**Many-file downloads run about 10× slower than single files.** The cause is in
the code, not the line: `processReserved` fetches a download's files strictly one
after another, and each costs a TorBox `requestdl` call plus a new connection
starting from TCP slow-start. For a 17 MB FLAC track that fixed cost is most of
the file's time.

A probe against a real TorBox CDN link (a 5.5 GB file, 12 s per run, from the
dev PC on the same line while production was busy fetching — so the absolute
numbers are depressed; the comparison is what counts):

| Run | 1 connection (today) | 4 range requests |
|---|---|---|
| 1 | 45.3 MB/s | 53.5 MB/s |
| 2 | **1.4 MB/s** | 29.2 MB/s |
| 3 | 15.2 MB/s | 31.9 MB/s |

- The CDN serves **HTTP/1.1 only** and answers ranges with `206`. So the Go
  HTTP/2 stream-window cap (4 MB ÷ RTT ≈ 80 MB/s at the ~40–100 ms measured RTT)
  that looked like it explained the 79 MB/s ceiling **does not apply** — ruled
  out by measurement. Forcing HTTP/1.1 or enlarging the HTTP/2 window changed
  nothing (30.0 and 29.2 MB/s).
- A single connection is **erratic** (1.4–45 MB/s); four were steady (29–54).
  Splitting a big file mostly buys consistency — one slow CDN connection can no
  longer stall the whole file — and possibly some peak.
- The 32 KB `io.Copy` buffer is not the limit at these rates (reasoning, not
  measured — the CPU is not the bottleneck). Not worth changing on its own.

#### The two changes

1. **Several files of one download at once.** The big, certain win for albums,
   season packs and audiobooks: overlap the per-file link request and connection
   ramp-up. Expected roughly 3–4× on many-file downloads (estimate, to be measured).
2. **One big file over several range requests.** Steadier single-file speed,
   maybe more peak. Only for files large enough to matter, and only when the
   server proves it honours ranges.

#### What can break, and the guard for each

- **Wrong bytes on disk.** A server that ignores `Range` answers `200` with the
  whole file, so N segments would each write the whole file at their offset.
  Guard: accept a segment only on `206` with a `Content-Range` that matches what
  was asked; anything else falls back to one plain stream. Segments are written
  with `WriteAt` into one preallocated `.part`, and the total is checked
  against `expectedTransferSize` before the rename, as today.
- **The `.part` + rename guarantee.** A file must still only appear at its real
  path once whole. Same rule per file, unchanged; with several files in flight,
  each keeps its own `.part`.
- **"Already fetched" on retry.** `fetchFile` skips a file whose size already
  matches. A preallocated `.part` must never be mistaken for a finished file —
  it is never at the real path, so this holds, but it needs a test that a
  retried download with leftover segment `.part`s refetches correctly.
- **Cancel and delete.** Every delete calls `CancelFetch`, which must now stop
  every goroutine of the download — all files, all segments — and still report
  `errFetchCancelled`, not a failure. The deadlocked-cancel and orphan-file
  tests already exist for one stream; they need a many-stream version.
- **One failure, one `handleFailure`.** Today a failed file fails the attempt
  once. With N goroutines, the first error must cancel its siblings and the
  attempt must fail exactly once with that first error — not N times, not with a
  sibling's "context canceled".
- **Idle/stall timeout.** Per connection, as now. A stalled segment must not be
  masked by its busy siblings, and must not cancel a download whose other
  connections are fine for longer than one idle window.
- **Per-file retry stays narrow.** `openFile` retries only refused requests;
  that rule applies per segment. Link-resolution errors — a 429 included — still
  go to the download-level backoff.
- **TorBox rate limit — the biggest unknown.** N files at once is N `requestdl`
  calls in a burst. A 429 maps to `ErrRateLimited`, and the backoff **blanks
  polling for the whole kind**, which presents as everything freezing. Faster
  fetching that trips that is a regression. Must be measured before choosing a
  default (see Stage 0).
- **The connection budget.** `max_concurrent_downloads` (3 on production)
  counts downloads. With files × segments per download, the real connection
  count multiplies — 3 × 4 × 4 = 48. There must be one global cap on
  connections, not two independent multipliers.
- **Progress.** Must stay monotonic and reach exactly 100% with bytes arriving
  out of order from several places. `SetFetchProgress` gets an atomic sum;
  `fetch_integrity_test` already pins the boundary cases for one stream.
- **The disk.** Many concurrent writers on a spinning disk can be slower than
  one sequential writer. Measure on `/storage_1`/`/storage_2` before defaulting
  up.
- **The shared line.** More connections take more of the line from everything
  else. Answered by the speed limit in Stage 3, not by holding defaults down.
- **AllDebrid.** Never measured for any of this. Stays at today's behaviour
  until it is.

#### Staged plan — each stage ships dark and is flipped only after live proof

0. **Measure on the Proxmox host itself, while idle**, with the throwaway probe
   (single vs N ranges, and N files at once on a real album):
   - Does one TorBox link serve concurrent ranges, and is there a per-IP or
     per-link connection cap?
   - How many `requestdl` calls in a burst before a 429? Watched through
     `GET /api/v1/status` error counts, not guessed.
   - What do the two storage disks sustain with 1, 4 and 8 writers?
   - What is the line actually capable of?

   Defaults are chosen from this data, not from the dev PC.
1. **Files in parallel.** New setting, `fetch_files_concurrently`, **default 1 =
   exactly today's code path**, kept as the `n == 1` branch rather than rewritten.
   Test-first, mutation-checked, race detector. Live: fetch the same cached
   album at 1 and at the candidate value, compare speed and the bytes (hash
   every file), check zero 429s and that Sonarr/CantiNode still import. Only
   then raise the default.
2. **Segmented big files.** `fetch_connections_per_file`, **default 1**, and
   only for files above a size threshold (e.g. 256 MB) once the server has
   proved `206` + `Content-Range`. Same verification, plus a deliberate test
   against a server that ignores `Range`, one that drops a segment mid-way, and
   one that returns the wrong range.
3. **One global connection cap** across downloads × files × segments, and a
   **speed limit** setting (`fetch_speed_limit_mbps`, 0 = unlimited, the
   default). Tuned for the fastest the CDN, TorBox and the disk allow; the
   operator caps it if the line is shared ("if we optimize to be as fast as we
   can. we can make a throttle in settings"). A throttled connection is slow on
   purpose, so time spent waiting on the limit must never count toward the
   stall timeout.

**Rollback at every stage is a setting**, not a revert: both new values at 1
run today's code exactly.

#### Done when

- Many-file downloads measurably faster on production (target ≥3×), single files
  no slower.
- Every byte identical to the single-stream fetch on the verification downloads.
- Zero new 429s and zero new failed imports over a few days of real traffic.
- The new invariants written into `CLAUDE.md`: what the connection budget is,
  that a segment is only accepted on a matching `206`, and that a download's
  attempt fails exactly once.

### Queued fixes

Two findings from the second code review (the fetch path, the TorBox client and
the delete paths), deliberately left for after the burn-in rather than changed
the day of it. Requested directly: "add ... to road map to fix".

- ✅ **One big download holds up everything behind it.** `Importer.Tick`
  fetches its batch of `provider_completed` downloads concurrently, up to
  `max_concurrent_downloads`, but then `wg.Wait()`s for the *whole batch* before
  returning — and `Tick` runs synchronously inside `Run`'s select loop, so the
  next tick cannot begin until the slowest fetch in the batch finishes. Its own
  doc comment says so ("Tick itself still blocks until every one of this batch
  has finished"), so this is a documented trade-off rather than an oversight,
  but two of its consequences are worth fixing:

  - **Head-of-line blocking.** Downloads due for a fetch are only selected at
    the start of a tick. With a 50 GB season pack fetching and two of three
    slots free, a download that becomes `provider_completed` meanwhile — the
    fast poll does notice it promptly, on its own goroutine — still waits for
    the pack to finish before its fetch can start. Every episode grab queues
    behind the biggest thing in flight.
  - **`last_tick_at` freezes.** Everything else the tick does stalls with it:
    the bulk provider refresh, discovery, missing-detection, cleanup and the
    stuck-download watchdog. `GET /api/v1/status` exists so an external monitor
    can tell the tick loop is alive, and during every long fetch it reports
    that it is not.

  The likely shape of a fix is a persistent, `Importer`-level fetch pool that a
  tick *feeds* rather than waits on, keeping `max_concurrent_downloads` as a
  limit across ticks. `tryStartFetch` already refuses to start a second fetch
  for a download mid-fetch, which is most of the safety a non-blocking tick
  needs. The hard part is the test suite: many importer tests call `Tick` and
  then assert a row reached `ready_for_import`, which depends on `Tick` being
  synchronous — so they need a way to wait for the pool to drain, not a
  rewrite. Also resize the pool live on `SetMaxConcurrent`, which today only
  takes effect when a batch drains (see `docs/configuration.md`). The burn-in
  should say how much this actually costs before it is built.

  **Measured on the first production burn-in.** With a 9.9 GB file transferring
  (~101 s at ~98 MB/s), a movie and an episode that finished at TorBox during it
  waited **84 s and 79 s** before their transfers began — with 2 of 3 fetch
  slots free the whole time — and `last_tick_at` froze for 103 s. The cost is
  the big download's transfer time, so it scales with size and inversely with
  link speed: on a 20 MB/s link the same file would hold every other finished
  download back for about eight minutes.

  **Fixed.** Fetches now run in a pool that outlives the tick: a tick hands due
  downloads to it and returns, so the next tick comes round on time and fills
  any free slot, and `last_tick_at` keeps moving. The limit holds across ticks,
  a freed slot is refilled the moment a fetch succeeds, and `Run` waits for
  in-flight fetches at shutdown; `Tick` still waits, which is what the fifty
  tests calling it rely on. Two tests through `Run` reproduced the production
  symptoms before the fix; every safeguard was mutation-checked and caught; the
  race detector is clean.

- ✅ **Two simultaneous `import_interval_seconds` saves can hang one of
  them.** Minor, and very unlikely. `SetConfig` tells `Run` about a new
  interval through a one-slot channel: a non-blocking send, and if the slot is
  already full, drain it and send again. That second send *blocks*. With two
  saves racing, the first can drain, the second can refill the slot, and the
  first's blocking send then waits for `Run` to read — which it cannot do while
  it is busy with a tick. That used to mean until a whole fetch batch drained;
  since the fix above, a tick no longer waits on fetches, so the hang is bounded
  by one tick's own work (the provider refresh, a few seconds).

  Only `import_interval_seconds` is affected. `SetFastPollInterval` uses the
  same pattern, but its reader, `runFastPoll`, is a separate goroutine that a
  long fetch never blocks. The fix is to treat the channel as a wake-up signal
  only: make the final send non-blocking as well, and have `Run` read the
  current value from `getConfig` when woken rather than trusting the value it
  received — so a dropped duplicate can never leave the ticker on a stale
  interval. The fix above removed the long wait; this race is what remains.

  **Fixed**, as described: both interval channels are now wake-up signals, every
  send is non-blocking, and the reader takes the current value from config. The
  listing-cache lifetime is applied from the current interval under its own
  lock too, which closes a smaller race beside it. The test storms the setter
  400 rounds deep with nothing reading — a single storm caught the hang only one
  run in five — and failed 8 runs out of 8 against the old code.

- ✅ **Notice when TorBox fails to unpack a usenet download.** Found on the first
  production burn-in. TorBox marked `Amphibia.S01E37` completed, but its file list
  was the raw post — 25 `.rar` parts, par2 volumes, an `.nzb`, an `.srr` — with no
  media file. AcerviNode fetched all of it (about 1.2 GB, ~750 MB of it par2 for
  ~490 MB of content), and Sonarr has sat on "Found archive file, might need to
  be extracted" ever since: nothing will ever import it.

  It is TorBox-side and transient, not a flag we fail to send. TorBox's default
  `post_processing` (`-1`) is documented to repair, extract and delete the
  sources, and re-submitting the very same NZB with our current settings came
  back correct — one `.mkv`, no archives. So the job to do is *noticing*, not
  configuring.

  Real SABnzbd reports a failed unpack as `Failed`, which is what lets \*arr
  blocklist the release and search again. Proposed: before fetching a usenet
  download, look at its file list, and if it holds only archive and recovery
  files (`.rar`/`.rNN`/`.partNN.rar`, `.par2`, `.nzb`, `.srr`, `.sfv`, `.nfo`)
  with nothing else, do not fetch it. Either mark it failed with a SABnzbd-style
  `fail_message`, or first resubmit its NZB to TorBox once — which worked when
  tried by hand — and fail only if that comes back archive-only too. Two
  details to respect: Sonarr downgrades exactly one message, "Unpacking failed,
  write error or disk is full?", from Failed to Warning, so do not use that
  wording; and a release that is legitimately an archive (a comic, an ebook
  bundle) is not something any \*arr app can import either, so the rule is
  safe for all of them.

  **Fixed**, the first way: a usenet download whose files are only archives and
  their recovery/metadata files is not fetched, the attempt fails with a reason
  counting the files by extension, and after the usual retries it ends Failed
  for Sonarr to blocklist and re-search. Checked against the real TorBox file
  list for the Amphibia job: packed, "1 .nfo, 1 .nzb, 9 .par2, 25 .rar, 1 .srr".
  Re-submitting the NZB first is left undone — a possible follow-up; see
  [Providers](docs/providers.md#when-torbox-does-not-unpack).

**Do next** — scoped, self-contained, verifiable without a second provider:

- ✅ **Mass-vanish circuit breaker.** Done — requested directly ("complete
  the first 3 tasks in Path to daily-driver"). `isSuspectedMassVanish`
  (`internal/database`) now refuses to run missing-detection for a whole
  `RefreshFromProvider` pass when more than half of at least 3 tracked
  Manual downloads for a kind are missing from the same listing at once —
  found rows in the same pass still update normally. See
  [CHANGELOG](CHANGELOG.md) and
  [Providers](docs/providers.md#proactively-detecting-a-vanished-manual-download).
- ✅ **Rate-limit-specific backoff for 429s.** Done, same request.
  `debrid.ErrRateLimited` is a new provider-agnostic sentinel
  (`torbox.APIError.Unwrap` resolves to it for a 429, recognizable via
  `errors.Is` through however many wrapping layers); `internal/importer`
  backs off that kind's own `List` polling specifically (30s base, doubling
  per hit, capped at 5 minutes, scoped independently per kind) instead of
  retrying every tick regardless. See
  [Providers](docs/providers.md#provider-rate-limit-backoff).
- ✅ **Retention/cleanup policy.** Done, same request. New
  `cleanup_after_days` config (0/disabled by default) has
  `Importer.cleanupOldDownloads` remove a Managed download's local files,
  provider-side copy, and row once it's sat `ready_for_import` for at least
  that many days — never a Manual download. Surfaced in Settings → General.
  See [Providers](docs/providers.md#retentioncleanup-policy).
- ✅ **Three settings gaps found by comparing against rdt-client's own
  settings, field by field, not guessed.** Requested directly ("take a
  complete look at all of the rdt-client settings — are we missing any that
  would make sense"). Most of rdt-client's surface didn't apply (pluggable
  download engines, unpack limits, banned trackers — architecturally not
  how AcerviNode works), but three genuinely did:
  - **Per-file fetch filtering** — `min_fetch_file_size_bytes`/
    `include_file_regex`/`exclude_file_regex` skip a file (samples, junk,
    unwanted types) when fetching a download's files to local disk.
    `Importer.filterFiles`, applied right after the provider's own file
    list comes back. Any combination can be set at once, unlike
    rdt-client's own "only use one" convention for include/exclude.
    ✅ Immediate follow-on, same technique against a second reference
    client: `max_fetch_file_size_bytes`, the symmetric counterpart to the
    minimum, found comparing against
    [decypharr](https://github.com/sirrobot01/decypharr)'s own
    `MinFileSize`/`MaxFileSize`. `config.Config.Validate` rejects a
    minimum greater than a nonzero maximum.
  - **Stuck-download watchdog** — `stuck_download_timeout_minutes`
    auto-errors a download that's sat `queued`/`downloading` with no
    genuine change reported for too long. Deliberately keyed differently
    than rdt-client's own blunt "maximum lifetime": on `updated_at` (only
    ever moves on a real change — state/progress/size/error), not simply
    elapsed time, so a large download still steadily making progress is
    never punished just for taking a while.
  - **Error-state cleanup** — `cleanup_error_after_days` automatically
    removes a download that's sat in `error` for too long, the one real gap
    the existing retention policy's own scope left open (Managed +
    `ready_for_import` only — an errored download had no automatic cleanup
    path at all). Applies to both Managed and Manual downloads, unlike that
    one, since an error already means a real dead end either way.

  All three surfaced in Settings → General, live-editable, no restart. See
  [Providers](docs/providers.md#per-file-fetch-filtering) (and the two
  sections immediately following it).

**Verify before trusting this daily** — process, not code:

- 💡 **Extended burn-in against real, continuous Sonarr/Radarr traffic.**
  Everything shipped so far has been live-verified in short, deliberate
  bursts. The delete-race fixed in the Source-backfill follow-on (below)
  only surfaced under rapid, repeated real testing — exactly the class of
  bug a quick smoke test won't catch but weeks of real unattended use will.

**Already tracked, real but lower urgency:**

- 💡 Real pause/resume for streamed Manual downloads, surviving a restart —
  see the entry below.
- ✅ Streamline the download UX — done, see the entry below.
- 💡 **Reconsider Docker packaging.** Deliberately deferred so far (Phase 5)
  in favor of a Linux binary + systemd. Explicitly told this is the lowest
  priority item here — not pursuing unless that changes.

**Someday / maybe** — deprioritized on purpose, not dismissed:

- ✅ **Database backup story.** Previously deprioritized by the user ("I don't
  care if we lose database"), on the reasoning that re-discovery would
  re-adopt anything still sitting on the provider. What changed that: the
  database stopped being only a download cache. Since Phase 9 it also holds
  every login account, plus categories, path overrides and all
  live settings — none of which any provider can hand back. Losing it now
  means losing the way in, not just the history.

  `internal/backup` snapshots the database to `<data_dir>/backups` every
  `backup_interval_hours` (default 24), keeping `backup_keep` (default 7),
  via SQLite's `VACUUM INTO` — consistent against a live database with no
  need to stop the service, and self-contained with no `-wal`/`-shm`
  sidecars. Opt-in, like every other retention setting
  here — but the only one where doing nothing has a cost rather than
  being the safe choice, so it is the one worth turning on deliberately.

  Pruning runs only after a *successful* backup (trimming first would mean a
  failed backup had also discarded a good one), `backup_keep: 0` retains
  everything, and only files AcerviNode wrote are ever pruned. Nothing is
  backed up at startup, so a restart loop can't flush good snapshots out of
  the window. `GET`/`POST /api/v1/settings/backups` plus a Settings → Backup
  tab; the API returns names and sizes but never a snapshot's contents,
  since one holds every account and both provider keys. Restore is manual and
  documented. Live-verified: retention pruned 4 to the newest 2, the
  interval retuned without a restart, and a restored snapshot opened clean
  with its rows intact.
- ✅ **Alerting/observability, part (a).** Built proactively, picked directly
  off this list's own "(a) first ... it's nearly free" note, while the user
  was away — the only way to know the importer's stuck used to be manually
  tailing the log, exactly the gap this closes. `GET /api/v1/status` now
  reports `last_tick_at` (the tick loop's own liveness — proves it hasn't
  stalled/crashed, full stop), per-kind `last_successful_list_at`, per-kind
  `rate_limited_until` (`Importer.RateLimitCooldownUntil`, previously
  exported only for tests, now read for real), and per-kind `error_count`
  (new `database.CountDownloadsByState`). Deliberately doesn't fold in
  TorBox's own `cooldown_until` (a separate, earlier fix — see
  [CHANGELOG](CHANGELOG.md) and
  [Providers](docs/providers.md#cooldown_until--measured-and-not-a-restriction))
  even though both were motivated by the same incident — a listing
  call that succeeds but finds nothing new still advances
  `last_successful_list_at`, so the two fields answer genuinely different
  questions ("is polling itself working" vs. "is the provider account
  restricted") and conflating them would have hidden the very incident that
  prompted both. `requireAuth`, same tier as `/version`/`/providers` — not
  admin-only, not fully open like `/health`. See
  [Providers](docs/providers.md#status-monitoring-get-apiv1status).
- 💡 **Alerting/observability, part (b).** Still not started: an outbound
  webhook AcerviNode itself fires on specific events (a download reaching
  `error`, sustained rate-limiting, the tick loop going quiet, auth
  failure) — matches the *arr "Connect" pattern, more flexible than polling
  (a) above, but real new surface (event scoping, retry semantics,
  per-event config) this project already passed on once for a similar
  reason (TorBox's own Notifications API, skipped in Phase 8). Revisit only
  if (a) genuinely turns out insufficient in practice — polling one cheap
  endpoint covers most of the same ground for a lot less complexity.

**Structural, blocking, and honestly big:**

- ⏳ **Serving the library instead of downloading it.** The single biggest
  architectural difference, and the one item on this list that needs a
  redesign rather than a feature. Everything currently lands on local disk,
  so past a few TB the disk is the limit instead of the debrid quota.

  Correcting this entry's own history: it used to say decypharr's trick is a
  FUSE mount. Their README now describes **read-only NFSv4 and SMB servers**
  with custom virtual folders. Different mechanism, same consequence — worth
  recording because the mechanism is exactly what is undecided here.

  **The hard part is not the protocol.** Whichever is picked, the same two
  problems arrive with it:

  - **Debrid links expire.** A virtual file has to re-resolve a fresh URL on
    access, which means a per-file URL cache with its own TTL, single-flight
    so ten readers do not trigger ten resolves, and a sane answer when the
    provider is rate-limiting mid-read.
  - **\*arr wants to import, not just read.** Sonarr/Radarr hardlink or move
    on import. Neither works from a read-only network mount, so an import
    becomes a full copy — which is the thing this was meant to avoid. Any
    design has to answer what \*arr actually does at import time before the
    serving mechanism matters at all.

  **Avenues, cheapest first:**

  - **Symlink into an rclone mount the user already runs.** rdt-client's
    approach. We serve nothing and write symlinks instead; rclone does the
    hard part. Almost no new code, no new protocol surface, and it composes
    with whatever the user already has. Costs an external dependency and a
    setup step, and the \*arr import question above still applies.
  - **WebDAV.** `golang.org/x/net/webdav` is in the standard extended
    library, so this is the least new code of anything we would serve
    ourselves. rclone can mount it, and several players read it directly.
    Good first thing to build precisely because it is small enough to prove
    the URL-cache and range-read machinery before committing to a protocol.
  - **NFSv4 and/or SMB.** What decypharr does now. Mounts natively on Linux
    and Windows with nothing extra installed, which is the real appeal. Go
    servers for both exist but are markedly less mature than the HTTP stack
    we rely on today, and SMB in particular is a large protocol to get subtly
    wrong.
  - **FUSE.** `hanwen/go-fuse`. Behaves like a real filesystem, which makes
    the \*arr import story the easiest of the four. Ties us to a mount
    lifecycle, needs FUSE present, and complicates the systemd unit
    (`ReadWritePaths` already bit us once). Linux-only, which is fine here.
  - **`.strm` files.** Write a stub pointing at the provider URL. Plex and
    Jellyfin play them; \*arr cannot import them properly, and the URL inside
    goes stale. Listed to be dismissed rather than rediscovered later.

  Sequence, if this is picked up: prove the URL cache and range reads behind
  WebDAV first, since that layer is shared by every option above it. Only
  then decide whether NFS/SMB is worth the protocol surface.

- ⏳ **All five debrid services.** TorBox and AllDebrid are done, with any
  number of accounts on either (`providers.<name>.type`), and everything
  routes by the provider a download actually belongs to — so adding one is
  implementing the `debrid` interfaces and nothing structural.

  Both rdt-client and decypharr support five: Real-Debrid, TorBox,
  AllDebrid, Premiumize and Debrid-Link. That leaves **Real-Debrid,
  Premiumize and Debrid-Link** to write, and it is the plainest gap against
  either of them.

  What actually blocks each is an account to verify against, not the code.
  This project's rule has been verify live, never guess, and every provider
  quirk found so far — TorBox's undocumented `cooldown_until`, AllDebrid's
  docs being wrong about its own response shape, the comma-separated-hashes
  claim that is false in practice — was found by making real calls. Writing
  three providers from documentation alone would produce three plausible
  implementations and no idea which of them work.

  So: acquiring a cheap account on each is the actual first task, not a
  detail. Premiumize is the most useful of the three to have anyway, since it
  is the only other one of the five that does usenet.

- ⏳ **Download engine choice.** One HTTP fetcher today, with no way to tune
  it. rdt-client ships four engines and the contrast is unflattering.

  **The missing throttling is the cheapest win and worth doing first,
  independent of any engine work.** There is currently no speed limit, no
  connection count, and no chunk sizing — `max_concurrent_downloads` caps how
  many downloads run at once and nothing caps what one of them does to the
  line. On a shared or metered connection that is a real absence, and it is a
  setting on the existing fetcher rather than a new engine.

  Engines worth having, in order of value:

  - **Parallel/chunked HTTP.** Range requests across several connections with
    resume. The natural evolution of what exists, and the one most likely to
    make a visible difference on a fast line. Now planned in detail, with
    measurements, under
    [Faster fetching](#faster-fetching--plan-first-not-started).
  - **Symlink.** No download at all — shares its entire design with the
    serving item above, and should be built with it rather than separately.
  - **aria2c handoff.** Hand the URL to an aria2c daemon over RPC. Small
    integration, mature downloader, and it inherits aria2's own tuning for
    free instead of us reimplementing it.
  - **Synology Download Station.** rdt-client offers it to keep downloads off
    the machine running the client. Only worth it if you actually have one.

  The engine picked should be per-download-able, not global: a symlink engine
  makes sense for a library and a chunked fetch for something you want on
  disk now, and forcing one choice across everything is the wrong shape.

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
- ✅ **Streamline the download UX**: done — three separate "download
  everything" paths (per-row default mode, detail view's always-both-visible
  "Download all"/"Download all (zip)" buttons, and the Settings default-mode
  preference controlling only the row's behavior) consolidated into one
  `DownloadOptionsDialog`, opened identically from the table row and the
  detail view, showing every mode this browser can actually do (folder
  streaming is simply absent, not disabled, on a browser without
  `showDirectoryPicker`) as explicit radio choices, remembering the last one
  picked as next time's default instead of that living in a separate
  Settings dropdown. Also fixed a real correctness bug found while
  investigating this: the per-file link and the Firefox/Safari "individual
  files" fallback both used a plain `window.open`, which — since the
  provider's per-file link carries no `Content-Disposition: attachment`,
  unlike the zip link — renders inline (plays the video, shows the image)
  in *every* browser instead of downloading, not just Firefox/Safari.
  `fsAccess.forceDownload` (fetch → blob → synthetic `<a download>` click)
  fixes this everywhere; blob URLs are always same-origin, so the `download`
  attribute reliably applies regardless of the provider link's own
  cross-origin status. Investigated (and ruled out, explained to the user)
  whether Firefox/Safari could get genuine folder-write access at all —
  they can't, short of a companion browser extension, which is a
  fundamentally different, much bigger undertaking than a UI consolidation
  pass. See [CHANGELOG](CHANGELOG.md). Same caveat as always: this is
  client-side browser behavior requiring a real user click, so — like the
  rest of this project's frontend work — it's type-checked/built clean and
  confirmed live-serving the updated bundle, but not confirmed by an actual
  browser click-through.
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
- **Cached & metadata previews on the "+ Add" form** (follow-on, prompted by
  a direct audit of the official TorBox API against what AcerviNode actually
  uses — "anything in the official torbox api we don't have but should?").
  `CheckCached` existed at the client layer from early on but nothing ever
  called it; closed for torrent/usenet/webdl, plus a new torrent-only
  metadata preview (`torrentinfo` — name/size/seeders/full file list, by
  hash alone, before ever adding it). Found live: the real wire format for
  `checkcached`'s multi-hash query doesn't match TorBox's own docs (a
  comma-joined value times out; repeated `hash=` params is what actually
  works). Live-verified against the real account: a known-cached test
  torrent correctly reported cached with its real metadata; a fabricated
  hash correctly reported not-cached and no-preview-available. See
  [Providers](docs/providers.md#cached--metadata-previews).

## Phase 4 — Multi-provider ✅

Delivered in two halves: the routing work that made a second provider
*possible*, then AllDebrid itself.

**The refactor (stages 1–5).** Every provider call used to resolve by *kind*
alone — `d.Provider` was recorded on every row and reported by the API, but
never used to route anything. Identical behaviour with one provider, and
wrong the moment there are two: a `provider_download_id` means nothing to a
different account. Fixed in order, each stage landing green on its own:

1. Route by `(provider, kind)` rather than kind — 11 call sites, five of
   which were found only by sweeping for *provider calls taking a download*
   rather than for the named lookups, since several inlined the same switch.
2. `debrid.Registry`: one `Dynamic*Provider` per provider per kind, built
   from config instead of a hardcoded `"torbox"`, and resolved through by
   the native API, the importer and both compat shims.
3. Rate-limit and list-success state keyed per `(provider, kind)` — limits
   are enforced per account, so one provider's 429 must not stall another.
4. A `default_provider` setting plus an optional per-add `provider`
   override. Re-add deliberately ignores both and returns to the account the
   download already belongs to.
5. A per-provider settings surface: `/settings/providers/{name}`, generic
   `liveSettings` methods, and a UI listing every configured provider.

**AllDebrid.** Torrent-only, deliberately — no usenet service exists, and
its hoster debriding is a synchronous unlock with nothing to poll, so
supporting web downloads would mean inventing a lifecycle rather than
integrating one. `providers.<name>.type` also separates an entry's name from
its implementation, so **two accounts on the same service** work.

Verified live with three providers configured at once (TorBox, AllDebrid,
and a second TorBox entry): per-provider capabilities reported correctly, a
usenet add naming AllDebrid refused, the same magnet added to two providers
staying two independent downloads, and an AllDebrid download fetched to disk
through the unlock path.

Four real bugs surfaced only by running it against real accounts, each fixed
with a regression test confirmed red first: an explicit `added_via=arr`
silently staying Manual when the provider deduped; the default resolving to
a provider with no credentials; a torrent-only default breaking usenet
outright; and a degraded provider marking live downloads as vanished.

Still open:

- Real-Debrid, Debrid-Link, Premiumize as they become worth the maintenance
  cost. The plumbing is done and proven, so each is now implementing the
  `debrid` interfaces and having an account to verify against — nothing
  structural.
- AllDebrid web downloads, if its saved-links model ever justifies the
  invented lifecycle.
- 💡 Per-provider detail on `GET /api/v1/status`, which still aggregates
  across providers per kind. Non-breaking to add; deliberately deferred so
  the endpoint's shape stayed stable for the UI's rate-limit banner.

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

- **Second pass, done proactively while the user was at work**: this code
  hadn't had a fresh look since the QA pass above, while everything shipped
  in the meantime (the Downloads popup subsystem, Phase 7's follow-ons) had
  already been separately reviewed as it landed. Read through the TorBox
  client/provider/adapters, both compat shims, `internal/importer`, and the
  native API again in full. Found one real, confirmed issue: the qBittorrent
  and SABnzbd shims' auth checks used a plain `!=` comparison while the
  native API's own auth deliberately uses `crypto/subtle.ConstantTimeCompare`
  — brought both shims in line with that convention. Also used the same
  live-account-audit technique that found the torrent hash bug: no new data
  bugs turned up, but a real usenet download's expiration from TorBox's
  `mylist` mid-audit reconfirmed the vanished-download scenario applies to
  usenet too, not just torrents (see the 💡 item below), and left
  `RequestUsenetZipDownloadLink` still unverified — the window to test it
  against a real download closed before the test could run. See the
  [CHANGELOG](CHANGELOG.md) for full detail.

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
- Immediate follow-on, found live by the user: the Downloads popup could
  split across two separate windows when triggered from two independently-
  opened AcerviNode tabs — `window.open()`'s "reuse by name" trick only
  works within one browsing-context group. Rebuilt the popup's
  coordination on `BroadcastChannel` + a singleton `navigator.locks` lock
  instead of direct window references, which don't care about that
  topology. See the [CHANGELOG](CHANGELOG.md) for the full design.
- Bug found live by the user asking a simple "why" (some torrents had a
  hash, some didn't): a torrent discovered while TorBox was still indexing
  it (placeholder name, no hash yet) got stuck with that incomplete
  snapshot forever, since `RefreshFromProvider` never revisited `hash`/
  `name` on later polls. Confirmed directly against the user's real TorBox
  account before fixing — two adopted torrents had an empty local hash
  while TorBox's own `mylist` already had the real one. Now backfilled
  automatically the next time the provider is polled; verified live, both
  torrents picked up their real hash and real name on the very next
  importer tick after deploying. See the [CHANGELOG](CHANGELOG.md).
- Immediate follow-on, requested by the user: Stop/retry-failed/dismiss
  controls added to the Downloads popup's per-batch header, styled as
  small icon buttons rather than several separate buttons bolted on.
  Stopping uses a real `AbortController` (not just hiding the row), and
  deliberately leaves the partial file on disk — free groundwork for the
  pause/resume item above, not thrown away. Fixed a real bug as a side
  effect: the popup's own "already processing" guard was never cleared
  after a batch finished, so re-downloading anything already tracked there
  silently did nothing; it's cleared on completion now regardless of
  outcome. See the [CHANGELOG](CHANGELOG.md).
- Immediate follow-on, built overnight while the user slept: once the
  placeholder category above was gone, the remaining category list's only
  real job was gating "Save path overrides" behind a category having been
  "known" first — backwards, since the backend never actually required
  that (`SetCategoryPath` accepts any name directly, confirmed by reading
  it). Settings' Categories section is now just "Save path overrides":
  existing overrides plus a two-field form that works for any category
  name, no prerequisite declare-it-first step, no separate management UI.
  Verified live via the API (set/confirm/clear an override for a
  never-before-seen category name) — the visual layout itself couldn't be
  checked without a browser and needs a look. See the [CHANGELOG](CHANGELOG.md).
- Self-review pass, done proactively while the user was at work — nothing
  reported broken, but code shipped without live testing deserved a second
  look before it got found live instead. Found and fixed one real bug: a
  file that genuinely failed before a later Stop click was silently
  dropped — invisible in the popup, never reported to the main window, and
  never retried, since `filesDone` (what `retryStopped` sliced on) counts
  every attempted file, not just successes. See the [CHANGELOG](CHANGELOG.md).
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

**Proactively detect a vanished Manual download** ✅ (immediate follow-on,
2026-07-29 — user picked this directly off a short list of next-move options).
Previously nothing noticed a Manual download's provider item was gone until
the user actually clicked download and hit `files_error` live —
`RefreshFromProvider` only ever *updated* rows it found a matching status
for, and a Manual download is never in `internal/importer`'s fetch-retry path
(the thing that catches this for a Managed download within a few ticks,
since the fetch attempt itself fails and eventually gives up with a clear
reason). Confirmed as a real, recurring gap twice already — once for a
torrent, then reconfirmed for a usenet download during a later live-account
audit (both expired from TorBox's own `mylist` while still cached locally as
`provider_completed`).

Built exactly as scoped above: `RefreshFromProvider` now increments a new
`downloads.missing_count` column (migration `0006_missing_count.sql`) each
time a tracked `AddedViaManual` row is absent from a *successful* provider
listing (a `List()` call that itself failed — e.g. a rate limit — never
counts as a miss, since `refreshKind` already skips calling
`RefreshFromProvider` at all on a listing error), and flags the row `error`
with a fixed, clear reason (`"no longer found in the provider's account"`)
once that reaches a threshold of 3 consecutive misses — a debounce, not a
single-miss rule, for exactly the reason originally identified: a row only
starts being tracked once it was already visible to the provider somehow, but
TorBox's own listing endpoints have shown brief eventual-consistency gaps
right around that boundary elsewhere in this project (see the hash/name
backfill entry above). Deliberately scoped to `AddedViaManual` only — a
Managed row's `missing_count` never moves, since its own fetch-retry path
already covers the same scenario with a more specific reason — and
deliberately not sticky, the same way a provider-reported error already
isn't: the threshold path never touches `RetryCount`, so a download that
reappears later (found again in some future listing) self-heals with zero
special-case code, reusing `RefreshFromProvider`'s existing
`state==error-and-RetryCount>0` stickiness check unchanged. A row already
`error` for some other reason is left alone by this path entirely, so it
never clobbers a more specific existing reason.

No frontend changes were needed at first — `GET /api/v1/downloads[/{id}]`
already surfaced `state`/`error_message` unconditionally, and the existing
error badge already rendered any `error_message` string. Full test coverage
in `internal/database/downloads_test.go`: a Managed row is never flagged
however long it's missing, a single miss doesn't flag a Manual row, the exact
threshold-th miss does (with the persisted row checked directly, not just the
in-memory struct), a Manual row that reappears before the threshold resets
its counter and updates normally, a flagged row self-heals once the provider
reports it again, and an already-`error` Manual row isn't double-flagged or
overwritten. See [Providers](docs/providers.md#proactively-detecting-a-vanished-manual-download).

Verified live in the truest sense — not staged: shortly after shipping, this
account's own real, ordinary usage produced a genuinely vanished torrent
(`Silo.S03E04...`), and AcerviNode's background polling flagged it `error`
with the new message on its own, unprompted.

**Immediate follow-on, the same day**: the feature above created a real gap
of its own — a Manual download now genuinely reaches `error` state, but
Retry/Re-add were still gated to `added_via === 'arr'` only, so there was no
recovery action to show at all. `POST .../readd` already worked for any
kind/added_via server-side (it only ever checked `state` and a stored
`Source`, never `added_via` — the restriction was purely a frontend
condition), so this was a UI fix: a new `has_source` field
(`GET /api/v1/downloads[/{id}]`) reports whether a download actually has a
link stored to resubmit (false for an uploaded file, or a discovered
download with no original link ever known), and the web UI now shows Re-add
for any download in `error` state with `has_source: true`, not just Managed
ones — Retry itself stays Managed-only, since there's still genuinely no
local fetch to retry for Manual. Confirmed live against real data on this
same account: the pre-existing "Dragon Ball Z" row has a real stored source
link (`has_source: true`), while a discovered test row correctly showed
`has_source: false`.

Same follow-on also added a streamed "Download all" button to the detail
view's Files section, next to the existing "Download all (zip)" — reusing
the exact same `handleDownloadAll` entry point the downloads table's per-row
button already used (same mode preference/folder-picker/Downloads-popup
behavior), rather than duplicating it. This one hasn't been confirmed by an
actual browser click-through yet — see [CHANGELOG](CHANGELOG.md).

**Second immediate follow-on, same day**: extending Re-add to Manual only
helps if a Manual download actually has a `Source` — a *discovered* one
never did (nothing to capture, since AcerviNode never received an add
request for it), which the user hit directly and asked about ("can we store
the nzb info for retry?"). Researched what TorBox's three services actually
expose per download, confirmed live: a torrent needs nothing from TorBox at
all (a magnet is always reconstructable from just its `hash`), while usenet
and webdl `mylist` both include an undocumented `original_url` field,
populated only for a URL-based add. `internal/importer.discoverManual` and a
new `database.BackfillSource` (wired into `RefreshFromProvider`) now capture
this into `Source` at discovery time and retroactively for an already-tracked
row, respectively. **Verified live in full** — not just unit tests — using
the exact same real Big Buck Bunny torrent from the vanish-detection
verification above: discovered with the reconstructed magnet already as its
`Source`, deleted from TorBox to simulate a vanish, flagged `error`
automatically, then `POST .../readd` was called for real and *actually
resubmitted successfully*, landing a genuine fresh `queued` torrent on the
account — the strongest verification of any change this session. A real NZB
file (provided directly for this test) confirmed usenet's file-upload case
correctly has `original_url: null`, matching the documented limit: once a
download has already vanished from the provider, there's nothing left to
backfill from either way. See
[Providers](docs/providers.md#re-add-for-a-discovered-download).

**Third immediate follow-on, found live during the verification above**:
repeatedly deleting and immediately re-checking Big Buck Bunny surfaced a
real, reproducible race — a just-deleted download could reappear as a ghost
Manual download, because TorBox's own delete isn't always instantly
reflected in its listing endpoints, and `discoverManual` runs independently
of any specific delete request. Fixed with a short-lived tombstone
(`deleted_downloads` table, `RecordDeletedDownload`/
`RecentlyDeletedDownloads`) that `handleDeleteDownload` writes to and
`discoverManual` checks before adopting anything — see
[CHANGELOG](CHANGELOG.md) for full detail. Briefly investigated what looked
like real data loss while chasing this down (several of the user's own
in-progress downloads had vanished from the local database) — confirmed
directly with the user before continuing rather than assumed, and it turned
out to be them cleaning up their own downloads through the web UI in
parallel with this testing, not a bug.

**Fourth immediate follow-on**: the Source-backfill work above still left one
real gap — a usenet download added through AcerviNode's own "+ Add" form as
an uploaded `.nzb` file (not a URL) had no `Source` and no way to get one
backfilled either, since the raw bytes only ever existed in that one
request. The user asked specifically for this to be storable so no orphaned
file could ever be left behind — landed on storing the bytes directly on the
`downloads` row as a `BLOB` rather than a file on disk, so deleting the row
removes the stored file atomically with it, no separate cleanup step and no
possibility of an orphan. Deliberately excluded from the normal list/detail
read path (only the cheap filename is included there) so every poll doesn't
pay for the file bytes; the actual blob is fetched once, only when Re-add
actually needs it. See [CHANGELOG](CHANGELOG.md) for full detail.

💡 **Real pause/resume for streamed Manual downloads, surviving an AcerviNode
restart**: requested by the user after the Downloads popup work above. Once a
download link is resolved, AcerviNode's server is already out of the data
path entirely (the browser streams straight from the provider's CDN to
disk), so an in-progress transfer already survives a server restart today —
incidentally, not by design. What's still missing is genuine pause/resume:
deliberately stopping and picking a transfer back up later, including after
the popup itself is closed or the browser restarts, without redownloading
from byte zero.

The one real unknown was checked live before adding this here rather than
assumed: **TorBox's CDN fully supports HTTP Range requests** — confirmed
against a real 83MB file (`nexus-216.cnam.tb-cdn.io`), a `Range:
bytes=1000-2000` request correctly returned `206 Partial Content` with an
accurate `Content-Range` header. That was the one fact that could have killed
the whole idea; it didn't. Two of the other building blocks are already in
place from existing features: a `FileSystemDirectoryHandle` already survives
a full browser restart via IndexedDB (proven by the remembered-default-folder
feature), and File System Access already supports resume-from-offset writes
natively (`createWritable({ keepExistingData: true })` +
`write({ type: 'write', position: N, data })`).

What's actually left to build:
- A new IndexedDB store recording bytes-written per file, throttled rather
  than updated on every chunk, reconciled against the real on-disk file size
  (not blindly trusted) on resume in case of a crash mid-write.
- Resume-aware fetch/write logic: `Range: bytes=N-` on the request, position-
  based writes on the response, with a per-request fallback (not just a
  one-time assumption) in case a specific response ever comes back `200`
  instead of `206`.
- Popup rehydration: on load, `DownloadWindow.tsx` needs to check IndexedDB
  for anything left unfinished from a previous session (not just wait for a
  live `add-batch`) and offer to resume it — new UI states (paused,
  resuming, needs-permission-to-resume) on top of what's there today.
  Resuming needs a fresh link each time (TorBox links expire after ~3 hours),
  which is a normal API call that can itself fail if AcerviNode happens to be
  down right at that moment — handled the same way as any other API error,
  not a special case.
- Pause/resume controls in the popup UI.

Deliberately not started yet: this is sized larger than any single feature
shipped this session (roughly the original popup feature plus all four of
its follow-on fixes combined), and — like everything else in this
subsystem — it can't be verified headlessly. Confirming it actually works
means live-driving the browser: start a large download, actually kill and
restart the AcerviNode server mid-transfer, confirm it paused cleanly,
resume it, confirm via request headers that it genuinely resumed with Range
rather than restarting, then check the finished file's integrity. That's a
dedicated session's worth of hands-on-the-browser verification, not
something to tack onto the end of an already-long one.

## Phase 8 — Web Downloads & account status ✅

Prompted directly by the user asking "can we add Torbox's mega api support?
should we add a few others while we do that?" — built fully autonomously per
explicit go-ahead ("do this without me as I will be gone for a few hours").
"Mega API support" doesn't mean anything Mega-specific in TorBox's real API —
it turned out to mean TorBox's generic Web Downloads (hoster-debrid) service,
confirmed by researching TorBox's real OpenAPI spec directly rather than
assuming, and confirmed live that Mega is one of ~160 currently active hosters
on it. Evaluated the TorBox SDK's remaining 6 service categories against
AcerviNode's actual purpose before deciding what else to add: Integrations
pushes files the wrong direction for this project's model (it's about
debriding *into* AcerviNode, not pushing out to a NAS/upload target),
RssFeeds would duplicate Sonarr/Radarr's own job, Notifications/General
aren't relevant to a download client, and Queued is already implemented
(TorBox's pre-processing queue, since Phase 3's status-update-speed
investigation). Landed on: Web Downloads (the real ask) + a small
UserService-based account-status display, everything else skipped.

- **`debrid.WebDownloadProvider`** — a third optional provider interface
  (`AddLink`/`Status`/`List`/`Files`/`RequestDownloadLink`/
  `RequestZipDownloadLink`/`Delete`), same "not every provider needs it"
  shape as `UsenetProvider`. Genuinely link-only, unlike torrent/usenet adds —
  TorBox's own `createwebdownload` has no file-upload variant either. New
  `downloads.kind = 'webdl'` (migration `0005_webdl_kind.sql`, recreating the
  table since SQLite can't `ALTER` a `CHECK` constraint in place) — every
  `webdl` row is always `added_via: manual`, since there's no *arr-facing
  shim that could add one on Sonarr/Radarr's behalf.
- **`debrid.AccountProvider`** — a one-method interface (`Account`) backing
  a live TorBox account-status display (plan tier, subscription state,
  premium expiry, lifetime bytes downloaded). `DynamicTorrentProvider.Account`
  delegates to its inner provider via a type assertion rather than needing a
  whole fourth `Dynamic*Provider` wrapper for one read-only call.
- New endpoints: `POST /api/v1/downloads/webdl` and
  `GET /api/v1/settings/account` — both follow existing conventions exactly
  (dedup/provider-status-fallback for the add endpoint; the routine
  "available: false, not a hard error" stance already established by every
  other settings endpoint for the account one).
- Web UI: a third "Web Link" tab in "+ Add" (no file-upload mode shown for
  it, matching the endpoint), and a TorBox account section in Settings.
- **Every claim ended up confirmed live**, including two that briefly looked
  unverifiable: two earlier attempts at a safe test web download failed (a
  GitHub raw-file link came back `UNSUPPORTED_SITE`; PixelDrain's anonymous
  upload now requires its own API key), but `archive.org` turned out to be
  one of TorBox's own ~160 supported hosters, and a small public-domain test
  file it hosts made a real, safe end-to-end test possible. Confirmed:
  `createwebdownload`'s response field `webdownload_id` (documented as a
  string, initially handled defensively via a `flexibleID` type pending
  confirmation) is actually a JSON number — a raw API call returned
  `{"webdownload_id": 1462379, ...}` — the same mismatch `usenetdownload_id`
  turned out to have (see Phase 3's status-update-speed entry); `flexibleID`
  was then simplified back to a plain `float64`. `RequestWebDownloadZipDownloadLink`
  was confirmed too: the resolved URL served a real `application/zip` with the
  correct `content-disposition`. The full add → status → files →
  per-file-link → zip-link → delete cycle ran end to end through AcerviNode's
  own live API, and the provider-side delete was independently confirmed by
  querying TorBox's own `webdl/mylist` directly afterward. Also confirmed:
  the account's real Mega folder download history (since expired), Mega's
  active status among TorBox's hosters, and the real account's actual
  `GET /user/me` response (a genuine Pro/`plan: 2` subscription, with far
  more fields than the SDK's own docs or Go types declare). See
  [Providers](docs/providers.md#web-downloads).
- Full test coverage added at every layer touched: TorBox client
  (`httptest` fakes for all six new client methods), a provider-adapter
  test file mirroring the existing ones, `internal/debrid`'s Dynamic-wrapper
  tests, `internal/api` handler tests, an `internal/importer` test proving
  the new provider wires into discovery/status-refresh correctly, and an
  `internal/database` regression test for the migration itself (a `webdl`
  row actually inserts and round-trips, not just that the migration runs
  without erroring). Windows Application Control blocked running
  `internal/debrid`'s own test binary locally during this session (unrelated
  to any of this session's code — every other package's tests ran fine); the
  full suite, including that package, was instead run and confirmed green
  from a WSL environment against the same working tree.

## Phase 9 — Login, users & roles ✅

Requested directly: "Maybe we should make login, users and first time setup
wizard? look at librinode to understand the feel of it." Followed the
instruction literally — read LibriNode's actual real implementation
(`internal/api/auth.go`, `internal/config/config.go`'s `AuthSettings`/
`UserAccount`, `App.tsx`'s gating logic, the full `SetupWizard.tsx`) before
designing anything, rather than approximating the idea from memory. Then
asked, and got an answer, on the one real design fork this raised for
AcerviNode specifically (LibriNode's own member role only restricts
Settings/System; AcerviNode's member is scoped to Manual downloads only,
"because of manual download ability and possible future additions").

- **The API key is completely unaffected.** It's still the instance's
  root-equivalent master credential — `currentRole` (`internal/api/auth.go`)
  treats a matching key as an anonymous admin session, unconditionally, the
  same as always. Sonarr/Radarr and scripts never need to change anything.
- **No login accounts means the feature is entirely inert** —
  `AuthSettings.Enabled()` is just `len(Users) > 0`. This is what makes it
  safe to ship as a default-on upgrade rather than something requiring an
  opt-in flag: confirmed live against the real WSL instance, `auth_enabled`
  read `false` after deploying this and every existing API-key-based call
  continued working completely unchanged.
- **Roles, enforced server-side**: `admin` — everything. `member` —
  Manual downloads only (add/view/manage a magnet/NZB/hoster link grabbed
  directly), no access to the *arr-driven Managed pipeline at all, no
  Settings. `internal/api/downloads.go`'s `downloadByID` — the single choke
  point every single-download handler (Get/Delete/Retry/Re-add/file-link/
  zip-link) already routed through — is where this actually lives, not
  scattered per-handler; `handleListDownloads` separately forces
  `added_via=manual` for a non-admin regardless of the request's own query
  param. Every `/api/v1/settings/*` route (general config, providers,
  categories, account status, user management) requires admin, except
  password self-service (any signed-in account can change its own).
- **First-run setup wizard**: `SetupNeeded` is `!AuthEnabled() &&
  !TorBoxConfigured()` — deliberately not also a database check, since
  every download insert path already requires an active provider, making
  "TorBox never configured" a reliable-enough proxy for "genuinely fresh."
  Three steps (Account → TorBox key, skippable, with a live Test → Done),
  much shorter than LibriNode's own six (it walks through library folders,
  metadata, indexers, and download clients — AcerviNode's whole setup
  surface is one provider).
- **The Default account** (created by the wizard, or promoted later via
  "Make default") can't be removed or demoted — an instance with login
  enabled can never end up with zero admins able to sign in.
- Sessions are in-memory only (a restart logs everyone out — matches this
  project's own accepted stance on losing the database, and LibriNode's
  identical choice for the same reason), PBKDF2-SHA256 (600,000 iterations,
  same format string LibriNode uses), HttpOnly/`SameSite=Lax` cookie,
  30-day expiry.
- New: `internal/config/auth.go` (16 tests), `internal/api/auth.go` (~35
  tests — password hashing, session lifecycle, `requireAuth`/`requireAdmin`,
  setup/login/logout, user management, member row-level enforcement),
  `cmd/acervinode` wiring (6 tests). Frontend: `SetupWizard.tsx`,
  `LoginForm.tsx`, `SecuritySettings.tsx` (Settings → Security — list/add/
  remove accounts, change role, promote default, reset password),
  `App.tsx`'s gating rewritten to insert setup/login ahead of the existing
  API-key prompt (not replacing it), Managed/Settings tabs hidden for a
  member.
- **Verified live end to end, deliberately never against the real
  instance**: a separate scratch instance (its own `config.yaml`, its own
  data dir) was taken through the entire flow for real — fresh install
  correctly reported `setup needed: true`, `POST /setup` created the first
  admin and signed the browser in via cookie, a second account added as
  `member` correctly got `403` from `/settings/general` and `200` (empty)
  from `/downloads`, correctly reached the provider layer (`503`, no TorBox
  key on the scratch instance) rather than being blocked by auth when
  adding a webdl link, attempting to remove the Default admin correctly
  failed, and logging out correctly ended the session (`401` on the next
  call). The real, already-in-use WSL instance was only ever hit with safe,
  read-only checks (`auth_enabled: false`, `setup needed: false`, existing
  API key still works) — deliberately never given a real test user, since
  the first account added becomes permanently un-removable (the
  Default-account protection) and would have flipped a real, currently-
  working instance into requiring login by accident. See
  [Providers](docs/providers.md#auth-login-accounts-and-roles).

**Follow-up: login made mandatory for the web UI.** Once the user actually
created a real admin account on the real instance through separate live
testing, "no accounts yet" stopped being this instance's reality — at that
point the original `ApiKeyGate` browser prompt was unreachable dead code (a
login-enabled instance can never go back to zero accounts), so it was
removed along with the `SetupNeeded`/`TorBoxConfigured` composition it
depended on. `SetupNeeded` is now simply `!AuthEnabled()`; the web UI always
requires a signed-in session, and the API key's role is purely programmatic
(Sonarr/Radarr/scripts) from here on. Verified by redeploying straight to
the real instance (already past setup, already signed in as its own
account) and confirming it came back up unaffected.

## Phase 10 — Native HTTPS ✅

Triggered by live-testing on a Proxmox VM reachable only over a plain LAN
IP: the web UI's folder-picker download mode (the browser's File System
Access API) requires a secure context — HTTPS, or `http://localhost` — and
that's a hard browser-vendor restriction with no client-side workaround.
Confirmed, not assumed: MDN and Chrome for Developers' own docs on the
secure-context requirement; the HTML `download` attribute sanitizing `/` to
`_` in every browser, specifically to prevent faking folder nesting that
way; `chrome://flags/#unsafely-treat-insecure-origin-as-secure` genuinely
works but is a manual per-browser/per-device flag, not something the server
side can provide.

- **Native, self-signed, auto-generated TLS**, not a reverse proxy — a real
  no-warning certificate (Let's Encrypt) needs a public domain to validate
  against, which a private LAN IP doesn't have, so self-signed is
  unavoidable either way; a proxy would just add a second service to run for
  no gain over doing it natively. Real precedent for this being a normal
  pattern: confirmed Portainer auto-generates a self-signed cert and serves
  HTTPS by default on port 9443; Synology DSM, Unraid, and pfSense/OPNsense's
  admin UIs do the same — the standard shape for *appliance-style*
  self-hosted software, a different convention than the *arr ecosystem's
  "always proxy it yourself."
- **Dual-listen, always** (`cmd/acervinode/main.go`) — the existing
  plain-HTTP listener on `port` keeps running completely unchanged; when
  `tls_enabled`, a second `*http.Server` on `tls_port` (default `8443`)
  serves the exact same handler. Nothing already pointed at `http://...`
  (Sonarr/Radarr, scripts, bookmarks) is ever affected either way.
- New `internal/tlscert` package: ECDSA P-256, self-signed, 10-year
  validity (deliberately no rotation logic), SANs covering loopback, every
  local interface IP, `localhost`, and the hostname. Generated once and
  reused as-is on every later start — never silently regenerated, so a
  device that already trusted it doesn't have to re-trust for no reason.
  `tls_cert_file`/`tls_key_file` let an operator supply a real certificate
  instead (config/env only, same treatment `data_dir` gets). Cert
  generation failing when `tls_enabled` is fatal, not a silent HTTP-only
  fallback.
- **One-click restart**: `POST /api/v1/settings/system/restart` reuses
  `run()`'s existing shutdown path — `signal.NotifyContext`'s own `stop`
  function, wired in as the trigger, needs zero new shutdown plumbing.
  Deliberately never automatic-on-save (would drop the admin's own session
  mid-edit). `packaging/acervinode.service` changed `Restart=on-failure` to
  `Restart=always`, since the endpoint's clean exit is exactly what
  `on-failure` doesn't restart — existing installs need the unit file
  re-copied once, a binary update alone won't touch it. `SupervisedBySystemd`
  (checks `INVOCATION_ID`) lets the UI say the truth instead of a confident
  "restarting…" when nothing will actually bring the process back.
- **Regenerate certificate** button (Settings → General, mirrors
  "Regenerate API key") for when the cert's SANs no longer match how the
  instance is reached (a DHCP IP change, say).
- **First-run setup wizard** gained an HTTPS step between TorBox and Done,
  showing the literal `https://<host>:<port>` URL to visit afterward — the
  restart doesn't move the current browser tab there on its own.
- **Verified live** on a scratch instance (never the real ones, same
  discipline as every other feature here): confirmed `showDirectoryPicker`
  is genuinely unavailable over `http://<real-lan-ip>` and available over
  `https://<real-lan-ip>` with the auto-generated cert (SANs correctly
  covering the actual interface IP, not just `localhost` — the easy way to
  get a false positive here); confirmed the regenerate-certificate button
  changes the cert on disk and an unsupervised restart correctly stops the
  process while saying so rather than claiming success. A real layout bug
  this surfaced: the HTTPS checkbox inherited a text-field's padding rule
  and rendered as an oversized box — fixed. See
  [Providers](docs/providers.md#tls-self-signed-https).
