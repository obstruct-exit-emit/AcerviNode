# Providers

AcerviNode talks to debrid services through four small interfaces in
`internal/debrid` — `TorrentProvider`, `UsenetProvider`, `WebDownloadProvider`
and `AccountProvider`. Every add, status check, and link resolution a compat
shim performs goes through one of these — never through a concrete provider
package directly.

They are separate interfaces rather than one big one because a service
supports whichever subset it happens to support, and the type system is the
right place to say so: TorBox implements all four, AllDebrid implements every
one except `UsenetProvider` because it has no usenet service at all. A
provider simply isn't registered for a kind it can't do, so it never gets
routed one — see [Multiple providers](#multiple-providers-debridregistry).

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

## Multiple providers (`debrid.Registry`)

More than one provider can be configured at once. `debrid.Registry` holds them
by name, with a separate map per kind, so "which provider" is a lookup rather
than a field.

**Name is not type.** An entry's name is free text; `providers.<name>.type`
picks the implementation and defaults to the name. That split is the whole
mechanism behind **two accounts on one service**: entries `torbox` and
`torbox-work`, both `type: torbox`, are fully independent providers — separate
credentials, separate listing cache, separate rate-limit backoff. One account
hitting a 429 does not slow the other down, which was verified live rather
than only unit-tested.

**A provider registers only the kinds it can actually do.** TorBox registers
all three; AllDebrid registers torrent and webdl and is simply absent from the
usenet map. This is why the accessors return a typed pointer rather than an
interface — a nil `*DynamicTorrentProvider` stored in an interface is not a nil
interface, and every caller has to decide what to do when a provider isn't
there.

**A kind can also be switched off deliberately.**
`providers.<name>.disabled_kinds` (Settings → Provider, or
`PUT /api/v1/settings/providers/{name}/kinds`) stops a provider registering
for a kind it does support. The mechanism is the same one AllDebrid's missing
usenet already uses — no wrapper, so no routing, no polling, no appearance in
the add endpoints — which is why turning a kind off needs no new cases
anywhere. Everything supported is on unless you say otherwise. Worth having
because capability isn't always something you want: two accounts on one
service can split kinds between them to keep their rate limits apart, and a
kind you never use is polling you never needed.

Switching a kind off interacts with `default_provider` exactly as a missing
service does, which was checked live rather than assumed: with AllDebrid as
the default, a usenet add falls through to TorBox while a torrent add stays
on AllDebrid. A default whose kinds are *all* switched off still routes
everything elsewhere, and a kind no configured provider handles fails the add
with a clear `503` rather than resolving to a provider that can't take it.

**Web downloads also route on the file host, after the fact.** Kind-level
routing can't see which hoster a link points at, so a provider that does web
downloads may still refuse the specific link. When routing chose the provider
itself, the add falls through to another configured web-download provider
rather than failing — `debrid.ErrHostNotSupported` is the sentinel each
provider maps its own refusal onto (AllDebrid's `LINK_HOST_NOT_SUPPORTED`
code; TorBox has no code here, only prose, so that match is deliberately
narrow and degrades to a plain provider error if the wording changes). Only
that sentinel is retried — any other failure might mean the add partly
landed, and re-sending it would risk a duplicate — and an explicitly named
provider is never swapped out.

**Routing resolves per kind, not globally.** `default_provider` names one
entry, but `DefaultNameFor(kind)` falls through to the first registered entry
that supports that kind when the default doesn't. Without the fallback, making
AllDebrid the default would break usenet outright: every usenet add would
resolve to a provider with no usenet service and fail, even with TorBox
configured right beside it. That was found live, doing exactly that.

Whatever the routing resolves to is **recorded on the download row** at add
time, so every later status poll, link request and delete goes back to the
same account. A provider removed later leaves those rows intact but no longer
resolving — see `DELETE /api/v1/settings/providers/{name}` in [API](api.md).

**Rate-limit backoff is per `(provider, kind)` pair**, not global and not
per-provider. A provider's usenet listing being throttled must not stop its
torrent polling, and one account's limit must not stop another's. Backoff is
exponential from a 30s base, capped at 5 minutes.

**One listing per provider, shared by every consumer.** `debrid.ListCache`
means several \*arr apps polling at once cost one provider call, not one each.
Callers that must not act on stale data — the importer's own tick — use
`Refresh()`/`ListFresh()`, which always fetch but still populate the cache for
everyone else. That distinction is load-bearing: reading its own shared cache
let the importer act on data a full poll interval old.

## Live settings

Provider credentials can be set or changed two ways: hand-editing
`providers.<name>.api_key` in `config.yaml` (restart required, the original
mechanism), or `PUT /api/v1/settings/providers/<name>` (the web UI's Settings
tab uses this) — no restart needed. Both end up in the same place. `<name>` is
the entry name, so an instance holding two TorBox accounts addresses them
separately.

This works because `cmd/acervinode` never hands a concrete provider directly to
the compat shims, the importer, or the native API — it hands each of them the
same `*debrid.DynamicTorrentProvider`/`*debrid.DynamicUsenetProvider` instance
(`internal/debrid/dynamic.go`), which implements the real provider interfaces by
delegating to whatever's currently `Set()`. Both compat shims are **always**
mounted now (not conditionally on a provider existing at startup) — before a key
is set, every provider-backed call just returns `debrid.ErrNoProvider` instead of
the route not existing, which is what makes configuring a provider for the first time
through the settings API (not just at startup) possible at all. `cmd/acervinode`'s
`liveSettings` type is what `PUT` actually calls: it swaps the Dynamic wrappers'
inner provider and calls `config.Save` to persist the change, all under one mutex
so concurrent settings changes don't race.

That generalization has since happened. The settings API is no longer
TorBox-shaped: `liveSettings.SetProviderAPIKey(ctx, name, key)` takes the
provider entry by name, and `POST`/`DELETE /api/v1/settings/providers` add and
remove entries at runtime. Nothing needs a restart, and nothing in
`internal/api` names a concrete provider any more.

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

### Directory permissions

Every directory created under `save_path`/`download_dir` (`ensureWritableDir`)
is `0777` — world-writable, not just owner-writable — and this is
deliberately re-applied every time, even to a directory that already
existed, rather than only at creation. Found live from a real Radarr
import failure: AcerviNode's own process runs as a dedicated, non-root
systemd user (see [Installation](installation.md#linux-deployment)), which
isn't necessarily the same user/group an \*arr app itself runs as — very
commonly a separate Docker container with its own PUID/PGID. The previous
`0755` mode meant only AcerviNode's own user could write into (and
therefore move a file out of) these directories at all.

This broke NZB-sourced imports specifically, not torrents: real SABnzbd's
own completed-item reporting always tells Sonarr/Radarr it's safe to move
a file — confirmed against their real source, `Sabnzbd.cs`'s `GetHistory`
sets `CanMoveFiles = true` unconditionally for every item — so Radarr
always attempted a genuine move/hardlink, which needs write access on
whichever directory directly contains the file, not just read access to
the file itself; that's exactly what `0755` didn't grant a differently-
privileged Radarr, producing "Access to the path ... is denied." A torrent
import silently avoided the identical wall instead of hitting it: Radarr's
qBittorrent client only sets `CanMoveFiles = true` when the torrent is
reported as paused after reaching its own configured seed limit — a state
AcerviNode never reports (it has no real local seeding at all; TorBox
handles that server-side) — so every qBittorrent-sourced import silently
fell back to copy-only, which needs only read access, at the cost of
duplicating the file on disk instead of erroring.

**World-writable, not group-writable — a deliberate correction made after
shipping `0775` first.** Group-writable seemed like the more conservative
choice, but it still requires the user to coordinate matching group
membership between AcerviNode and every *arr app that needs to write here
— real friction on a fresh install, and one a live report (a real
Proxmox/NAS deployment where AcerviNode itself wasn't even running as its
packaged dedicated user) showed doesn't actually resolve cleanly in every
real environment (LXC UID-namespace remapping, ad hoc non-systemd
deployments, etc.). The standard self-hosted-media-stack answer — give
every container the same PUID/PGID, confirmed live against a real
reference client's own Docker packaging (rdt-client's
`ghcr.io/linuxserver/baseimage-alpine` base and its own
`README-DOCKER.md`) — isn't something AcerviNode can ask of apps it
doesn't package itself. `0777` is the zero-configuration equivalent, and a
narrow one: it only loosens these specific per-download directories,
nothing else AcerviNode manages. See
[Installation](installation.md#letting-sonarrradarr-move-files-out-of-download_dir)
for the alternative (matching AcerviNode's own systemd `User=`/`Group=` to
your stack) if `0777` doesn't fit your threat model — or just change
`download_dir_mode` (`internal/importer.SetDirMode`, live, no restart, see
[Configuration](configuration.md)) to whatever mode you'd rather use;
`0777` is only the default, not hardcoded.

**The torrent copy-only inefficiency mentioned above is now also fixed** —
see [qBittorrent API compatibility](qbittorrent-api.md#state-mapping)'s
`pausedUP` state and `ratio`/`ratio_limit` fields: both of Sonarr/Radarr's
`CanMoveFiles`/`CanBeRemoved` conditions are now satisfied for a completed
torrent, the same as SABnzbd's already were, so an *arr app can hardlink
(not copy) a completed torrent's files, and — with its own "Remove
completed downloads" setting enabled — clean up the source automatically
afterward via this shim's existing delete endpoint.

### Live fetch progress

A Managed download's progress previously froze the instant the provider
itself finished (`d.Progress` jumps to `1.0` the moment a row reaches
`provider_completed`) — including for however long this section's own
local file transfer to disk then took, which for a large file is real,
observed time with nothing showing it was still happening at all. Both
compat shims already reported `provider_completed` as still
"downloading"/"Moving" for exactly this reason (files aren't actually on
disk yet), but the *progress value* itself didn't reflect that — it just
sat at whatever the provider last reported.

`fetchFile` now wraps its file write in a throttled `progressWriter` that
calls `database.DB.SetFetchProgress` as bytes land on disk (throttled to
roughly twice a second — frequent enough to feel live against the ~4s poll
interval both the native API and web UI already use, infrequent enough not
to contend the tracking map's mutex on every single `io.Copy` buffer
flush), aggregated across every file in a multi-file release by
`processDownload`'s own running total. `database.EffectiveProgress`
substitutes this live value in for `Progress` everywhere a download is
reported — the native API, the web UI (same field, no frontend changes
needed), and both compat shims' own progress fields, so Sonarr/Radarr's own
Activity view reflects real fetch progress too — while every other state
keeps reporting the persisted `Progress` unchanged. Cleared unconditionally
once a fetch attempt ends, success or failure, so a stale percentage never
lingers into a retry attempt or past `ready_for_import`.

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

### Fast per-download poll

`refreshStatuses` alone means a download's local state only ever advances on
an `import_interval_seconds` boundary (10s by default) — a download that
finishes moments after a tick simply waits for the next one. A controlled,
same-account, same-content comparison against
[rdt-client](https://github.com/rogerfar/rdt-client) — a reference debrid
download client — via its actual Managed-equivalent auto-fetch path (not its
on-demand one, which isn't a fair comparison) found AcerviNode taking roughly
2x longer to notice an already-cached file was ready and get it onto local
disk. Timing instrumentation pinned the entire gap to this exact mechanism —
the file-fetch step itself measured in microseconds once triggered.

The obvious fix — shorten `import_interval_seconds` — was tried directly
against a real account and rejected: even a 2-second interval, still doing a
full `List()` for all three kinds every tick, immediately tripped TorBox's
real rate limit. Reading rdt-client's own source confirmed why this can't be
the fix: its real torrent-status loop runs every **1 second** flat
(`TaskRunner.cs`), not the 10-second "Check Interval" its own UI shows (that
setting gates a different, unrelated background service) — meaning it
already polls 10x more often than AcerviNode's default, the same direction a
naive fix would have pushed, just further into rate-limit territory.

The actual fix borrows a different strategy, found by reading
[decypharr](https://github.com/sirrobot01/decypharr)'s own TorBox client
(`pkg/debrid/providers/torbox/torbox.go`): `GetTorrent`/`UpdateTorrent` call
`mylist` filtered to one specific `id`, not the full account listing.
Confirmed against TorBox's own official SDK docs (not guessed): passing `id`
to `mylist`/`usenet/mylist`/`webdl/mylist` "will return an object rather than
list" — a single-item lookup TorBox can presumably serve far more cheaply
than listing everything, the same principle decypharr's own polling design
leans on (its bulk `TorrentsRefreshInterval` defaults to a lazy 10 minutes;
per-active-item status checks via this id-filtered call are what actually
keep it responsive).

`Client.GetTorrent`/`GetUsenetDownload`/`GetWebDownload` implement this;
`Provider.Status`/`UsenetProvider.Status`/`WebDownloadProvider.Status` (already
part of `debrid.TorrentProvider`/`UsenetProvider`, previously implemented as a
full `List()` plus a linear scan) now use them directly — a free speedup for
every existing call site that resolves one specific download's status right
after an add (`internal/api`, `internal/qbittorrent`, `internal/sabnzbd`), not
just the new poll below.

**`Files` needed the same change, found live, not just for consistency:**
the first real end-to-end test of the fast poll hit a genuine race —
`refreshActiveDownloads` noticed a brand-new torrent as `StateCompleted`
within about a second of adding it (the id-filtered lookup genuinely does
reflect current state that fast), but `processDownload`'s very next step,
`Files()`, was still built on `ListTorrents()` + a linear scan — TorBox's
*bulk* listing hadn't indexed the same brand-new torrent into itself yet,
so it failed outright with "not found." Not a bug in the fast poll itself;
the bulk listing and the id-filtered lookup are just genuinely different
views with different latency, and `Files()` was checking the slower one.
The retry/backoff path caught it (attempt 2 succeeded once the bulk listing
caught up), but needlessly — `Provider.Files`/`UsenetProvider.Files`/
`WebDownloadProvider.Files` now use the same targeted `Get*` lookup `Status`
does, closing the gap at the source instead of leaning on retry to paper
over it.

`Importer.runFastPoll` runs `refreshActiveDownloads` on its own goroutine and
its own ticker (`fastPollInterval`, 3s by default — `fast_poll_interval_seconds`,
live-configurable, no restart, see [Configuration](configuration.md); the
default was tuned live against a real provider to stay responsive without
risking a rate limit, but a user with many downloads active at once may
want to widen it themselves), independent of `Run`'s own bulk-tick loop so
a slow file fetch mid-`Tick` never delays noticing a *different* download's
completion.
Each tick, for every kind, it fetches only the Managed (`arr`) downloads
currently `queued`/`downloading` (`database.ListActiveManagedDownloads` —
manual downloads and anything already past those states are never fast-polled,
since nothing's waiting on them any faster) and checks each one individually
via `Status`, applying the result through the exact same
`database.RefreshFromProvider` the bulk path uses — fed one row and one status
instead of a whole account's worth, so there's no second state-transition
implementation to keep in sync. A rate limit hit here records against the
same per-kind cooldown state `refreshKind`'s bulk `List()` path uses (see
below) — both paths share one provider-side budget, so either one backing off
backs off the other too, rather than each tracking (and potentially fighting
over) an independent one.

### Refresh ordering guard

Once both the bulk path and the fast per-download poll above exist, a real
race becomes possible: multiple independent pollers (either compat shim's
own reactive refresh on every `/info`/`mode=queue` request, `Tick`'s bulk
pass, and `runFastPoll`'s targeted one) can all be mid-flight against the
provider for the *same* download at once. `database.DB`'s connection pool is
a single connection (`SetMaxOpenConns(1)`), so the resulting `UPDATE`s can't
corrupt each other — but serialization only guarantees they don't collide,
not that they land in the order their underlying provider data was actually
fetched in. A slower request that started earlier can finish (and write)
after a faster one that started later, silently regressing progress/state
back to stale data with nothing to ever correct it, since whichever poller
already landed the fresher result has no reason to run again immediately.

Found live, not hypothetically: watching a real, genuinely uncached torrent
download (added specifically to exercise this, since TorBox's normal
instant-cache path never shows real transfer progress at all), `GET
/api/v2/torrents/info` stayed frozen reporting 13.9% long after the same
download's own database row — and TorBox's own API, queried directly —
had already reached 50%+.

`database.RefreshFromProvider` now takes a `fetchedAt time.Time` — captured
by every call site the moment *before* it calls the provider (`List`/
`Status`), not when the call returns — and gates each row's actual write on
it via `refreshGuardAllows`: an in-memory, per-download-ID map (guarded by
its own mutex, deliberately not the database itself — this is a
same-process ordering decision, not something that needs to survive a
restart) recording the `fetchedAt` of the most recently *applied* update.
A write whose `fetchedAt` is older than what's already recorded is silently
skipped — the row keeps its fresher value instead of regressing.

### WAL mode, and why every write's speed matters here specifically

`database.DB`'s single connection (`SetMaxOpenConns(1)`, see above) means
*every* operation against it — a read from the web UI's own list poll
included, not just the writes discussed above — serializes through that one
connection, one at a time, regardless of what else is going on. That makes
how long any single write holds it for directly how long everything else
queued behind it has to wait — investigated directly off a report of the
app hanging/stuttering.

SQLite's own defaults are a rollback journal and `synchronous=FULL`, which
means a full fsync of the whole database file on every single commit.
`database.Open` now sets `journal_mode=WAL` and `synchronous=NORMAL`
instead — a WAL commit is an append, not a whole-file fsync, so each
individual write is meaningfully faster, which is what actually shortens
everyone else's wait behind the one connection. This doesn't touch the
single-connection design at all — WAL's usual headline benefit (concurrent
readers not blocked by a writer) doesn't even apply here, since there's only
ever one connection to begin with regardless of journal mode; the win is
purely lower per-write latency. Measured live: 200 individual-transaction
writes against a real copy of this project's own database, single `sqlite3`
process to isolate the fsync cost from per-invocation process-spawn
overhead — roughly 0.2ms/write before, roughly 0.06ms/write after. A silent
no-op (not an error) on an in-memory (`:memory:`) database, e.g. in tests —
SQLite keeps "memory" journaling for those regardless, per its own docs.

### Polling loops wait for the previous request, not a fixed clock tick

A second, compounding contributor to the same hanging/stuttering
investigation: both the web UI's own list poll (`App.tsx`) and the download
detail view's poll (`DownloadDetail.tsx`) used `setInterval`, which fires on
a fixed cadence regardless of whether the previous call actually finished.
`GET /api/v1/downloads/{id}` (what the detail view polls) blocks
server-side on a live provider call (`handleGetDownload`'s file list) that
can take up to `provider_request_timeout_seconds` (30s default) when the
provider itself is slow — the same condition a prior fix already added a
"Loading…" placeholder for, so the wait itself is visible rather than
looking hung (see CHANGELOG). With `setInterval`,
a single 30-second-slow poll didn't just run late — several more fired
behind it every 4 seconds regardless, each its own live provider call,
piling up concurrent in-flight requests against both AcerviNode and TorBox
at exactly the moment either was least able to spare it. Both now use a
self-rescheduling `setTimeout` instead — the next poll is only ever
scheduled after the previous one genuinely finishes, so a slow poll is at
worst late, never compounding.

### LiveStatus: the same cache, reused for the native API/UI

The ordering guard above needs a per-download entry regardless — extending
it to also hold the fast-moving fields themselves (ETA, torrent swarm info,
usenet phase) was a small addition with a real payoff: `database.DB.LiveStatus(id)`
lets `internal/api` show the exact same live data both compat shims already
did, without adding a *third* kind of synchronous provider call per request.
Right after fixing the race above — caused by too many independent pollers
hitting the provider for the same download — adding another one purely for
the native API's own benefit would have undone the lesson just learned.

Both concerns share one map (`refreshCacheEntry{fetchedAt, live LiveStatus}`)
precisely because they're the same operation: `refreshGuardAllows` writes the
live snapshot at the same moment, and under the same ordering check, as the
persisted write — a stale response is exactly as wrong to cache as it would
be to write to the database, so it's rejected the same way. `LiveStatus`
itself just reads the cache; `ok` is `false` whenever nothing's polled that
download yet (e.g. it was only just added).

`internal/api/downloads.go`'s `toDownloadResponse` takes a `database.LiveStatus`
as a plain parameter (not something it looks up itself) — keeps it a pure,
testable mapping, with `handleListDownloads`/`handleGetDownload`/etc. reading
the cache once per row and passing the result in. See [API](api.md#download-json-shape)
for the resulting `eta_seconds`/`seeders`/`leechers`/`download_speed_bytes`/
`phase` fields.

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
the two a given download is — set once at insert time, never changed
afterward:

- **`arr`**: added through the qBittorrent or SABnzbd compat shim — i.e. by
  an *arr app — or, added directly via the native API's own add endpoints
  with `added_via=arr` explicitly requested (admin-only — see
  [Adding downloads directly](api.md#adding-downloads-directly) — a member
  gets `403`, not a silent downgrade to Manual). Both land identically:
  auto-fetched by Completed Download Handling to `download_dir` or a
  `category` override, same as always. Shown in the web UI's **Managed**
  tab; the "+ Add" button offers this as an explicit Managed/Manual choice
  for an admin (never shown to a member at all, matching their own lack of
  Managed access — see [Auth: login accounts and roles](#auth-login-accounts-and-roles)).
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
what happens to everything currently unmatched depends on whether the
instance itself is genuinely fresh (`database.HasAnyDownloads` — has this
database ever tracked a single download, of any kind, before this tick
started). On an **established** instance — this feature (or a newly added
second provider) showing up on something that's already been running a
while — nothing is adopted: every currently-unmatched item is instead
recorded into `discovery_baseline` (with `discovery_seeded` as the
per-provider-per-kind marker that seeding has already happened) and
permanently ignored, so it doesn't flood the Manual tab with a big
pre-existing history. On a **genuinely fresh** install, though, there's no
existing history to protect — the account's current contents are adopted
immediately instead, and the baseline is seeded empty. Either way, only
items present at seed time are affected by that one-time branch; everything
that shows up *afterward* — added to TorBox at any time from then on,
whether through AcerviNode or directly — is always adopted normally.

Found live: a fresh Proxmox install recognized the configured TorBox
account but never showed its existing downloads, because this always took
the established-instance branch regardless of whether the instance was
actually fresh. `freshInstall` is computed exactly once per
`refreshStatuses` tick, before any kind's own discovery runs — checking it
fresh inside each kind's own pass instead would make the answer depend on
iteration order (torrent adopting its items first would make the database
non-empty by the time usenet's own check ran, wrongly baselining usenet's
equally pre-existing items).

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
`handleDeleteDownload` (`internal/api`) tombstones every real delete
(`database.RecordDeletedDownload`) before removing the local row;
`discoverManual` skips adopting anything tombstoned within
`recentlyDeletedGracePeriod` (5 minutes, generous on purpose — a
`provider_download_id` that's genuinely gone never legitimately reappears,
since a fresh add always gets a new one, so this only ever blocks
re-adopting the exact same now-defunct id). Tombstones past their own expiry
are pruned opportunistically on every new one recorded, rather than needing
a separate cleanup job.

**A tombstone's lifetime depends on whether the provider-side delete
actually succeeded**, because that five-minute reasoning quietly assumes it
did. The provider call is best-effort — a provider outage or rate limit must
never leave a row the user can't remove — so when it fails, the item is
genuinely still on the account, and a short window doesn't prevent the ghost
it exists to prevent; it just delays it until the window lapses. A failed
delete therefore records a much longer tombstone
(`unconfirmedDeleteGracePeriod`, 30 days — deliberately longer than TorBox's
own retention, so an orphan left behind this way ages off the account before
the tombstone does). Found during a live burn-in, not by inspection: two
downloads were deleted while the account happened to be rate-limited, both
provider deletes returned `429`, and both reappeared as ghost Manual
downloads once the five minutes were up — including one that had been
Managed, which came back in the wrong tab.

**This same race is exactly why a Managed download could turn into a Manual
one, reported directly and confirmed live.** `internal/qbittorrent`'s and
`internal/sabnzbd`'s own delete handlers (`POST /api/v2/torrents/delete`,
`name=delete`) never called `RecordDeletedDownload` at all — only
`handleDeleteDownload` in `internal/api` did. Both shim handlers already call
`provider.Delete` unconditionally (`deleteFiles` doesn't gate *whether* that
call happens, only what gets passed through to it — TorBox's own `Delete`
implementation ignores the flag entirely regardless; see
[Local file deletion](#local-file-deletion) below), so the provider-side
torrent/download genuinely was being deleted on every request. The bug was
purely the missing tombstone: without one, *any* delete through either shim
— triggered by a user, or by an *arr app's own routine "remove completed
download" cleanup step after import — was vulnerable to the exact listing-lag
window described above. The very next `discoverManual` tick landing in that
window would see the item still sitting in the provider's listing (not yet
caught up with its own successful delete) with no local row protecting it,
and adopt it fresh as a brand-new Manual download. To a user watching the UI,
a download that started life Managed appeared to have silently become
Manual. Fixed by giving both shims' delete handlers the exact same tombstone
call `handleDeleteDownload` already had — all three delete surfaces now
behave identically here.

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

### Local file deletion

**`deleteFiles=true`/`del_files=1` never actually deleted local files, on any
of the three delete surfaces, until this was found and fixed.** Every delete
handler (`internal/api`'s `handleDeleteDownload`, `internal/qbittorrent`'s
`POST /api/v2/torrents/delete`, `internal/sabnzbd`'s `name=delete`) passed
that flag straight to `provider.Delete(ctx, id, deleteFiles)` — but TorBox's
own implementation ignores it entirely (the parameter is literally named `_`)
and only ever removes the provider-side copy. The only code path that ever
called `os.RemoveAll` on a download's local files was the unrelated
[Retention/cleanup policy](#retentioncleanup-policy) below — a completely
separate, automatic flow a user-initiated or *arr-initiated delete never
went through. Reported directly and confirmed by tracing all three handlers.

`Importer.RemoveLocalFiles(d *database.Download) error` closes the gap:
the same `resolveDestDir`-then-`os.RemoveAll` logic `cleanupDownload` already
used, exposed as its own method so every delete surface can call it without
duplicating `internal/importer`'s config-dependent path resolution
(download_dir, category overrides, an explicit `save_path`) — none of
`internal/api`/`internal/qbittorrent`/`internal/sabnzbd` know how to compute
a download's destination directory on their own. Wired through the `Settings`
interface (`DeleteLocalFiles`), the same indirection every other live-config
value already goes through — `cmd/acervinode`'s `liveSettings` is the only
thing holding a reference to the actual `*importer.Importer`. Same guard as
`cleanupDownload`: refuses to touch anything for a row with no `Name`, since
`resolveDestDir` would otherwise collapse to the bare category directory
shared with every other download in it. Best-effort everywhere it's called —
a failure here logs a warning but never blocks the row itself from being
deleted, matching how the provider-side delete call is already handled.

### Canceling an in-flight fetch on delete

`Importer.CancelFetch(id string)` interrupts whatever fetch `processDownload`
is doing for `id` right now, if anything, and blocks until it has genuinely
stopped — not just been asked to — before returning. `handleDeleteDownload`
calls it unconditionally, as the very first thing it does, before touching the
provider, local files, or the database row. Without this, deleting a download
that `internal/importer` was still mid-write for had no way to interrupt that
goroutine: it kept writing (potentially recreating whatever
[local file deletion](#local-file-deletion) above had just removed) and only
noticed the row was gone once its own final status update failed against an
already-deleted row — well after the fact. Live-verified: deleting a
multi-gigabyte Managed torrent partway through its fetch, with
`deleteFiles=true`, now leaves nothing behind on disk and the row disappears
from the API immediately, instead of racing an in-flight write.

Tracked via `Importer.activeFetches`, a map of download id to a
`context.CancelFunc` + a `done` channel, registered by `processDownload` itself
right before it starts fetching and cleared via `defer` when it returns. This
doubles as a guard against a second hazard: a fetch that outlives one
`import_interval_seconds` tick (a large multi-file torrent, same shape as the
one used for live verification above) would otherwise still be sat in
`provider_completed` with no `next_retry_at` set when the *next* tick's own
`ListDownloadsDueForRetry` runs, and get handed to a second, fully concurrent
`processDownload` goroutine writing into the same destination directory —
`processDownload` now checks-and-registers atomically at the top and returns
immediately (not an error) if `id` is already registered.

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

### Per-file fetch filtering

Requested directly, comparing AcerviNode's settings against
[rdt-client](https://github.com/rogerfar/rdt-client)'s own — it can skip
files under a minimum size (samples, `.nfo`/`.txt` junk) and include/exclude
by regex; AcerviNode fetched every file the provider reported, no filtering
at all, before this. `min_fetch_file_size_bytes`/`max_fetch_file_size_bytes`/
`include_file_regex`/`exclude_file_regex` (`config.Config`, all disabled —
`0`/`0`/empty — by default) are checked by `Importer.filterFiles`, called
from `processDownload` right after the provider's own file list comes back
and before any of them are fetched. A file is kept only if it's at least
the minimum size **and** at most the maximum size (each only when actually
set) **and** (no include pattern, or its path matches it) **and** (no
exclude pattern, or its path doesn't match it) — every check that's
configured must pass; unlike rdt-client's own "only use one or the other"
convention for include/exclude, a file here has to satisfy all of them.
`max_fetch_file_size_bytes` itself came from a second comparison, against
[decypharr](https://github.com/sirrobot01/decypharr)'s own `MinFileSize`/
`MaxFileSize` — the symmetric counterpart to the minimum, e.g. skipping an
oversized bonus feature bundled alongside the main file. `config.Config.Validate`
rejects a `min_fetch_file_size_bytes` greater than a nonzero
`max_fetch_file_size_bytes` — a range that could never match anything.
Matched against each file's path (e.g. `Show/episode.en.srt` for a
multi-file torrent's own subdirectory structure) for the regex checks, not
its size or any other field. Purely local: never changes what the provider
itself considers part of the download, or what
`GET /api/v1/downloads/{id}`'s own `files` list reports — only which of
those files actually get written to disk. A filter matching nothing
doesn't leave the download stuck — it trivially reaches `ready_for_import`
with zero files fetched, the same as a torrent that genuinely had none.

### Stuck-download watchdog

Also requested directly off the same rdt-client comparison — its own
"maximum lifetime" setting auto-errors a torrent that's been running too
long, regardless of whether it's still genuinely making progress.
AcerviNode's version (`stuck_download_timeout_minutes`, `config.Config`, `0`
disabled by default) is deliberately keyed differently:
`Importer.checkStuckDownloads`, run right after `refreshStatuses` on every
`Tick`, marks a `StateQueued`/`StateDownloading` row `StateError` once its
own `updated_at` is older than the configured timeout —
`database.ListStuckDownloads`. `updated_at` only actually moves when
`UpdateDownloadStatus`/`RefreshFromProvider` change something real (state,
progress, size, or the error message — see `RefreshFromProvider`'s own
no-op check), never on a poll that found nothing new, so a stale
`updated_at` here means the provider has genuinely stopped reporting
anything new, not just that the download has been running a while. A large
download still steadily, actively transferring on a slow connection is
never affected by this however long the whole thing takes — only a
download that's actually gone quiet trips it, the same "idle, not total"
philosophy `ImportFetchTimeoutSeconds` already applies to a single file's
own fetch. Applies to both Managed and Manual downloads — being stuck
queued/downloading isn't a state that means anything different depending on
how it was added, unlike the retention policy above.

### Error-state cleanup

The retention policy above only ever removes a *successfully finished*
Managed download; an `error` row — retry-exhausted, a genuinely
provider-reported failure, or a vanished Manual download the missing-count
threshold above flagged — had no automatic cleanup path at all before this,
matching a gap in AcerviNode's own settings found the same way as the two
above: rdt-client has a separate "delete after N minutes in error" setting
distinct from its normal finished-download cleanup. `cleanup_error_after_days`
(`config.Config`, `0` disabled by default) enables
`Importer.cleanupErroredDownloads`, run alongside `cleanupOldDownloads` at
the end of every `Tick` — `database.ListErroredDownloadsEligibleForCleanup`
finds every `StateError` row whose `updated_at` is older than the cutoff,
and each one is removed through the exact same `cleanupDownload` the
retention policy already uses (local files if any, best-effort provider
delete, tombstone, row removal). **Applies to both Managed and Manual
downloads**, unlike the retention policy's Managed-only scope — an error
already means AcerviNode gave up or the provider itself lost track of it,
not an in-progress state like `provider_completed`/Manual that needs
preserving until a user actually grabs it.

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

#### A refresh pass judges only its own provider's rows

Missing-detection asks "was this row absent from the listing?", which is only
a meaningful question about rows the polled provider actually holds. A row
belonging to a *different* provider is absent by construction, every single
time.

The importer used to list rows by kind alone, so polling AllDebrid handed it
every TorBox torrent row as well — and each was duly flagged
`no longer found in the provider's account` within three ticks. Live
downloads, on an account that was never asked about them. Both compat shims
already grouped by provider before refreshing; the importer's bulk pass was
the one caller that didn't.

The mass-vanish guard hid this whenever the wrongly-missing fraction happened
to exceed its 50% threshold, which is why it survived the multi-provider work:
it only bites on an instance sitting *below* that line — say five AllDebrid
rows and two TorBox rows, where 28% sails under the guard and the two TorBox
rows get flagged.

Fixed in two places on purpose:

- `ListDownloadsByProvider` scopes what the importer reads, so the right rows
  are passed in the first place.
- `RefreshFromProvider` skips any row whose provider doesn't match
  `RefreshOptions.Provider` regardless, as a backstop. The failure mode here
  is flagging live downloads as gone, so forgetting to scope a future caller
  should be harmless rather than destructive. It also removes any chance of
  two providers' id spaces colliding into a false match — identity is the
  `(provider, provider_download_id)` pair, exactly as
  `GetDownloadByProviderID` already treats it.

The same scoping applies to the mass-vanish fraction itself, which would
otherwise count another provider's rows as missing and trip the guard on a
provider answering perfectly.

#### Mass-vanish circuit breaker

The debounce above only protects against
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

**The breaker is time-bounded, because otherwise it can never conclude.**
Distrusting the listing is the right first response, but it cannot be the
permanent one: an account the user genuinely emptied produces byte-for-byte
the same listing this guard was built to disbelieve. Deleting everything from
the provider's own site, or from a second AcerviNode pointed at the same
account, is a completely ordinary thing to do — and the guard's response was
to freeze every tracked row forever. Never progressing, never flagged
missing, never cleaned up.

Found live rather than by inspection, on a real instance whose TorBox account
had been emptied from elsewhere: three rows stuck indefinitely, two of them
showing 0% for over eight hours, while this warning fired **6,409 times in
ten hours — 73% of every line in the log**, drowning the signal it existed to
raise. Note the shape of that failure is the same one the guard's own code
comment already records fixing once before, when rows already in `error`
counted toward the fraction and jammed it permanently. Same freeze, different
route in.

So `massVanishDecision` tracks how long each `(provider, kind)` scope has
been failing the guard, and after `massVanishMaxDuration` (30 minutes) it
hands the listing back to normal missing-detection. At the default 10-second
poll that is ~180 consecutive successful listings all agreeing the account is
empty. Releasing is not itself the destructive step either —
`missingDetectionThreshold` still requires three further consecutive misses
per row afterwards, so a listing that recovers in the meantime costs nothing.

Three details the implementation depends on:

- **Per scope, not global.** One provider's empty listing says nothing about
  another's, and a shared clock would let a healthy provider keep resetting a
  jammed one's — or a jammed one drag a healthy one down.
- **A healthy listing clears the history outright**, rather than merely
  pausing it. The next anomaly is a *new* anomaly and gets its own full grace
  period, instead of inheriting a stale clock from one that already resolved.
- **The warning is throttled** to one per scope per `massVanishLogInterval`
  (5 minutes), with the hand-back announced exactly once on the transition.
  A warning that repeats every tick forever is not a louder signal, it is a
  quieter one.

The state is in-memory only and resets on restart, deliberately: the question
is "has this listing looked wrong for long enough to stop being a suspected
glitch", and a freshly-started process has no grounds to claim it has been
watching. It is surfaced per provider/kind as `listing_anomalous_since` on
[`GET /api/v1/status`](api.md), which is the one state where everything else
looks healthy — lists succeeding, no rate limit — while nothing is actually
reconciling.

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

### Status monitoring (`GET /api/v1/status`)

Requested by the roadmap's own "Path to daily-driver parity" punch list
(ROADMAP.md), as the cheapest of two discussed shapes for closing a real gap:
the only way to know the importer's tick loop was stuck, before this, was
manually tailing the log. Built directly on top of state the importer
already tracked or nearly tracked for other reasons — no new subsystem, no
new config, nothing an operator has to opt into.

Three signals, each answering a different question:

- **`last_tick_at`** (global): when `Importer.Tick` last *ran*, recorded at
  the very top of `Tick` regardless of what happens once inside. Proves the
  background loop itself is alive — if this stops advancing by more than a
  tick or two's worth of `import_interval_seconds`, the process has hung or
  the goroutine has died, a different failure mode than any one provider
  kind having trouble.
- **`kinds.<kind>.last_successful_list_at`**: when that kind's provider last
  answered a bulk `List()` call *without erroring*. Deliberately doesn't mean
  "found something new" — a `List()` call that succeeds but returns nothing
  changed still counts as successful. This is the one genuinely subtle part:
  during the real TorBox `cooldown_until` incident (see
  [`cooldown_until`](#cooldown_until--a-real-undocumented-account-restriction)
  above), every listing call kept returning `200 OK` with zero items — not an
  error — so `last_successful_list_at` would have kept advancing the entire
  time, offering no signal that anything was wrong. That's why
  `cooldown_until` is surfaced as its own separate field on the account
  status endpoint rather than folded into this one: they answer genuinely
  different questions ("is polling itself working" vs. "is the provider
  account restricted"), and conflating them would have hidden exactly the
  incident that prompted both features.
- **`kinds.<kind>.rate_limited_until`** / **`kinds.<kind>.error_count`**:
  direct reads of state the importer already had for its own purposes —
  `Importer.RateLimitCooldownUntil` (see
  [rate-limit backoff](#provider-rate-limit-backoff) above, previously
  exported only for a test to assert against, now also read for real here)
  and a new `database.CountDownloadsByState`/`Importer.ErrorCounts` pass-
  through, respectively.

Deliberately unauthenticated-adjacent but not fully open: routed through
`requireAuth` (the same tier as `/api/v1/version`/`/api/v1/providers`), not
`requireAdmin` and not the fully-open `/api/v1/health` — it reveals
operational detail (timestamps, error counts) beyond a bare liveness check,
but an external monitor is expected to already carry the same API key used
for everything else, not to be a fully anonymous prober.

Verified with unit tests at every layer (`database.CountDownloadsByState`'s
grouping/omission behavior, `Importer.LastTickAt`/`LastSuccessfulListAt`/
`ErrorCounts`, the handler's success/error/auth-required paths) and live
against the real WSL dev instance: `curl` against `/api/v1/status` returned
`last_tick_at` within one interval of the request, `kinds.torrent`/
`kinds.usenet` showing recent `last_successful_list_at` values, and
`error_count` matching what the Managed downloads table showed for that kind
at the time.

## TorBox (`internal/debrid/torbox`)

The first concrete provider, and the only one implementing all three kinds. TorBox exposes both a torrent
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

**Also confirmed live:** every `mylist`/`getqueued` call now sends `limit=1000`
alongside `bypass_cache`, matching what
[rdt-client's own TorBox client](https://github.com/rogerfar/rdt-client) (via
`TorBox.NET` v2.1.0) always sends — AcerviNode previously sent `bypass_cache`
alone. Verified directly against a real account with three alternating
back-to-back requests: response bytes were identical either way (the account
is well under the 1000-row cap), but omitting `limit` was consistently 2–4x
slower per call (e.g. 5.29s vs 1.51s) — TorBox's server evidently does more
work per request without a `LIMIT` clause, not a payload-transfer effect.
Requested directly, comparing AcerviNode's perceived responsiveness against
rdt-client running the same real-world setup.

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

### Usenet post-processing states

TorBox's usenet service doesn't just fetch NZB articles — it also runs its
own SABnzbd-style post-processing server-side (par2 verification, par2
repair if verification fails, archive extraction) before a download is
actually retrievable, the same steps a real, self-hosted SABnzbd instance
runs locally. TorBox's own [help
center](https://support.torbox.app/en/articles/9928977-download-statuses)
describes this and calls the surfaced `download_state` family **"Direct
Unpack"** (e.g. `"Direct Unpack: Verifying"`, `"Direct Unpack: Repairing"`,
`"Direct Unpack: Completed"`) — not documented as an exhaustive list, and
not reachable to scrape directly (the help center article 403s a plain
fetch). This family is entirely absent from `mapDownloadState`'s own
vocabulary (see [State mapping](#state-mapping) above): that mapping was
ported from decypharr, which doesn't route usenet through TorBox at all — it
runs its own separate NNTP/par2/unpack engine — so it was never exercised
against these states in the first place. Reported live: a usenet download
mid-verify/repair/extract would fall through `mapDownloadState`'s "anything
unmatched is an error" default and show as failed while TorBox was still
legitimately working on it.

Confirmed as a real, independently-hit bug in a comparable open-source
project, not just inferred from the help center's prose:
[Viren070/AIOStreams issue #903](https://github.com/Viren070/AIOStreams/issues/903)
documents a real production failure from exactly this family —
`download_state = "Direct Unpack: Completed"` arriving with
`download_present` still `false`, `download_finished` already `true` — and
its fix (`packages/core/src/debrid/torbox.ts`) is what
`internal/debrid/torbox/usenet_provider.go`'s `mapUsenetState` is modeled
on. The design was shipped before it could be independently reproduced live
(no safe test NZB was available yet — unlike the Creative Commons torrent
used elsewhere in this project, see [Managed vs. Manual](#managed-vs-manual)
above, there's no equivalent public-domain NZB without an actual indexer/API
key) — the user supplied two real ones directly for this purpose shortly
after, closing that gap; see below for what was actually observed.

**Live-verified against two real usenet downloads, submitted directly by the
user for this purpose** (a small single-file NZB and a 6.8GB DVD9 boxset,
both added through the SABnzbd shim and polled every 1-2 seconds against
TorBox's raw API in parallel with AcerviNode's own translated status).
Neither actually exercised the documented `"Direct Unpack: <phase>"` family
at all — both went `"downloading"` → **`"processing"`** →
`"completed"`, where `"processing"` is a distinct, real, live-confirmed
TorBox state (`download_finished=true`, `download_present=false`,
`active=true`), held for several minutes on the larger file. TorBox's own
help center documents `"Processing"` as its own separate phase ("doing some
processing in the background and putting the file in the correct spot...
usually takes less than 5 minutes"), distinct from the Direct Unpack family
— this may be the more common real-world case for a straightforward
download, with granular Direct-Unpack sub-states reserved for ones that
actually need repair (neither test file did). The fix held up regardless:
`"processing"` was never explicitly coded for, and was still correctly
classified as still-downloading (not error) purely through the
`active && progress > 0` fallback — the exact robustness the redesign was
for. `usenetPhase` and `sabnzbdPhaseStatus` were extended afterward to
surface `"processing"` explicitly too (as `"Verifying"` — see below), rather
than leaving it in the generic `"Downloading"` bucket now that it's known to
be a real, common case and not just a hypothetical one.

The design deliberately isn't another exact-string whitelist — that would
just break again the next time TorBox adds or renames a phase. Instead,
`mapUsenetState` treats `download_present`/`download_finished`/`active`/
`progress` as authoritative, and only consults the raw string for the two
outcomes that genuinely need it:

- **Completed**: `download_finished && (download_present || state starts
  with "direct unpack: completed")` — the `download_present` check is the
  ordinary path; the string check is the AIOStreams-confirmed exception,
  for the narrow window where TorBox's own "ready" field hasn't caught up
  with the state string yet.
- **Error**: the raw state contains `"fail"` or `"invalid"` (case-insensitive
  substring, not exact match — real SABnzbd-style failures come in more than
  one exact phrasing, e.g. `"Failed"` vs. a more specific par2-repair
  failure message).
- **Downloading**: `active && progress > 0` — covers plain `"downloading"`
  *and* every `"Direct Unpack: <phase>"` sub-state uniformly, without
  needing to know each one's exact spelling.
- Otherwise: `queued` (or `unknown` for an empty string) — the same safe
  defaults as before.

`DownloadPresent`/`Active` are real, documented fields on TorBox's own SDK
response schema (`torbox-sdk-js`'s `GetUsenetListOkResponseData`) that
weren't modeled in `UsenetDownload` until this needed them — torrent/webdl
don't get the same treatment here, since "Direct Unpack" is explicitly
usenet/SABnzbd-specific terminology and no equivalent gap was found on
either of those services.

**Surfacing the phase, not just avoiding the bug.** Knowing a download is
merely "still active" isn't the same as showing *which* step it's on — real
SABnzbd reports distinct `Verifying`/`Repairing`/`Extracting`/`Moving`
statuses, and Sonarr/Radarr's own `SabnzbdDownloadStatus` enum already has
first-class members for all four (confirmed against Sonarr's real source),
so there was no reason to collapse them into one generic `"Downloading"`
once the underlying state was being read correctly anyway.
`debrid.DownloadStatus` gained a `Phase` field (never persisted — read
fresh on every poll, the same treatment as `ETASeconds`) that
`usenetPhase` derives from the raw state via the same kind of
substring match as `mapUsenetState`'s own failure detection — matched
against the text *after* the last colon specifically, since every state in
this family shares the literal `"Direct Unpack:"` prefix, which itself
contains `"unpack"` (matching the whole string would wrongly tag `"Direct
Unpack: Completed"` as extracting). `internal/sabnzbd/queue.go`'s
`sabnzbdPhaseStatus` maps that phase to the matching real status string.
`"processing"` (see above) has no exact real-SABnzbd equivalent — reported
as `"Verifying"`, real SABnzbd's own first post-download step and the
closest safe match for the same pipeline position (after
`download_finished`, before `download_present`), rather than sending the
literal word `"Processing"`, which has no member in Sonarr's
`SabnzbdDownloadStatus` enum and risks a deserialization error there instead
of just an imprecise (but safe) label here.

**A genuine failure, live-verified.** The user later supplied a real NZB
specifically engineered to fail par2 repair (too few repair blocks present
in the release), closing the last gap in this section — every prior test
NZB happened to succeed. TorBox's own raw state came back as
`"failed (Repair failed, not enough repair blocks (165 short))"` within
under a minute. Confirmed correctly classified as `StateError` by the
existing `"fail"` substring check, and confirmed the full detail reaches
both `GET /api/v1/downloads`'s `error_message` and the SABnzbd shim's
`fail_message` (`mode=history`) — the field a real \*arr app actually reads
to decide a download needs a new release — without any code changes
needed; both were already wired correctly. One real bug *was* found this
way: the same raw string also contains `"repair"`, which `usenetPhase`'s
substring match doesn't distinguish from an in-progress repair, so a
download that had already failed still reported `Phase: "repairing"`.
Fixed by only ever computing `Phase` when the mapped state is
`StateDownloading` — it was only ever meant to describe an in-progress
sub-phase, and a terminal state (error or completed) has no sub-phase to
report at all.

One further real distinction, not TorBox-reported but genuinely
AcerviNode's own: once a usenet download reaches local `provider_completed`
(TorBox is done; [Completed Download Handling](#completed-download-handling-internalimporter)
hasn't fetched the files to local disk yet), the queue now reports
`"Moving"` instead of a generic `"Downloading"` — real SABnzbd's own
"post-processing done, now placing files into their final location" phase,
which is exactly what AcerviNode's own fetch-to-local-disk step is.
Confirmed safe against Sonarr's real source (`Sabnzbd.cs`'s `GetQueue`):
`Moving`, like every other in-progress `SabnzbdDownloadStatus`, falls to a
catch-all `DownloadItemStatus.Downloading` — never treated as ready to
import, so this can't trigger Sonarr looking for files before
`internal/importer` has actually placed them, the exact same safety
property plain `"Downloading"` always had.

**What doesn't apply:** real SABnzbd's "organizing" concept (renaming/
sorting completed files, or a category-based post-processing script) has no
TorBox-side equivalent to surface — TorBox just serves whatever it
extracted, as-is, via its own file listing. There's no separate
"organizing" phase for AcerviNode to pass through.

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
subscription), `is_subscribed`, `premium_expires_at`, `total_bytes_downloaded`,
`cooldown_until`.

#### `cooldown_until` — a real, undocumented account restriction

Found live while investigating a real "everything looks frozen" report — every
download's `progress` had stopped updating, `RefreshFromProvider`'s mass-vanish
warning was firing on literally every single tick, hours running, and
`refreshKind`'s bulk `List()` calls (both kinds) were consistently returning zero
items. Confirmed this wasn't AcerviNode's own request at fault by replicating it
by hand (`curl` with the exact same `bypass_cache=true&limit=1000` params, same
key) directly against TorBox — same empty result. `GET /user/me`, checked next,
had `cooldown_until` set to a real future timestamp (roughly 24h out from the
account's own `updated_at`) — every listing endpoint stayed empty for as long as
that held.

That specific causal mechanism (`cooldown_until` being *why* listings are empty,
as opposed to a coincidental correlation with some other account-level state) is
**not independently confirmed from TorBox's own documentation** — there isn't
any found for this field at all. The correlation observed live was exact and
repeatable in the moment, which is the most that can honestly be claimed. Given
the account's own usage counters (`torrents_downloaded`/`usenet_downloads_downloaded`)
were both very high at the time, this reads like a TorBox-side anti-abuse/rate
cooldown triggered by sustained heavy API usage — this project's own long-running
live-testing sessions being the most likely cause on the account it was found on,
not something a normal personal-use pattern would be expected to trigger.

AcerviNode doesn't change any polling behavior based on this field — no special
backoff, no different retry logic — it's surfaced purely for visibility:
`debrid.AccountStatus.CooldownUntil` → `GET /api/v1/settings/account`'s
`cooldown_until` → a warning banner in the Settings page's TorBox account section
whenever it's set to a future time. Without this, the exact same "why has nothing
updated in hours" investigation would otherwise require reading logs or querying
TorBox directly by hand, same as how this was actually found.

### One provider listing per interval, shared by everything

`internal/importer`'s background poll and both compat shims' reactive
refreshes (qBittorrent's `/torrents/info`, SABnzbd's `mode=queue` and
`mode=history`) all need the same thing: the provider's current listing for
a kind. Each of them used to fetch its own — the shims on *every single
request*, so provider load scaled directly with how many *arr apps were
connected and how fast they polled. Sonarr, Radarr, Readarr and Lidarr each
polling more than one of those endpoints per cycle multiplies fast, and
since TorBox meters rate limits per API key across its servers (v8.4.1),
tripping that limit stalls *every* kind at once — while a kind is in
rate-limit backoff `refreshKind` skips its listing entirely, so nothing
advances and the whole app looks frozen.

A single `debrid.ListCache` now lives on each shared `Dynamic*Provider`
wrapper — the same pointer the importer and the shims already hold — so one
fetch per kind per interval serves all of them, and concurrent callers share
an in-flight call instead of each starting another. Its TTL tracks
`import_interval_seconds` (`Importer.SetConfig` retunes it live): that
setting is already the user's answer to how often the provider should be
asked, and a shim request has no reason to answer it differently. Measured
against the real API, four simulated *arr apps polling continuously for 30
seconds: **3 provider calls, 2,227 requests served** — and that call count
doesn't move as more *arr apps are added.

Two details the implementation depends on:

- **A reused response keeps its original fetch timestamp.** `ListCached`
  returns when the underlying call *started*, not now.
  `database.RefreshFromProvider` gates writes on that timestamp, so
  reporting a reused response as current would let it overwrite fresher
  state that landed in between — the exact regression that guard exists to
  prevent. This is why the cache isn't simply buried inside `List()`, where
  callers would have nowhere to read the real timestamp from.
- **`ListCached` is deliberately not part of the `TorrentProvider`/
  `UsenetProvider` interfaces.** Callers reach it through an optional
  interface, so a plain provider (a test fake, or any implementation with no
  reason to know about caching) still satisfies the interface and simply
  fetches directly.

The importer's *fast* per-download poll is untouched: it uses a targeted
`Status()` lookup rather than `List()`, and exists precisely to get
genuinely fresh data on demand for one in-flight download.

### Cached & metadata previews

`CheckCached` existed on `debrid.TorrentProvider` (and `Client.CheckCachedTorrents`)
from early on, but nothing ever actually called it — real, working capability
sitting completely idle until asked about directly ("anything in the official
torbox api we don't have but should?"). Closed for all three kinds, plus a
companion by-hash metadata preview for torrents, on direct follow-up request
("finish up checkcached for all torrent/usenet/webdl. add torrent info because
why not"). See [API](api.md#cached--metadata-previews) for the endpoint shapes
the "+ Add" form's debounced preview is built on.

**The wire format doesn't match TorBox's own docs — confirmed live, not
assumed.** `checkcached`'s `hash` query parameter is documented as "comma
separated," but a comma-joined value consistently timed out against the real
API (`curl` exit 28, twice in a row); repeated `hash=` params — what
`CheckCachedTorrents` already happened to send, untested until now — correctly
returned every cached hash requested, even for two at once. `checkCached`
(the shared helper `CheckCachedTorrents`/`CheckCachedUsenet`/
`CheckCachedWebDownloads` all now call) follows the live behavior, not the
docs.

**The hash isn't always a real hash.** For torrents it's the actual BitTorrent
infohash. For usenet/webdl, TorBox's own docs say to "md5 the link... or file"
— confirmed live to mean an MD5 of the link itself (`internal/api`'s
`md5Hex`), computed server-side so the frontend never needs to know; a
usenet download added by file upload rather than URL has no link to hash at
all, so its own check-cached endpoint only ever accepts a URL, matching the
"+ Add" form's own `url`/`link` fields exactly.

**`torrentinfo` needs no API key at all** — confirmed live and matches
TorBox's own docs ("Authorization: None required"); AcerviNode sends its key
anyway for consistency with every other call, harmless either way. A torrent
TorBox can't find enough peers for within its own search window comes back a
plain HTTP `500` with a real `detail` message (surfaced through the exact
same `doGet`/`checkSuccess` error path as everything else), not a `200` with
empty data.

**`debrid.TorrentInfoProvider` is a separate, optional one-method
interface** — same "structural, not every provider needs every capability"
shape as `AccountProvider`, not folded into `TorrentProvider` itself, since
not every future debrid provider will have an equivalent public
by-hash-preview endpoint. `DynamicTorrentProvider.TorrentInfo` delegates via
a runtime type assertion against whichever provider is currently set, the
same pattern `Account` already established — a provider that doesn't
implement it just means nothing to preview, not a hard error.

Live-verified end to end against the real account: `check-cached` correctly
reported a known-cached test torrent's hash as cached and an unrelated
fabricated hash as not cached; `torrent-info` returned the same torrent's
real name, size, seed/peer counts, and full 6-file list, and separately
returned a routine `available: false` for the fabricated hash. The
usenet/webdl `check-cached` endpoints were confirmed to hit the real API
without erroring, but not against a genuinely cached item — the test account
had no usenet/webdl downloads on hand at the time to produce a true
`cached: true` positive for either.

## AllDebrid (`internal/debrid/alldebrid`)

The second concrete provider, under `https://api.alldebrid.com`. Torrents and
hoster links only — **AllDebrid has no usenet service**, so it registers no
usenet wrapper and never appears as an option for a usenet add. That was
checked three independent ways before being treated as settled, because their
marketing copy reads as though it might.

Auth is `Authorization: Bearer <key>`, plus a required `agent` parameter on
every call (`agent=acervinode`) identifying the calling application — a request
without it is rejected regardless of the key.

Torrent endpoints used: `POST /v4/magnet/upload` (magnet),
`POST /v4/magnet/upload/file` (multipart `.torrent`), `GET /v4/magnet/files`,
`POST /v4/magnet/delete`. Hoster links use `POST /v4/link/unlock`,
`GET /v4/user/links`, `POST /v4/user/links/save`,
`POST /v4/user/links/delete`. Account status comes from `GET /v4/user`.

### Things AllDebrid's own documentation gets wrong

Each of these was found live, against a real account, and each one breaks
something real if you trust the docs instead:

- **Magnet status comes back in two different shapes.** A single-magnet
  lookup and a bulk one don't return the same structure, and the client
  accepts both rather than assuming either.
- **Saving a hoster link normalises it.** A Mega link submitted as
  `mega.nz/file/ID#KEY` comes back from `/v4/user/links` as
  `mega.co.nz/#!ID!KEY`. This is load-bearing: returning the caller's original
  link as the id would mean every later listing reported a link matching
  nothing tracked locally, so each of these downloads would look like it had
  vanished from the account moments after being added, and missing-detection
  would eventually flag them all as gone. `AddLink` therefore makes a second
  call and matches the stored form back by filename and size.
- **503 is how AllDebrid actually sheds load, not 429.** 80 concurrent
  requests produced zero 429s and 21 nginx `503 Service Unavailable`
  responses. Both map to the same rate-limit backoff; treating only 429 as a
  limit would have missed the real signal entirely.
- **`MAGNET_TOO_MANY_ACTIVE` is a rate limit in everything but name** and is
  mapped as one, so it backs off rather than erroring the download.

### How the shapes differ from TorBox

- **A saved hoster link is born complete.** AllDebrid unlocks synchronously —
  one call validates the link, resolves the host and returns a direct URL —
  so there is no in-progress web-download object to poll. Saving the link is
  what gives it a durable presence the listing can enumerate, so it maps onto
  AcerviNode's add/poll/fetch shape by arriving already `completed`. Honest,
  since by then AllDebrid really has resolved it.
- **Direct URLs are unlocked on demand, never cached.** They are short-lived
  and session-bound, so a stored one would work now and quietly 404 later.
- **No zipped multi-file download.** AllDebrid unlocks one link at a time with
  no bundling endpoint, so `RequestZipDownloadLink` says so rather than faking
  it locally.
- **No cache-check endpoint.** `/v4/magnet/instant` is gone and there was
  never a hoster-link equivalent, so `CheckCached` answers from what is
  already on the account — which is the sense the caller is asking about.
- **AllDebrid is actively retiring endpoints**, so what it answers matters
  more than what its docs list. Re-checked live against a real account:
  `/v4/user`, `/v4/user/links`, `/v4/magnet/files` and `/v4/link/unlock` all
  answer normally with no deprecation flag. `/v4/magnet/status` now returns
  `DISCONTINUED` outright — AcerviNode does not use it, reading magnet state
  from `/v4/magnet/files` instead, so nothing here depends on it. Worth
  re-running that check when AllDebrid behaviour looks off, since a
  discontinued endpoint fails as a normal API error rather than anything
  obviously version-related.
- **Torrent status is a numeric `statusCode`**: 0–3 downloading, 4 ready,
  5 and up terminal failure. A cached magnet arrives already ready, skipping
  the transfer codes entirely.

## Auth: login accounts and roles

The API key (`config.yaml`'s `api_key`) has always been AcerviNode's only
credential, and it still is for programmatic access — nothing here changes
that. On top of it, the web UI requires a real login: named accounts, a
session-cookie-based sign-in, and two roles. Requested directly, with the
reasoning "because of manual download ability and possible future
additions." Modeled directly on LibriNode's own real implementation
(`internal/api/auth.go`, `internal/config/config.go`'s
`AuthSettings`/`UserAccount`) — same PBKDF2 hash format, same in-memory
session store, same first-run wizard shape — read from the actual
sibling-project source rather than guessed at, since the whole point was
matching its feel.

**The API key stays the root-equivalent master credential for programmatic
access.** Sonarr/Radarr and scripts can't do cookie logins, so they keep
using it exactly as before — `currentRole` (`internal/api/auth.go`) treats a
matching API key as an anonymous admin session, unconditionally. This is
unaffected by anything below.

**Login is mandatory for the web UI** — there's no API-key-only way to
browse the dashboard (the original `ApiKeyGate` prompt was removed once the
one real instance had a login account of its own; a person now always signs
in with a username and password, never by pasting the API key into the
browser). `AuthSettings.Enabled()` (`len(Users) > 0`) still exists as the
underlying signal, but in practice it's permanently true from the moment the
first account is created — nothing ever removes the last one (see the
Default account below).

**Roles**: `admin` can do everything — Settings (provider keys, general
config, categories, cleanup policy, backups), user management, and both the
Managed and Manual tabs. `member` is scoped to Manual downloads only — adding,
viewing, and managing a magnet/NZB/hoster link grabbed directly, the same
things the Manual tab's own "+ Add" already does. A member has **no**
access to the *arr-driven Managed pipeline at all, and no Settings access.
The reasoning: interfering with a Managed download (retrying/deleting
something Sonarr/Radarr is actively tracking) is a materially bigger deal
than a member managing their own manual grabs, and this leaves room for
"possible future additions" (the second half of the request) to have a
natural home in the member tier without touching admin-only surface.

**A member can still reach your provider's API key, and the role boundary
cannot prevent it.** Not through Settings — every credential-bearing endpoint
refuses them — but through a file download link. TorBox authorizes a CDN
download with `token=<api key>` embedded in the URL itself (stripping it gets
a flat `400 missing field 'token'` from the CDN), and resolving such a link
for a Manual download is precisely what the member tier exists to allow. So
the key travels with the link, to anyone entitled to request one.

Treat a member account as trusted with the provider account behind it.
Proxying the transfer through AcerviNode would close it — and is technically
viable, since the CDN supports Range and the importer already fetches over
HTTP the same way — but is declined on cost rather than being impossible.
See [API](api.md#direct-file-downloads) for what was measured and why.

This is enforced server-side, not just hidden in the UI (`internal/api`):
- `downloadByID` — the single choke point every single-download handler
  (Get/Delete/Retry/Re-add/file-link/zip-link) routes through — 403s a
  non-admin touching a row whose `AddedVia` isn't `manual`.
- `handleListDownloads` forces `added_via=manual` for a non-admin regardless
  of what the request's own query param asked for.
- Every `/api/v1/settings/*` route (including the new user-management ones)
  is `requireAdmin`, not just `requireAuth` — except
  `PUT .../users/{username}/password`, which allows self-service: any
  signed-in account may change its own password without being an admin.

**The first-run setup wizard** (`SetupNeeded`, `internal/api/auth.go`'s
`handleSetup`) claims a genuinely fresh instance in one step: create the
login account (always admin, always the protected *Default* account — see
below), sign the browser in, no API key required. `SetupNeeded` is simply
`!AuthEnabled()` — login being mandatory means there's no other condition to
weigh; an instance with TorBox already configured but no login account yet
is still treated as needing setup, not as an upgrade case to route around.
Much shorter than LibriNode's own wizard (Account → Library → Metadata →
Indexer → Downloads → Done) since AcerviNode's whole setup surface is one
provider: Account → TorBox key (skippable) → Done.

**The Default account** is the one account that can't be removed or
demoted (`config.Config.RemoveUser`/`SetUserRole`) — whichever account the
setup wizard created, or whoever's since been promoted via "Make default."
Guarantees a login-enabled instance can never end up with zero admins able
to sign in. `SetDefaultUser` promotes a replacement (and admin, in the same
step) before the old default can be safely removed.

**Sessions are in-memory only** — a process restart logs everyone out,
matching LibriNode's identical choice for an identical reason: simpler than
persisting session state somewhere durable for a benefit (surviving a
restart while signed in) nobody's asked for, and consistent with this
project's own stance on the database itself (acceptable to lose). PBKDF2-
SHA256, 600,000 iterations, format `pbkdf2-sha256$<iterations>$<salt
hex>$<hash hex>` — the exact same format LibriNode uses (not that anything
reads across between the two projects; just consistency within the same
author's work). Cookie is `acervinode_session`, HttpOnly, `SameSite=Lax`,
30-day expiry.

**A session's role is cached at login, not re-derived from config on every
request** (`currentUser` reads `sess.role` straight from the in-memory
session) — so anything that changes what an account is allowed to do
(`handleSetUserRole`, `handleRemoveUser`) must explicitly revoke that
account's existing sessions itself, or they'd keep running under the old,
now-stale role until they naturally expire. `handleSetUserPassword` and
`handleMakeDefaultUser` except the *caller's own* session token from that
revocation on purpose — a password change or being made Default doesn't
change what the caller's current session is allowed to do, so there's
nothing stale to fix. `handleSetUserRole` doesn't get to make that
exception: found by code inspection, it used to except the caller's own
token the same way, which meant an admin demoting their own (non-Default)
account kept full admin access through that same already-open session
indefinitely — exactly the privilege the demotion was supposed to remove.
Fixed: it revokes all of the target account's sessions unconditionally,
including the caller's own if they targeted themselves. The web UI's own
admin session is not exempt from this — it authenticates every call via the
session cookie, never the raw API key (`activeKey` in `App.tsx` is always
`''` once signed in) — so a self-demotion through Settings → Security really
does end the acting admin's own session immediately; the next background
poll (within `POLL_INTERVAL_MS`) gets a `401`, which `handleUnauthorized`
already turns into a graceful return to the login screen, no separate
frontend fix needed.

**Verified live**, not just in tests: a separate scratch instance (own
`config.yaml`/data dir, never the real one) was taken through the entire
flow for real — fresh install correctly reported `setup needed: true`,
`POST /setup` created the first admin and signed in, a second account added
as `member` correctly got 403 from `/settings/general` and 200 from
`/downloads` (empty), correctly reached the provider layer (503, no TorBox
key on the scratch instance) rather than being blocked by auth when adding a
webdl link, and logging out correctly ended the session (401 on the next
call). The real, already-configured WSL instance was deliberately *not*
used for any of this — adding even one test user there would have become
permanently un-removable (the Default-account protection) and would have
flipped it into requiring login for a real, currently-in-use instance,
which is exactly the risk this design is supposed to avoid inflicting on
anyone by accident.

Once the user had actually created a real admin account on the real
instance through separate live testing, login being permanently enabled
there stopped being a hypothetical — at that point the `ApiKeyGate` browser
prompt was dead code (the real instance can never go back to having zero
accounts), so it and the `SetupNeeded`/`TorBoxConfigured` composition were
both removed in favor of the simpler, permanent design described above.
Verified by redeploying straight to the real instance (already past setup,
already logged in as `dan`) and confirming it came back up unaffected.

## TLS: self-signed HTTPS

The web UI's "stream every file straight into a folder you pick" download mode
(`web/src/fsAccess.ts`'s `supportsDirectoryPicker`) depends on the browser's File
System Access API, which only exists in a *secure context* — HTTPS, or
`http://localhost` — confirmed against MDN and Chrome for Developers' own docs.
An instance reached over a plain LAN IP (`http://192.168.x.x:7846`, say — the
common case for a home server or a Proxmox VM) never satisfies that, and no
client-side trick works around it: the HTML `download` attribute's filename
sanitizes any `/` to `_` in every browser, specifically to prevent exactly this
kind of folder-path faking.

**Native, self-signed, auto-generated TLS**, not a reverse proxy. Real precedent
for this being a normal pattern, not a workaround: Portainer auto-generates a
self-signed cert and serves HTTPS by default on port 9443 (confirmed against
its own docs); Synology DSM, Unraid, and pfSense/OPNsense's admin UIs do the
same. It's the standard shape for *appliance-style* self-hosted software — a
local control surface, not assuming the operator already runs a reverse proxy
— a different convention than the *arr ecosystem's "always proxy it yourself,"
and a better fit here since the actual trigger (a secure-context-gated browser
API) is a fairly modern need most older self-hosted software never had to
solve. A reverse proxy (Caddy) was considered and dropped: a real,
warning-free certificate (Let's Encrypt) needs a public domain to validate
against, which a private LAN IP doesn't have, so self-signed is unavoidable
either way — a proxy would just add a second service to run for no gain over
doing it natively.

**Dual-listen, always** (`cmd/acervinode/main.go`'s `run`) — the existing
plain-HTTP listener on `port` keeps running completely unchanged; when
`tls_enabled`, a *second* `*http.Server` listens on `tls_port`, serving the
exact same handler — same routes, same auth model, nothing security-relevant
differs between the two. Nothing already pointed at `http://...` (Sonarr/
Radarr, scripts, bookmarks) is ever affected by turning this on or off.

**The certificate** (`internal/tlscert`) is ECDSA P-256, self-signed, valid
10 years — deliberately no rotation/renewal logic, matching this project's
"acceptable to keep simple" stance elsewhere (e.g. the database itself).
SANs cover loopback (both families), `localhost`, the machine's own hostname,
and every IP `net.InterfaceAddrs()` reports — generated once and reused as-is
on every subsequent start (`EnsureCertificate` never silently regenerates: a
device that already clicked through the browser's trust warning shouldn't
have to do it again for no reason). Stored under `<data_dir>/tls/{cert,key}.pem`
— already inside the systemd unit's `ReadWritePaths=/var/lib/acervinode`, so
no hardening changes were needed. An operator can supply a real certificate
instead via `tls_cert_file`/`tls_key_file` (e.g. one obtained through
Tailscale's own cert tooling) — config/env only, deliberately not an editable
Settings UI field, the same treatment `data_dir` already gets.

Cert generation failing when `tls_enabled` is true is **fatal** — `run()`
refuses to start rather than silently falling back to HTTP-only. An operator
who explicitly turned TLS on and got quietly downgraded would have no
indication why the folder-picker still doesn't work.

**The SAN-drift caveat.** A cert's SANs are only as good as what AcerviNode
could detect about itself at generation time — if the machine's LAN IP later
changes (a DHCP lease renewal, a different network), the existing cert stops
matching and the browser hard-rejects it, not a soft warning. Settings → General
has a **Regenerate certificate** button for exactly this (mirrors "Regenerate
API key"), which forces a fresh cert and requires a restart to load it — refused
if a `tls_cert_file`/`tls_key_file` override is configured, since regenerating
something the operator supplied themselves isn't this feature's place.

**Restarting.** Settings changes that need a restart to apply (`port`,
`tls_enabled`/`tls_port`, ...) already persist to `config.yaml` the moment
they're saved — same as before this feature — but actually restarting is now
a one-click **Restart now** action (`POST /api/v1/settings/system/restart`)
instead of an SSH session, reusing `run()`'s existing shutdown path:
`signal.NotifyContext`'s own `stop` function is wired in as the restart
trigger, so calling it marks the same context Done an actual SIGTERM would,
with zero new shutdown plumbing. This is deliberately never automatic-on-save
— restarting the instant a field is saved would drop the admin's own session
mid-edit with no chance to reconsider.

This only actually brings the process back, though, where a supervisor is
watching it. `packaging/acervinode.service` changed `Restart=on-failure` to
`Restart=always`, since the endpoint's clean exit (code 0) is exactly what
`on-failure` is defined to *not* restart — but an already-installed unit file
doesn't pick that up from a binary update alone (see
[Installation](installation.md#upgrading-an-existing-install)), and a raw
`nohup`'d dev instance was never supervised at all. `Settings.SupervisedBySystemd`
checks for `INVOCATION_ID` (set by systemd for every unit it runs) so the UI can
tell the difference — a confident "Restarting…" when something's actually
watching, an honest "nothing will bring this back" when nothing is.

**The first-run setup wizard** gained an HTTPS step (between TorBox and Done)
offering the same toggle, for the same reason every other wizard step exists:
so a fresh install doesn't require a trip to Settings for something most
people will want immediately. Enabling it here calls the general-settings
endpoint and the restart endpoint back to back, then — critically — displays
the literal `https://<host>:<port>` URL to visit afterward as text, not just a
toggle: the restart doesn't move the current browser tab anywhere on its own,
so without an explicit URL the admin would have no obvious next step.

## Adding a new provider

1. New subpackage under `internal/debrid/<name>/`.
2. Implement only the interfaces the service actually has a real backend for.
   Implementing `UsenetProvider` against a service with no usenet is worse than
   not implementing it: a provider that isn't registered for a kind never gets
   routed one, whereas a stub gets routed adds it can only fail.
3. Add a constructor to `knownProviders` in `cmd/acervinode/main.go`, matching
   `providerConstructor` — this stays the only place a concrete provider type
   is named outside its own package.
4. Declare what it supports in `knownProviderCapabilities` in the same file.
   `registerProviderEntry` reads it to decide which wrappers to register, and
   it is what the settings UI uses to decide which kinds to offer.

That's all. Nothing in `internal/qbittorrent`, `internal/sabnzbd`,
`internal/database` or `internal/api` needs touching — that's the point of the
interface seam, and adding AllDebrid changed none of them.

A few things worth knowing before writing the client, all learned the
expensive way:

- **Verify what the service actually supports before implementing it**, not
  from its marketing pages. AllDebrid's copy reads as though it does usenet;
  it does not.
- **Map every way the service signals overload** to the rate-limit error, not
  just HTTP 429 — AllDebrid uses 503, and names one of its limits
  `MAGNET_TOO_MANY_ACTIVE`. Backoff only works if the signal is recognised.
- **Return ids exactly as the service will report them back** in its own
  listing. If saving or adding normalises what you sent, the normalised form
  is the id — otherwise every later listing matches nothing locally and
  missing-detection eventually declares the downloads gone.
- **Don't cache resolved direct URLs** unless the service documents them as
  durable. Most are short-lived and session-bound.
- **Test against a real account.** Every provider-specific bug listed in this
  document was found live; none surfaced in unit tests against a fake.
