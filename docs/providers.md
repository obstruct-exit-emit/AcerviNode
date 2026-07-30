# Providers

AcerviNode talks to debrid services through two small interfaces in
`internal/debrid`. Every add, status check, and link resolution a compat shim
performs goes through one of these — never through a concrete provider package
directly.

## `TorrentProvider`

Backs the [qBittorrent shim](qbittorrent-api.md). Methods:

- `Name() string`
- `AddMagnet(ctx, magnetURI string, opts AddOptions) (ProviderDownloadID, error)`
- `AddTorrentFile(ctx, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error)`
- `Status(ctx, id ProviderDownloadID) (DownloadStatus, error)`
- `List(ctx) ([]DownloadStatus, error)`
- `Files(ctx, id ProviderDownloadID) ([]DownloadFile, error)`
- `RequestDownloadLink(ctx, id ProviderDownloadID, fileID string) (url string, error)`
- `Delete(ctx, id ProviderDownloadID, deleteFiles bool) error`
- `CheckCached(ctx, hashes []string) (map[string]bool, error)` — providers without
  a cache-check endpoint may return all-`false` rather than implement real logic

## `UsenetProvider`

Backs the [SABnzbd shim](sabnzbd-api.md). Same shape, NZB-flavored:

- `Name() string`
- `AddNZBFile(ctx, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error)`
- `AddNZBURL(ctx, url string, opts AddOptions) (ProviderDownloadID, error)`
- `Status`, `List`, `Files`, `RequestDownloadLink`, `Delete` — identical semantics
  to `TorrentProvider`'s versions

