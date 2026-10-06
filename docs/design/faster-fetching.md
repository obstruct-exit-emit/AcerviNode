# Faster fetching — design plan

**Status: plan only. Nothing here is built.** Tracked in
[ROADMAP.md](../../ROADMAP.md#faster-fetching--plan-first-not-started).

Every Managed download goes through the fetch path, so this plan is ordered
around one rule: **it must not be possible for this work to make downloading
worse.** Each stage ships switched off, with today's code path as the
switched-off branch, and is switched on only after it has proved itself on the
production instance.

---

## 1. Goal and non-goals

**Goal.** Fetch Managed downloads from the provider to local disk faster:

- many-file downloads (albums, season packs, audiobooks) by **≥3×**;
- single large files **no slower**, and **steadier** — no more single
  connections that crawl at 1–2 MB/s;
- with **identical bytes on disk**, **no new rate limiting**, and **no new
  failed imports**.

**Tuned for the fastest the system allows, capped by the operator.** Defaults
are set by where speed stops scaling or errors start — the TorBox CDN, TorBox's
API limits, the disk — not by any one line's bandwidth. Anyone who needs to
share the line sets a speed limit in Settings (§4.6), which is off by default.
(Decided by the operator: "if we optimize to be as fast as we can. we can make
a throttle in settings".)

**Non-goals for this work.**

- Resuming a partly-fetched *file* across attempts. Today a failed attempt
  refetches each unfinished file from the start; that stays. (Segmenting makes
  resume natural later — see §10.)
- Other engines (aria2, symlink, Download Station).
- Manual downloads fetched on demand from the UI. Their per-file path is a
  separate route and is left alone.
- AllDebrid. It keeps today's sequential behaviour until it has been measured
  (§4.7).

---

## 2. What we know

### Measured on production (burn-in, 2026-09/10)

| Download | Files | Size | Fetch | Speed |
|---|---|---|---|---|
| Night of the Living Dead (usenet) | 1 | 19.3 GB | 244 s | ~79 MB/s |
| Amphibia S01E37 (usenet) | 1 | 321 MB | ~5 s | ~64 MB/s |
| Pink Floyd, The Wall (torrent, FLAC) | 26 | 434 MB | ~56 s | ~7.7 MB/s |
| Avantasia 2CD (torrent, MP3) | ~24 | 261 MB | ~40 s | ~6.5 MB/s |

### Measured against the TorBox CDN (dev PC, same line, production busy)

A 5.5 GB file, 12 s per run. Absolute numbers are depressed by the production
instance fetching at the same time; the comparison is what counts.

| Run | 1 connection | 4 range requests |
|---|---|---|
| 1 | 45.3 MB/s | 53.5 MB/s |
| 2 | 1.4 MB/s | 29.2 MB/s |
| 3 | 15.2 MB/s | 31.9 MB/s |

- CDN host `*.tb-cdn.io`, **HTTP/1.1 only**; `Range` answered with `206`.
- RTT from here ~40–100 ms (TCP connect time).

### Conclusions

1. **The many-file slowdown is ours.** Files are fetched strictly one at a
   time, and each pays a TorBox `requestdl` round trip plus a fresh TCP
   connection in slow start. For a 17 MB FLAC track that fixed cost is most of
   its time.
2. **A single connection is erratic**, several are steady. Splitting a big file
   buys consistency first, peak second.
3. **Ruled out:** HTTP/2 flow control (the CDN does not speak HTTP/2; forcing
   HTTP/1.1 or enlarging the window changed nothing) and the 32 KB copy buffer
   (the CPU is not the limit at these rates — reasoning, not measured).

---

## 3. How fetching works today

The path a Managed download takes, and the guarantees each step gives. Any new
design has to keep every one of them.

| Step | Where | Guarantee it provides |
|---|---|---|
| Pool | `dispatch` / `startFetch` / `runFetch` | At most `max_concurrent_downloads` downloads in flight; the count and registration happen under one lock. A freed slot is refilled only after a success. |
| Context | `startFetch` | Each download has a `context.WithCancelCause`; `CancelFetch` cancels it with `errFetchCancelled` and waits up to 10 s for it to stop. |
| Outcome | `processReserved` deferred check | If the download's context was cancelled with `errFetchCancelled`, **whatever** error surfaced is reported as a removal, never a failure. |
| Failure | `runFetch` → `handleFailure` | One failed attempt = one retry count increment and one backoff, or `error` after `import_max_retries`. |
| File list | `p.Files` → `packedOnly` → `filterFiles` | Usenet left packed fails before a byte is fetched; samples and filtered files are dropped. |
| Files | the loop in `processReserved` | **Sequential**; the first file error fails the attempt. |
| One file | `fetchFile` | Skips a file already at its real path with the right size; writes to `.part` and renames only when whole; checks the byte count against `expectedTransferSize` (server `Content-Length` first, provider size second). |
| Stall | `idleTimeoutReader` + per-file timer | A connection silent for `import_fetch_timeout_seconds` is cancelled; that fails the attempt (and is retried at download level). |
| Link | `openFile` | One `RequestDownloadLink` per file attempt; a **refused** request (non-200) is retried twice with a fresh link; link-resolution errors (429 included) and transport errors are **not** retried here. |
| Progress | `progressWriter` → `SetFetchProgress` | Throttled to 2/s; measured against the files being fetched; never goes backwards; reaches exactly 100%. |
| Finish | `UpdateDownloadSavePath`, `ready_for_import` | Save path persisted before the state changes. |

Rate limiting today: a 429 from `requestdl` during a fetch fails that download's
attempt and backs **that download** off. It is not fed into the per-kind polling
cooldown. But TorBox counts every call from this account against one budget, so
a burst of link requests can make the **poller's** next call 429 — and the
poller's backoff blanks polling for the whole kind, which presents as everything
freezing. That indirect path is the main thing to protect against.

---

## 4. Design

### 4.1 Shape

Two independent knobs, one shared budget:

- **F** — files of one download fetched at once (`fetch_files_concurrently`).
- **S** — connections for one large file (`fetch_connections_per_file`).
- **G** — total download connections across everything (Stage 3).
- **L** — a speed limit across all fetching, in MB/s; **0 = unlimited**, the
  default (Stage 3, §4.6).

Both F and S **default to 1**, and at 1 the code takes today's path — kept as an
explicit branch, not reimplemented — so switching them off is an exact
rollback.

### 4.2 Stage 1 — several files at once (F)

**The change is confined to the file loop in `processReserved`.** `fetchFile`,
`openFile`, the `.part`/rename logic, the size check and the idle timer are
reused unchanged, one call per file, exactly as today.

- **Workers.** A bounded set of F goroutines takes files from a queue. Files
  are queued **largest first**, so the biggest file starts immediately and the
  download is not left waiting on one large file started last.
- **First error wins, once.** The workers share a child context made with
  `context.WithCancelCause` from the download's context. The first worker to
  fail records its error (under a `sync.Once`) and cancels the child with it;
  the others stop. The loop returns that **first** error — never a sibling's
  "context canceled" — so `handleFailure` sees the real cause exactly once.
  Hand-rolled (~30 lines); no new dependency for `errgroup`.
- **Removal still reads as removal.** A delete cancels the *download's*
  context, which cancels the child too. The deferred check in
  `processReserved` reads `context.Cause` of the download's context, so
  whichever worker's error surfaces first, the outcome is `errFetchCancelled`.
  This already works for one worker and needs a many-worker test, not new
  logic.
- **Stalls.** Each `fetchFile` keeps its own idle timer. A stalled file
  cancels only itself, returns an error, and that error fails the attempt —
  the same outcome as today, reached sooner if other files were still going.
- **Progress.** The per-file closure that adds a captured `fileDoneBytes`
  becomes one shared atomic byte counter. Each file reports its *delta*; on
  completion it tops up to its exact size, so the total lands on exactly 100%.
  A sum of non-negative deltas cannot go backwards.
- **Already-fetched files.** Unchanged: `fetchFile` still skips a file already
  complete at its real path, per file, so a retry refetches only what is
  missing.
- **Directories.** Concurrent `ensureWritableDir` on the same folder is safe
  (`MkdirAll` and `Chmod` are idempotent).
- **Link requests** go through the limiter (§4.4) before `RequestDownloadLink`.
- **Connections.** Fetches get their own `http.Transport` with
  `MaxIdleConnsPerHost` sized to the configured concurrency, so finished files
  hand their connection to the next instead of opening new ones (Go's default
  keeps only 2 idle per host).

Connection count in Stage 1 is bounded by `max_concurrent_downloads × F`
(3 × 4 = 12 on production at the likely default), so no global cap is needed
yet.

### 4.3 Stage 2 — one large file over several connections (S)

Only for a file whose **server-reported** size is at least a threshold
(starting point 256 MB), and only on a provider that allows it (§4.7).

- **Prove ranges first, waste nothing.** The first request is
  `Range: bytes=0-(chunk-1)`. The server must answer `206` with a
  `Content-Range` of exactly `bytes 0-(chunk-1)/TOTAL`. If it answers `200`,
  it is ignoring ranges: that same response is read as one plain stream, as
  today. Nothing is requested twice.
- **The server's TOTAL is the size.** Not the provider's: TorBox's sizes were
  verified exact for torrents and web downloads but **not for usenet**
  (`expectedTransferSize`'s own reasoning). Splitting on an estimate would
  write a file of the wrong length.
- **Dynamic chunks, not a fixed split.** The file is cut into fixed chunks
  (start at 32–64 MB) in a queue, and S connections each take the next chunk.
  A slow connection simply finishes fewer chunks. A fixed split into S parts
  would leave the file waiting on its slowest part — the exact erratic-
  connection problem measured in §2.
- **Every chunk is checked.** Accepted only on `206`, a `Content-Range` whose
  start, end and total match what was asked, and exactly that many bytes
  received. Anything else fails the chunk.
- **Writing.** One `.part` file, opened once, extended to TOTAL
  (`Truncate`, sparse), chunks written with `WriteAt` at their offsets. The
  `.part` → rename rule and the final size check are unchanged.
- **Links.** If Stage 0 shows one TorBox link serves concurrent ranges, all
  chunks of a file share one resolved link (no extra `requestdl` calls).
  Otherwise each connection resolves its own. A link refused mid-file (expired)
  is re-resolved through the existing refused-request retry.
- **Retry rules stay narrow, per chunk.** Exactly `openFile`'s rules: a
  refused request is retried with a fresh link; link-resolution errors and
  transport/stall errors fail the attempt. Retrying stalled chunks is tempting
  and cheap with a chunk queue, but it changes an invariant — left for a later
  decision, with data.
- **Progress** feeds the same atomic counter.
- **Crash or failure mid-file** leaves a `.part` that the next attempt
  truncates and refetches, exactly as today (`os.Create`).

### 4.4 Link-request limiter

A small token bucket per provider account, in the importer, taken before every
`RequestDownloadLink` call on the fetch path. Its rate is set from what Stage 0
measures, comfortably below the point where TorBox starts answering 429. It
spreads a burst of N files' link requests over time instead of firing them at
once — which protects the poller, not just the fetch.

Open decision (§9): whether a 429 seen while fetching should also start the
kind's polling cooldown. Today it does not.

### 4.5 Stage 3a — one global connection budget (G)

With segments, the count multiplies: downloads × files × connections, 3 × 4 × 4
= 48 at likely defaults. A single semaphore caps **connections**, acquired by
whatever opens one (a file in Stage 1, a chunk in Stage 2).

- **Only leaves acquire.** A file worker that splits into chunks does not hold
  a slot while its chunks wait for theirs. Nested acquisition is how a
  semaphore deadlocks — all slots held by parents waiting on children — so it
  is excluded by construction.
- **Fairness.** F and S still cap one download, so a 26-file pack cannot take
  every slot from the download queued behind it.

### 4.6 Stage 3b — the speed limit (L)

One limit for **all** fetching together, not per connection or per download —
"use at most 40 MB/s" is what an operator means, whatever is running.

- **A shared byte budget.** One token bucket refilled at L bytes/s, drawn
  from by every fetch connection before it writes what it read. Small draws
  (each read's worth), so connections share the budget smoothly instead of one
  taking a large burst while the others wait.
- **Off means off.** L = 0 bypasses the bucket entirely — no lock, no wait —
  so the default costs nothing.
- **Live.** Changing L in Settings applies to connections already running, so
  turning the limit down takes effect at once rather than on the next download.
- **The stall timer must not fire on throttling.** A throttled connection is
  slow on purpose. The idle timer measures time without bytes *from the
  server*; time spent waiting on the bucket is not the server being silent and
  must not count. The wait happens after a read has already reset the timer,
  and the timer is paused for the wait. Without this, a tight limit across many
  connections would make every one look stalled and fail every download — the
  exact way this work could break downloading.
- **Fair across downloads.** The bucket is first-come per read, and every
  connection reads in small pieces, so no download starves another. Measured in
  tests rather than assumed.
- **Shown where it matters.** Settings states the limit is shared across all
  downloads; the dashboard's per-download speed already shows its effect.

### 4.7 Providers

A per-provider allowance for F and S, so a setting cannot switch on
parallelism for a provider nobody has measured:

- **TorBox** — torrent, usenet and webdl: allowed once Stage 0 has measured them.
- **AllDebrid** — F = S = 1, today's behaviour. Its `/v4/link/unlock` per file
  has its own unmeasured limits, and it has no zip or range evidence yet.

### 4.8 Settings

| Setting | Default at ship | Default after live proof | Notes |
|---|---|---|---|
| `fetch_files_concurrently` | 1 (today) | Stage 0 decides, likely 4 | live-editable |
| `fetch_connections_per_file` | 1 (today) | Stage 0 decides, likely 4 | live-editable |
| `fetch_speed_limit_mbps` | 0 (unlimited) | 0 (unlimited) | live-editable; shared across all fetching |
| segment threshold, chunk size, link rate | — | — | internal constants, not settings; chosen from Stage 0 data |

Three knobs, not six. Every setting added is one more thing an operator can set
wrong; the global connection cap is derived, not a fourth setting.

### 4.9 Observability

Needed to prove the change and to notice a regression later:

- The existing `importer: download ready` log line gains duration, bytes and
  MB/s, and the F/S it ran with.
- A 429 from `requestdl` is logged as such (it is today folded into a generic
  fetch failure).

Comparing before/after then needs no special tooling: the same download's log
line at each setting.

---

## 5. Risk register

| Risk | What goes wrong | Guard | Proven by |
|---|---|---|---|
| Wrong bytes | A server ignoring `Range` returns the whole file to every chunk | Chunk accepted only on `206` + exact `Content-Range`; `200` falls back to one stream | Fake CDN that ignores ranges; one that returns a shifted range |
| Short file | A chunk ends early | Exact byte count per chunk; total checked before rename | Fake CDN that drops a chunk mid-way |
| Partial file visible | Import sees a half-written file | `.part` + rename, unchanged | Existing test, ported to parallel |
| Double failure / wrong cause | N workers each report, or a sibling's "canceled" is reported | First error wins under `sync.Once`; one `handleFailure` | Two files failing at once → retry count +1 with the first error |
| Delete reads as failure | Removal mid-fetch logged "will retry" | Outcome read from `context.Cause` of the download context | `TestTick_DeliberateCancelIsNotRecordedAsAFailure`, many-worker version |
| Orphan files after delete | A worker writes after the delete | `CancelFetch` waits for `done`, which closes only after every worker returned | `TestCancelFetch_StopsInFlightFetch`, many-worker version |
| Stall masked | A dead connection hidden by busy siblings | Per-connection idle timer, unchanged | `TestFetchFile_TimesOutOnSlowTransfer`, with a busy sibling |
| Rate limiting | Burst of `requestdl` → 429 → poller backs off the whole kind | Limiter (§4.4) sized from Stage 0 | Live: zero new 429s; status `error_count` flat |
| Too many connections | Line saturated; CDN refuses | F × S per download; G globally (Stage 3); no nested acquisition | Concurrency counter in the fake CDN never exceeds the cap |
| Progress wrong | Goes backwards, overshoots, or stops short of 100% | Shared atomic delta counter, top-up on completion | Progress property test across random F/S and file sizes |
| Disk thrash | Many writers on a spinning disk slower than one | Stage 0 disk measurement chooses defaults | Live before/after on the real storage |
| Retry semantics drift | Per-chunk retries widen what is retried | Same narrow rules as `openFile` | Existing retry tests, ported per chunk |
| Throttle trips the stall timer | Every slow-on-purpose connection looks dead; every download fails | Bucket wait excluded from the idle timer; timer paused while waiting | Tight limit (e.g. 1 MB/s) across many connections with a short idle timeout: all complete |
| Throttle unfair | One download takes the whole budget | Small per-read draws from one shared bucket | Two downloads under a limit each get a share |
| Throttle overhead | Unlimited default slowed by locking | L = 0 bypasses the bucket entirely | Benchmark: L = 0 path allocation- and lock-free |
| Untested provider | AllDebrid parallelised blind | Per-provider allowance, AllDebrid at 1 | Unit test on the allowance |
| Usenet size estimates | Split on a wrong size | Server's `Content-Range` total is the size | Fake CDN whose total differs from the provider's size |

---

## 6. Test plan

All test-first; every stage mutation-checked (each guard removed must fail a
test, and every mutant must compile); all with `-race`.

**A configurable fake CDN** (`httptest`) is the core fixture: serves files with
real `Range` support and knobs for — ignoring ranges, returning a shifted or
wrong-total range, stalling a given connection, dropping a connection
mid-chunk, refusing the first request (expired link), slow-but-steady
connections — and it counts concurrent connections and `requestdl` calls.

**Byte-identity property test.** Random file sets (counts, sizes including 0
and sizes around chunk boundaries) × random F and S: every file's SHA-256 must
equal the source. The single most important test.

**Ported existing tests**, each gaining a many-worker version:
`TestProcessDownload_ProgressNeverGoesBackwardsBetweenFiles`,
`TestProcessDownload_ReportsLiveFetchProgress`,
`TestTick_DeliberateCancelIsNotRecordedAsAFailure`,
`TestTick_StalledFetchStillRetries`, `TestCancelFetch_StopsInFlightFetch`,
`TestTick_SkipsAlreadyDownloadedFiles`,
`TestFetchFile_RetriesARefusedFileWithAFreshLink`,
`TestFetchFile_GivesUpAfterRepeatedRefusalsAndSaysWhy`,
`TestFetchFile_StalledServerIsNotRetriedPerFile`,
`TestFetchFile_ServerLengthIsTrustedOverAnInexactProviderSize`,
`TestFetchFile_RejectsAShortChunkedBody`, and the fetch-pool tests.

**New tests.** First error wins and is recorded once; F and S never exceeded;
G never exceeded with no deadlock under contention; limiter spreads link
requests; F = S = 1 issues exactly today's sequence of requests (the rollback
guarantee, pinned); the throttle holds aggregate throughput within a few
percent of L across many connections, applies a live change at once, never
trips the idle timer, and costs nothing at L = 0.

---

## 7. Stage 0 — measure on the production host first

Defaults are chosen from data taken **on the Proxmox host, while idle**, not
from the dev PC. A small probe binary (built from this repo, run by hand)
fetches an already-cached download's link through the native API, then:

1. **Where scaling stops.** One file: 1, 2, 4, 8, 16 connections × 30 s each.
   The default is the point where more connections stop adding speed or start
   adding errors — whatever the cause (CDN, TorBox, the host). The line is not
   a design input: the speed limit is.
2. **Concurrent ranges on one link.** Does one resolved TorBox link serve
   several ranges at once, or does the CDN refuse/limit a second connection?
3. **Many files.** A real cached album: F = 1, 2, 4, 8. Time and errors.
4. **Link requests.** How many `requestdl` calls in a short burst before a 429?
   Check TorBox's published limits first; then probe gently (5, 10, 20 calls,
   stopping at the first 429), outside busy hours, watching
   `GET /api/v1/status`. A 429 here is the one probe step with a real cost —
   it can briefly back off polling — so it goes last and stops early.
5. **Disk.** `/storage_1` and `/storage_2`: sequential write with 1, 4, 8
   concurrent writers.

Recorded in this document before Stage 1 code starts.

---

## 8. Rollout

Each stage: build test-first → ship with its setting at 1 (no behaviour change)
→ deploy → live verification → raise the default → watch a few days of real
traffic.

**Live verification per stage.** Fetch the same cached download at the old and
the new setting; compare every file's SHA-256 between the two runs, the time,
`GET /api/v1/status` error counts, 429s in the log, and that Sonarr/Radarr/
CantiNode/LibriNode still import it.

**Rollback** is the setting back to 1 — today's code path — with no redeploy.

**Order:** Stage 0 → Stage 1 (files) → Stage 2 (segments) → Stage 3a (global
cap) and 3b (speed limit). Stage 1 delivers most of the measured gain and
touches the least.

The speed limit does not depend on Stages 1–2 and could ship first, on today's
single stream, if an operator needs it sooner. Its one hard requirement — not
tripping the stall timer — applies identically either way.

---

## 9. Open questions

**For the operator** (answers change defaults, not the design):

- What `/storage_1` and `/storage_2` are: local HDD, SSD, ZFS pool, NFS/SMB
  mount? Sparse files and `WriteAt` behave very differently on a network mount.
- Is AllDebrid in active use?

**Design decisions to take with Stage 0 data:**

- Should a `requestdl` 429 seen while fetching also start the kind's polling
  cooldown? It protects the account but freezes polling — the symptom we are
  trying to avoid.
- Retry a stalled chunk within the attempt (cheap with a chunk queue) or keep
  today's rule that a stall fails the attempt?
- Chunk size and segment threshold.

---

## 10. Alternatives considered

- **TorBox zip link** (one `requestdl` with `zip_link=true` returns the whole
  torrent as one `.zip`; verified live earlier, used by the UI's "download
  everything"). One connection instead of N — but it needs local extraction,
  doubling disk use and adding a step that can fail; behaviour on very large
  packs is unknown; usenet/webdl support unverified; AllDebrid has nothing
  equivalent. Rejected for the import path.
- **aria2c handoff.** Mature segmented downloader, but an external daemon to
  install, secure and supervise. Stays a roadmap option, not this work.
- **HTTP/2 tuning.** Ruled out by measurement — the CDN is HTTP/1.1.
- **Larger copy buffer.** Not the bottleneck; may be folded into Stage 1 as a
  harmless change, but not pursued for speed.
- **Resume of partial files across attempts.** Natural with chunks (a sidecar
  record of finished chunks), but it adds on-disk state that must survive
  crashes correctly. Deferred until Stages 1–2 are proven.