This is a **separate, optional** interface on purpose: not every debrid service has
a real usenet backend. A provider package implements it only if the service
genuinely supports NZB downloads. `cmd/acervinode`'s `newTorBoxProviders` function is
the one place that knows which concrete constructors exist for the configured
provider name — TorBox exports both `torbox.NewProvider` (torrents) and
`torbox.NewUsenetProvider` (usenet). A torrent-only provider like Real-Debrid would
only export the first — see [Live settings](#live-settings) below for what happens
to the SABnzbd shim in that case (it stays mounted either way, now — it just
answers with an error until a usenet-capable provider is configured).

TorBox's torrent and usenet capabilities are two separate types on purpose, not one
type satisfying both interfaces at once, even though their method sets could
technically overlap (`Status`, `List`, `Files`, etc. have identical signatures
across both interfaces): TorBox's torrent IDs and usenet download IDs are separate,
provider-assigned numeric spaces with no guarantee against collision, so a single
`Status(ctx, id)` couldn't safely tell which service `id` belongs to. Keeping them
as `torbox.Provider` and `torbox.UsenetProvider`, each wrapping the same underlying
`torbox.Client`, avoids that ambiguity.

## `WebDownloadProvider`

Backs `POST /api/v1/downloads/webdl` directly — there's no *arr-facing compat shim
for this one (no protocol Sonarr/Radarr speaks maps onto "debrid a direct hoster
link"), so it's only reachable through the native API/web UI. Same shape as
`UsenetProvider`, minus a file-upload variant (a hoster link is always a URL):

- `Name() string`
- `AddLink(ctx, link string, opts AddOptions) (ProviderDownloadID, error)`
- `Status`, `List`, `Files`, `RequestDownloadLink`, `RequestZipDownloadLink`,
  `Delete` — identical semantics to `TorrentProvider`/`UsenetProvider`'s versions

Also a **separate, optional** interface for the same reason `UsenetProvider` is:
not every debrid service exposes a generic "debrid any hoster link" service the
way TorBox's Web Downloads does. Every `downloads` row of this kind is always
`AddedViaManual` — there's no *arr-facing shim that could add one on an app's
behalf, so `database.KindWebDL` documents that directly rather than leaving it
implicit (see [Managed vs. Manual](#managed-vs-manual)).

## `AccountProvider`

A third, separate, optional interface — one method, `Account(ctx) (AccountStatus,
error)` — for a provider that can report its own account status (plan tier,
subscription state, premium expiry, lifetime bytes downloaded). Backs
`GET /api/v1/settings/account` (see [API](api.md)) and the Settings page's TorBox
account section. Not every provider needs to implement it; one that doesn't simply
doesn't satisfy the interface, and the settings UI has nothing to show for it — the
same "structural, not every provider needs every capability" approach as
`UsenetProvider`/`WebDownloadProvider`.

`debrid.DynamicTorrentProvider.Account` is the one place this gets a little
different from the other two Dynamic wrappers: rather than a whole separate
`DynamicAccountProvider` type, it type-asserts its already-`Set()` inner provider
against `AccountProvider` and delegates if it matches, erroring otherwise. This
reuses the existing live-provider-swap machinery instead of adding a fourth
parallel wrapper for one read-only call.

## What the interfaces deliberately leave out

Categories and save paths are a compat-shim/database concern, not a provider
concern — \*arr apps set them purely to know which local path to watch for
completed imports, and AcerviNode stores them directly on the `downloads` row. The
provider interfaces stay protocol-agnostic.

## Live settings

Provider credentials can be set or changed two ways: hand-editing
`providers.torbox.api_key` in `config.yaml` (restart required, the original
mechanism), or `PUT /api/v1/settings/providers/torbox` (the web UI's Settings tab
uses this) — no restart needed. Both end up in the same place.

This works because `cmd/acervinode` never hands a concrete provider directly to
the compat shims, the importer, or the native API — it hands each of them the
same `*debrid.DynamicTorrentProvider`/`*debrid.DynamicUsenetProvider` instance
(`internal/debrid/dynamic.go`), which implements the real provider interfaces by
delegating to whatever's currently `Set()`. Both compat shims are **always**
mounted now (not conditionally on a provider existing at startup) — before a key
is set, every provider-backed call just returns `debrid.ErrNoProvider` instead of
the route not existing, which is what makes configuring TorBox for the first time
through the settings API (not just at startup) possible at all. `cmd/acervinode`'s
`liveSettings` type is what `PUT` actually calls: it swaps the Dynamic wrappers'
inner provider and calls `config.Save` to persist the change, all under one mutex
so concurrent settings changes don't race.

The settings API is deliberately narrow — `SetTorBoxAPIKey`, not a generic
"configure any provider" endpoint — since TorBox is the only provider that exists.
Generalize this (and `Settings` in `internal/api`) when a second provider is added.

## Completed Download Handling (`internal/importer`)

Neither interface has a "download the bytes to disk" method — that's deliberately
one level up, in `internal/importer`, built entirely on `List`, `Files`, and
`RequestDownloadLink`, which both interfaces already provide. A background loop
(`Importer.Run`, ticking every `import_interval_seconds`) does two things every
tick: refreshes every tracked download's state from its provider
(`refreshStatuses`, see below), then finds every `downloads` row now in
`provider_completed` state and resolves each file's real link, streaming it over
plain HTTP to `save_path` (or `download_dir` as a fallback) — the same thing a
normal download client does, just sourced from a debrid CDN link instead of
BitTorrent/NNTP. A row only reaches `ready_for_import` once its files are actually
on disk; both compat shims report `provider_completed` as still "downloading" to
\*arr apps for exactly this reason — see [qBittorrent API](qbittorrent-api.md) and
[SABnzbd API](sabnzbd-api.md).

This works identically for any future provider, torrent or usenet, with zero
changes — it only depends on `List`/`Files`/`RequestDownloadLink`, which every
provider already has to implement.

### Proactive status refresh

Both compat shims sync a download's local state against the provider
*reactively* — only when an \*arr app happens to call `GET /api/v2/torrents/info`
or `mode=queue`. On its own, that meant a download's state only ever advanced
when something external polled one of those endpoints; watching only the native
API or web UI (neither of which touches a provider at all) could leave a
finished download looking permanently "queued", and even an actively-polling
\*arr app only caught up on its own poll cadence.

`Importer.refreshStatuses` closes that gap: every tick, it calls each configured
provider's `List` for both kinds and applies the result via
`database.RefreshFromProvider` — the exact same sync logic both compat shims'
`refreshFromProvider` call, now shared in one place (`internal/database`) instead
of duplicated per shim, so all three interpret a provider's state identically.
Because this runs on `import_interval_seconds` regardless of external polling, a
download that finishes between polls — or with nothing polling at all — is
picked up within one tick, and if that same tick moves it into
`provider_completed`, its files get fetched immediately after, in the same
`Tick` call. `List` errors are logged, except `debrid.ErrNoProvider` (no key
configured yet), which is expected and would otherwise spam the log every tick.

This does **not** shrink whatever delay exists on the provider's own side — TorBox's
`mylist` (even with `bypass_cache=true`, see below) has been observed taking a few
minutes to index a brand-new torrent, independent of how it's polled. What this
closes is AcerviNode's own contribution to the delay: previously a finished
download could sit unnoticed indefinitely with nothing polling; now it's picked
up within one `import_interval_seconds` tick of the provider actually reflecting
it, guaranteed.

A fetch that fails (a `Files`/`RequestDownloadLink` call error, or the HTTP
download itself failing) doesn't retry on every subsequent tick forever, and
doesn't retry instantly either: `Importer.handleFailure` records the failure and
schedules the next attempt with exponential backoff — attempt *N* waits
`import_interval_seconds`×2^*N*, capped at one hour — stored on the row as
`retry_count`/`next_retry_at` (`Tick` only picks up rows whose `next_retry_at`
has passed). Once `retry_count` reaches `import_max_retries`, the download is
moved to `error` instead of scheduled again, so a permanently-broken link stops
occupying a retry slot forever rather than silently never finishing. Both fields
are surfaced on `GET /api/v1/downloads/{id}` — see [API](api.md) — and shown in
the web UI's detail view. This give-up is sticky by design —
`database.RefreshFromProvider` won't silently resurrect it back to
`provider_completed` just because the provider still reports its old
"completed" state on a later poll (`retry_count > 0` is what distinguishes a
local give-up like this one from a provider-reported error — see
[State mapping](#state-mapping) below, where the opposite is true: those
recover automatically).

A download that gave up isn't stuck forever, though: `POST
/api/v1/downloads/{id}/retry` resets it back to `provider_completed` with
`retry_count` cleared, so the next tick attempts it again from scratch — the
manual counterpart to the automatic backoff above, for when the underlying
cause (a transient rate limit, the provider being briefly down) has since
cleared. The web UI's detail view shows a "Retry" button once a download is
in `error` state.

Sometimes retry alone isn't enough, though — confirmed live: a torrent that
kept failing with "not found" turned out to have expired from TorBox's own
`mylist` entirely, not a transient fetch problem retry could recover from.
For that, `POST /api/v1/downloads/{id}/readd` resubmits the download's
original magnet/NZB URL (stored as `Download.Source` at add time, for
link-based adds only — nothing's kept for an uploaded file) to the provider
as a genuinely new add, then points the local row at the new
`provider_download_id`. The web UI shows both "Retry" and "Re-add" side by
side once a download is in `error` state.

A download's files don't need to be fetched to local disk at all to be
usable: `GET /api/v1/downloads/{id}/files/{fileId}/link` resolves a direct,
provider-hosted URL for one file — the exact same `RequestDownloadLink` call
`fetchFile` above makes, just handed straight back to the caller instead of
being streamed to disk. Always a live provider call, never cached — see
[API](api.md#direct-file-downloads). This also meant `GET /api/v1/downloads/{id}`
needed a real file list to attach a link to, which surfaced a genuine
pre-existing bug: the local `download_files` table it read from was defined
but nothing ever populated it, so `files` was always `[]` in practice, even
for a fully completed download. Fixed by having it query the provider live
too, the same way `internal/qbittorrent`'s own file listing already did —
see CHANGELOG.

### Provider rate-limit backoff

`refreshKind`'s own `List` call, before this, retried on every single tick
regardless of *why* the previous attempt failed — including a provider rate
limit, where retrying immediately is actively counterproductive (each retry
itself can count against the same rate-limit window, extending how long it
takes to clear). Found not hypothetical: a burst of manual live testing
sustained a real TorBox `rate limit exceeded (status 429)` for several
minutes straight, with the then-current behavior doing nothing to help it
recover.

`debrid.ErrRateLimited` is a provider-agnostic sentinel error a concrete
provider's error chain can include (via `errors.Is`/`%w`) whenever a call
failed specifically because of a rate limit — `torbox.APIError.Unwrap`
resolves to it for a `429` status specifically, nothing else, so this stays
recognizable through however many `fmt.Errorf("...: %w", err)` layers wrap
it on the way up (every provider adapter method does exactly that).
`internal/importer` doesn't need to import `internal/debrid/torbox` or know
about `*APIError` at all to check for it.

When `refreshKind` sees `debrid.ErrRateLimited` from `List`, it records a hit
for that kind (torrent/usenet/webdl each have independent backoff state — a
rate limit on one shouldn't pause polling for the others) and skips calling
`List` again for that kind entirely until a cooldown passes, rather than
retrying every tick regardless. The cooldown grows exponentially per
consecutive hit (30s, 60s, 120s, …), capped at 5 minutes — deliberately a
much shorter cap than the per-download fetch-retry backoff's 1-hour ceiling,
since a rate limit is a short-lived, provider-side condition (typically a
per-minute window), not something that plausibly needs an hour before it's
worth trying again. A successful `List` call clears the backoff state
entirely, so the next rate limit (if any) starts counting from scratch.
Purely in-memory, not persisted — this is operational backoff state, not
something that needs to survive a restart the way a download's own
`retry_count` does.

### Managed vs. Manual

Not every download should be auto-fetched to local disk. An *arr app strictly
needs that — its own import step scans `save_path` and finds nothing if the
files aren't actually there — but a download added directly (through the web
UI's own "+ Add" form, or sitting in the provider's account entirely outside
AcerviNode) has no such requirement; the point of adding it that way is
usually to browse/grab files on demand, the way TorBox's own web UI works,
not to have it silently land on disk.

`database.Download.AddedVia` is the permanent, immutable record of which of
the two a given download is — set once at insert time, from *how* it was
added, never changed afterward:

- **`arr`**: added through the qBittorrent or SABnzbd compat shim — i.e. by
  an *arr app. Auto-fetched by Completed Download Handling like always. Shown
  in the web UI's **Managed** tab.
- **`manual`**: added directly via the native API's add endpoints (the web
  UI's own "+ Add" form — an *arr app has no way to reach that endpoint, it
  only knows the compat shims), or *discovered* — see below. Never
  auto-fetched; `ListDownloadsDueForRetry` filters to `arr` only, so a manual
  download sitting in `provider_completed` just stays there, and the user
  grabs files on demand via the per-file/zip-link endpoints (see
  [Direct file downloads](api.md#direct-file-downloads)) instead. Shown in
  the web UI's **Manual** tab, which is also the only place those manual-grab
  buttons appear at all — a Managed download is already being auto-fetched,
  so the UI doesn't offer a redundant manual download for it (the endpoints
  themselves don't restrict by AddedVia; this is purely a web UI choice).
  Retry isn't offered for a manual download in `error` state — there's no
  local fetch attempt to retry, the row is just reflecting the provider's own
  live state (see [State mapping](#state-mapping) above for how it gets
  there) or the vanish-detection feature's own conclusion (see below). Re-add
  *is* offered, though, whenever `has_source` is true — see
  [Re-add for a discovered download](#re-add-for-a-discovered-download) and
  [Re-add for a file-uploaded NZB](#re-add-for-a-file-uploaded-nzb-not-discovered)
  below for where that source can come from even for a Manual download.

**Discovery** is what makes an item added directly through TorBox's own
site/app — not through AcerviNode at all — show up in Manual too, not just
items added through AcerviNode's own "+ Add" form. Every tick,
`Importer.discoverManual` diffs the same provider `List()` call
`refreshStatuses` already makes against what's locally tracked (by
`provider_download_id`); anything present at the provider with no local row
at all gets adopted as a fresh `manual` download.

The one wrinkle: the very first time this runs for a given provider+kind,
nothing is adopted. Every currently-unmatched item is instead recorded into
`discovery_baseline` (with `discovery_seeded` as the per-provider-per-kind
marker that seeding has already happened) and permanently ignored — this is
what stops the feature from flooding the Manual tab with an account's entire
pre-existing history the moment it ships. Only items that show up
*afterward* — added to TorBox at any time from then on, whether through
AcerviNode or directly — are ever adopted.

A discovered download has no add-request `Source` to capture the normal way
(there was never a request through AcerviNode's own add endpoints for one),
but it isn't always stuck with an empty one either — see
[Re-add for a discovered download](#re-add-for-a-discovered-download) below.

**A just-deleted download can't be immediately re-discovered as a ghost.**
Confirmed live as a real race, not just a theoretical one: a provider's own
delete isn't always instantly reflected in its own listing endpoints (TorBox's
`mylist` could still briefly show a torrent right after its delete call
returned success), and `discoverManual` runs on its own schedule, independent
of any specific delete request. A tick landing in that narrow window would
otherwise see the still-technically-present item with no local row anymore
and adopt it fresh — a ghost Manual download for something a user just
intentionally removed, showing as "Available" when it genuinely isn't.
`handleDeleteDownload` (`internal/api`) now tombstones every real delete
(`database.RecordDeletedDownload`) before removing the local row;
`discoverManual` skips adopting anything tombstoned within
`recentlyDeletedGracePeriod` (5 minutes, generous on purpose — a
`provider_download_id` that's genuinely gone never legitimately reappears,
since a fresh add always gets a new one, so this only ever blocks
re-adopting the exact same now-defunct id). Tombstones older than the grace
period are pruned opportunistically on every new one recorded, rather than
needing a separate cleanup job.

**Category is deliberately not offered for Manual downloads** — no input on
the web UI's "+ Add" form, no Category column in the Manual tab, no Category
row in the detail view (the native API's add endpoints still *accept* an
optional `category` for programmatic callers, but it has no effect for a
Manual download and the UI doesn't ask for one). Category only drives real
behavior for a Managed download — it's what `category_paths` save-path
overrides key on (see [Configuration](configuration.md#categories-and-save-paths))
— and Manual downloads are meant to mirror TorBox's own web UI, which has no
categorization concept at all. Brainstormed with the user and left as a 💡
item in ROADMAP.md to revisit if the Manual tab ever gets hard to navigate.

### Retention/cleanup policy

Nothing removed a completed download automatically before this — every
`ready_for_import` Managed download (and its local files) sat around
forever unless a user manually deleted it, which is fine for a short-lived
test instance but not for a daily driver: local disk usage and the
`downloads` table both grow without bound. `cleanup_after_days`
(`config.Config`, 0 by default — the only setting in this config where 0 is
a meaningful, valid "off" rather than something `Validate` rejects) enables
`Importer.cleanupOldDownloads`, which `Tick` runs last, after status refresh
and fetching are both done for that tick.

**Deliberately scoped to Managed + `ready_for_import` only**
(`database.ListDownloadsEligibleForCleanup`, `WHERE state = 'ready_for_import'
AND added_via = 'arr' AND completed_at < cutoff`) — that combination means an
\*arr app has already imported the download elsewhere, so AcerviNode's own
local copy (and the debrid quota its provider-side copy is still occupying)
is redundant storage at that point. A Manual download in the analogous
`provider_completed` "available" state is never a candidate, on purpose —
that's the ongoing state for something the user hasn't grabbed yet, and
auto-deleting it would mean deleting something before it was ever used.
`completed_at` (not `added_at`) is the age reference, since that's when the
download actually finished importing, not when it was first added.

For each eligible row, `cleanupDownload`: removes the local files
(`os.RemoveAll` on the same directory `resolveDestDir` computed when
fetching them — an *arr app's own explicit `save_path` always wins,
otherwise a category override or `download_dir`/category, always namespaced
by the download's own name), skipped with a warning rather than attempted
if the row has no `Name` at all — `resolveDestDir` would otherwise collapse
to the bare category directory shared with every other download in it, and
`os.RemoveAll` on that would be far more destructive than intended; then
best-effort deletes the provider-side download; then records a delete
tombstone and removes the row — the exact same
`database.RecordDeletedDownload` race-avoidance a user-initiated delete gets
(`internal/api`'s `handleDeleteDownload`), since this runs on `Tick`'s own
independent schedule, the same as discovery.

### Proactively detecting a vanished Manual download

For a Managed download, a provider item vanishing self-corrects within a few
ticks: `internal/importer`'s own fetch attempt fails, retries, and eventually
lands in `error` with a clear reason. A Manual download is never in that
fetch-retry path at all (it's never auto-fetched — see above), so nothing
would otherwise notice at all — the row would just sit there looking done
until the user actually clicked download and hit the error live (`files_error`
— see [API](api.md)).

`RefreshFromProvider` (`internal/database/downloads.go`) now catches this
proactively too, for both `internal/importer`'s own ticks and each compat
shim's reactive polling (same shared function, all callers benefit). A row
whose `provider_download_id` is missing from a *successful* provider listing
(`p.List()` itself failing — e.g. a rate limit — doesn't count as a miss;
`refreshKind` already skips calling `RefreshFromProvider` at all on a listing
error) increments `downloads.missing_count`; once that reaches
`missingDetectionThreshold` (3, not user-configurable — a debounce
implementation detail, not a tuning knob) consecutive misses, the row is
flagged `error` with a fixed reason (`"no longer found in the provider's
account"`), and `missing_count` resets to 0 the instant the row is seen again
in any listing before then.

**Why a debounce at all, not a single-miss rule:** a row only starts being
tracked once it was already visible to the provider somehow — either an
immediate `Status()` call right after adding it (`handleAddWebDownload` etc.),
or already present in a `List()` response at discovery time
(`discoverManual`) — but TorBox's own listing endpoints have shown brief
eventual-consistency gaps around exactly that boundary elsewhere in this
project (the hash/name backfill above is a direct example: a fresh add's
first snapshot can be provisional). A single-miss rule risked wrongly flagging
a download that was still genuinely there.

**Deliberately scoped to `AddedViaManual` only** — a Managed row is never
touched by this mechanism (`missing_count` stays 0 for it always), since its
own fetch-retry path already covers the same scenario with a more specific
reason. **Deliberately not sticky**, the same way a provider-reported error
isn't (see [State mapping](#state-mapping)): `missing_count`'s threshold path
never touches `RetryCount`, so if the provider reports the download again on
a later poll, `RefreshFromProvider`'s own
`d.State == StateError && d.RetryCount > 0` stickiness check doesn't apply,
and it self-heals with no special-case code. A row already `error` for some
other reason (e.g. a genuine provider-reported failure) is left alone by this
path entirely, so it never overwrites a more specific existing reason.

**Mass-vanish circuit breaker.** The debounce above only protects against
one item briefly disappearing from an otherwise-normal listing — it does
nothing to distinguish a genuine mass-delete from a provider listing that
came back successful but anomalously empty or truncated (a partial
provider-side outage, a transient backend bug). Nothing about a successful
HTTP response tells `RefreshFromProvider` the difference; without a
safeguard, every currently-tracked Manual download would independently
cross `missingDetectionThreshold` within the same few ticks and all get
flagged at once. `isSuspectedMassVanish` runs once per `RefreshFromProvider`
call (not per row, since a real mass-vanish would affect the whole batch
identically): if at least `massVanishMinTracked` (3) `AddedViaManual` rows
are tracked for the kind, and more than `massVanishFraction` (50%) of them
are missing from this listing, the entire pass is treated as suspicious —
`handleMissingFromProvider` is skipped for every row in it, not just the
ones that look questionable, and a warning is logged instead. Rows that
*are* found in the same pass still update normally; only the missing-side
detection is suppressed. `massVanishMinTracked` exists so a small account
(one or two Manual downloads) isn't permanently exempt from real
vanish-detection just because a small absolute count happens to look like a
large fraction.

### Re-add for a discovered download

Retry/Re-add aren't gated on `added_via` at all on the backend (see
`internal/api`'s `handleReAddDownload`) — only on `state == error` and a
non-empty `Source`. The frontend used to only ever show them for a Managed
download, back when a Manual one could never actually reach `error` in the
first place; now that the vanish-detection feature above can put a Manual
download there too, the web UI shows Re-add (not Retry — there's still no
local fetch to retry for Manual) for *any* download in error state that has
one, gated on a `has_source` field (`GET /api/v1/downloads[/{id}]`) rather
than a blind `added_via` check.

The remaining question is where a **discovered** download's `Source` — never
captured the normal way, since there was no add request through AcerviNode to
capture a link from — comes from at all. `debrid.DownloadStatus.OriginalURL`
is the answer, and it means something different per kind, confirmed live
against real account data for each:

- **Torrent**: always a reconstructed magnet, `magnet:?xt=urn:btih:<hash>` —
  a torrent client/debrid service resolves the rest (name, trackers, files)
  from DHT/trackers on its own, so nothing needs to come from TorBox at all.
  Confirmed live that TorBox itself doesn't reliably record an original
  magnet anywhere retrievable (a real magnet-added torrent's `mylist` entry
  had both its `magnet` *and* `original_url` fields `null`), so this doesn't
  rely on it — the hash alone is always enough, and a torrent always has one
  once indexed (`magnetFromHash`, empty only if `hash` itself is still
  empty — e.g. mid-indexing at discovery time).
- **Usenet/webdl**: TorBox's own `mylist` response for both services
  includes an `original_url` field — confirmed live for both: a real
  usenet download submitted via an uploaded `.nzb` file had it `null`
  (nothing to record — there was no URL, just raw bytes), while a real web
  download submitted via a link had it populated with the real URL. Neither
  field is in any published TorBox docs or the official SDK; found by
  inspecting real responses.

`debrid.DownloadStatus.OriginalURL` carries whichever of these applies,
computed by each provider adapter (`torrentToStatus`/`usenetToStatus`/
`webDownloadToStatus`). Two places consume it, covering both timing cases:
`internal/importer.discoverManual` sets a newly-adopted row's `Source`
directly from it at discovery time, and `database.RefreshFromProvider`
(`BackfillSource`) retroactively fills in `Source` for a row already tracked
before the provider happened to report one — gated the same way as the
existing hash/name backfill (only when `Source` is currently empty, so this
never overwrites a value that was already there).

**The remaining limit, narrowed further below**: once a download has
*already* vanished from the provider entirely (the scenario the
vanish-detection feature above catches), there's nothing left to backfill
from — the provider has no record of it at all anymore, `original_url`
included. Source can only ever be backfilled while the download is still
visible in a listing; a **discovered** usenet download that was originally
added via a file upload (so `original_url` was always `null`, even before it
vanished) has no recoverable `Source` either way. This part is unavoidable —
AcerviNode never had the bytes, and neither does the provider once it's gone.

Verified live end to end, not just in tests: a real torrent (Big Buck Bunny,
a Creative Commons short film) was added directly through TorBox — bypassing
AcerviNode entirely — and discovered with `Source` already set to the
reconstructed magnet; deleted directly from TorBox to simulate a genuine
vanish; AcerviNode's own background polling flagged it `error` on its own;
and `POST .../readd` was called for real, successfully resubmitting the
reconstructed magnet and landing a fresh, real `queued` torrent on the
account. A real NZB file (provided directly for this test) confirmed usenet's
`original_url` is `null` for a file-upload-based add, matching the documented
limit above.

#### Re-add for a file-uploaded NZB (not discovered)

The one case that *is* recoverable: a usenet download added **through
AcerviNode's own "+ Add" form** as an uploaded `.nzb` file (not a URL, and not
discovered). `Source` stays empty for it — same as any file upload — but
unlike a torrent (already covered by the hash-reconstructed magnet) or a
discovered NZB (nothing was ever uploaded to AcerviNode), the raw bytes
*were* available at add time, right there in the request. `handleAddUsenet`
now stores them directly on the row (`downloads.source_file`/
`source_file_name`, migration `0008_source_file.sql`), and
`handleReAddDownload` falls back to resubmitting them via `AddNZBFile` when
`Source` is empty but a file is stored.

Stored as a `BLOB` column on the row itself, deliberately, rather than a
separate file on disk: deleting the row (`DeleteDownload`) removes the stored
file atomically with it — no separate cleanup step, and no way for a stray
orphaned file to survive a deleted download the way a disk-based approach
would risk. The blob is deliberately excluded from `downloadColumns`/
`scanDownload` (the normal read path every list/detail fetch uses) — only
`source_file_name` (cheap) is included there, enough to compute `has_source`
without paying for the file bytes on every poll. The actual bytes are only
ever fetched via a dedicated `GetSourceFile`, called exactly once, the moment
`handleReAddDownload` actually needs to resubmit them.

## TorBox (`internal/debrid/torbox`)

The first, and so far only, concrete provider. TorBox exposes both a torrent
service and a usenet service under `https://api.torbox.app/v1/api`, authenticated
with `Authorization: Bearer <key>` (the `requestdl` endpoint also accepts
`token=<key>` as a query parameter, since the resulting URL is meant to be handed
directly to a downloader).

Torrent endpoints used: `POST /torrents/createtorrent` (magnet or multipart file),
`GET /torrents/mylist`, `GET /torrents/checkcached`,
`POST /torrents/controltorrent`, `GET /torrents/requestdl`.

Usenet endpoints follow the same shape under a `/usenet/...` path family (add,
list, request-download-link, control/delete).

### State mapping

TorBox reports a `download_state` string (shared across both services) that
`internal/debrid/torbox/provider.go`'s `mapDownloadState` translates into
AcerviNode's provider-agnostic `debrid.DownloadState`. The vocabulary itself
isn't published as an exhaustive list anywhere in TorBox's own docs, so it was
ported from [decypharr](https://github.com/sirrobot01/decypharr)'s own
production mapping (`pkg/debrid/providers/torbox/torbox.go`'s
`getTorboxStatus`) rather than guessed — the reference implementation this
project benchmarks against, and one that's actually running against real
TorBox accounts. A qualifier TorBox appends to some states (e.g. `"stalled
(no seeds)"`) is stripped before matching, same as decypharr's own regex.

The important part: **anything unmatched is treated as an error**, not "still
downloading" — this includes a stalled/no-seeds torrent. TorBox's own [help
center](https://support.torbox.app/en/articles/9928977-download-statuses)
independently confirms an explicit `"Error"` state exists (server error,
missing encryption key, missing par2 files, etc.), which previously had
nowhere to go but the same bucket as genuinely-still-downloading states —
found while auditing the whole state machine, not from a specific bug report,
and confirmed against the real account's own data (`mapDownloadState` is
tested directly against `"stalled (no seeds)"`, the exact raw string a real
torrent on the test account had at the time).

A local `error` state reached this way is *not* sticky — if the provider
later reports genuine progress again (e.g. a stalled torrent finds a seed),
it recovers automatically on the next refresh. Contrast with
[Completed Download Handling](#completed-download-handling-internalimporter)'s own retry
exhaustion below, which *is* sticky by design.

Both also fall back to `GET /queued/getqueued?type=torrent|usenet` — a
separate pre-processing queue TorBox holds a download in (e.g. an account
concurrency limit, or backend load) *before* it appears in `mylist`/
`usenet/mylist` at all. Found by inspecting a comparable open-source debrid
client's own polling code ([RDT-Client](https://github.com/rogerfar/rdt-client)),
which checks both endpoints where AcerviNode previously only checked the
`mylist` family — see `Provider.List`/`Status` and `UsenetProvider.List`/
`Status`, which merge a queued-only entry in (or fall back to it) so a
backlogged download shows as genuinely `queued` instead of "not found."
`queued/getqueued` carries no progress/state/size — only proof the download
exists and is pending — so this closes a narrow visibility gap, not a speed
gap: once something's actually downloading, both AcerviNode and RDT-Client
read progress from the same `mylist` endpoint the same way.

**Confirmed live (not just from docs):** `mylist`/`usenet/mylist` is cached
server-side for up to 600 seconds by default — a freshly added torrent was simply
absent from the response until `bypass_cache=true` was passed. Both `ListTorrents`
and `ListUsenetDownloads` always set it, since AcerviNode's whole polling model
(both compat shims' `refreshFromProvider`, and `internal/importer`'s own ticks)
depends on this endpoint reflecting current state promptly, not on a 10-minute
delay.

Exact field names and error envelope are cross-checked against the official
[`torbox-sdk-go`](https://github.com/TorBox-App/torbox-sdk-go) rather than
guessed — see `internal/debrid/torbox/types.go` for the structs actually in use.
That said, the SDK's own docs aren't always right either: `createusenetdownload`'s
`usenetdownload_id` is documented there as a string, but a real account's response
sends it as a JSON number (found via a real NZB upload failing with `json: cannot
unmarshal number into Go struct field ...usenetdownload_id of type string`) — same
numeric shape as a torrent's `torrent_id`, and now decoded/formatted the same way
(`CreateUsenetDownload`).

`requestdl` also has an undocumented `zip_link=true` parameter — omit
`file_id` and add it, and TorBox returns a single URL that zips every file
in the torrent/usenet download server-side. Not mentioned in the official
SDK or public docs; found by testing directly against a real account, then
confirmed the returned URL actually serves a real `.zip`
(`Content-Type: application/zip`, correct total size) via `curl -I`. Backs
`RequestZipDownloadLink` and `GET /api/v1/downloads/{id}/zip-link` — see
[API](api.md#direct-file-downloads). The torrent side is directly verified live;
the usenet side (`RequestUsenetZipDownloadLink`) mirrors the same shape but
wasn't independently confirmed — by the time it was written, every usenet
download on the test account had expired from `mylist` (0 items), leaving
nothing live to test `zip_link` against on that side specifically.

### Web Downloads

TorBox's third service, alongside torrents and usenet: debrids direct links from
~160 supported hosters (Mega, 1Fichier, Mediafire, PixelDrain, and more —
`GET /webdl/hosters` returns the current list dynamically, no hardcoded copy kept
here since it would just go stale). Confirmed live against the real account: Mega
itself is active (`status: true`), and a real (if since-expired) Mega folder
download already existed in the account's own history from before this feature was
built, which is what confirmed `mylist`'s actual JSON shape (including a
legitimate file `id: 0`) against real data rather than docs alone.

Genuinely link-only — `POST /webdl/createwebdownload` (confirmed directly against
TorBox's real OpenAPI spec, not the SDK's docs, since those have already been wrong
once for this project) takes `application/x-www-form-urlencoded`, not multipart,
and has no file-upload field at all, unlike `createtorrent`/`createusenetdownload`.
`link` is the only required field. Otherwise the same shape as the other two
services: `POST /webdl/controlwebdownload` (delete), `GET /webdl/requestdl`
(`web_id`/`file_id`/`token`, plus the same undocumented `zip_link=true` trick),
`GET /webdl/mylist` (`bypass_cache=true`, same 600-second caching behavior as the
other two `mylist` endpoints).

**Both `createwebdownload`'s response shape and the zip-link trick are now
confirmed live**, using `archive.org` (itself one of the ~160 supported
hosters) as a safe test target — a small, public-domain audio file
(`archive.org/download/testmp3testfile/mpthreetest.mp3`) let this get
verified end to end without touching anyone's copyrighted content, after two
earlier attempts failed (a GitHub raw-file link came back `UNSUPPORTED_SITE`;
PixelDrain's anonymous upload now requires its own API key):

- `createwebdownload`'s response field `webdownload_id` is documented as a
  string, but — confirmed via a raw API call directly against a real account
  (`{"webdownload_id": 1462379, ...}`) — comes back as a JSON number, the
  same mismatch `usenetdownload_id` turned out to have (see above). `types.go`
  models it as a plain `float64`, the same as every other provider-assigned id.
- `RequestWebDownloadZipDownloadLink` — confirmed live: the resolved URL
  served a real `application/zip` with the correct `content-disposition`.
- The full add → status → files → per-file-link → zip-link → delete cycle
  was verified end to end through AcerviNode's own live API, and the
  provider-side delete was independently confirmed by querying TorBox's own
  `webdl/mylist` directly afterward — the test download was actually gone
  from the account, not just the local row.

### Account status

`GET /user/me` backs `Provider.Account` (`debrid.AccountProvider` — see above).
Confirmed live against the real account: the actual response has far more fields
than either the official SDK's docs or its own Go types declare (e.g.
`total_bytes_downloaded`, `torrents_downloaded`, `web_downloads_downloaded` weren't
documented anywhere found during research) — `UserData` in
`internal/debrid/torbox/types.go` only models the subset AcerviNode's own account
status display actually uses: `plan` (an integer tier — 0 Free, 1 Essential, 2 Pro,
3 Standard, confirmed live against the real account, which is a Pro/`plan: 2`
subscription), `is_subscribed`, `premium_expires_at`, `total_bytes_downloaded`.

## Adding a new provider

1. New subpackage under `internal/debrid/<name>/`.
2. Implement `debrid.TorrentProvider`. Implement `debrid.UsenetProvider`,
   `debrid.WebDownloadProvider`, and/or `debrid.AccountProvider` too, only for
   whichever of those the service actually has a real backend for.
3. Register it in `cmd/acervinode`'s provider construction (`newTorBoxProviders`
   and `liveSettings`, or their equivalents for the new provider) — that's the
   only place a concrete provider type is referenced outside its own package.

No changes are needed in `internal/qbittorrent`, `internal/sabnzbd`, or
`internal/database` to add a provider — that's the point of the interface seam.
