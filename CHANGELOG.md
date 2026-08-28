# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Tests

- **Covered the backup scheduler loop**, which had no tests at all — every
  existing one called `RunOnce` directly, leaving the one path that actually
  runs unattended for months unexercised. Now covers firing on the interval,
  deliberately *not* firing at startup, a zero interval meaning "never" rather
  than "immediately", `SetConfig` retuning a running loop in both directions,
  and clean shutdown on context cancel. Backup coverage 76% → 87%.

- **Covered every `Dynamic*Provider` delegation for the unconfigured case.**
  This is the seam the whole application routes through, and "no provider
  configured" is an ordinary state — a fresh install before anyone pastes a
  key, or an entry whose key was cleared. All 27 methods across the three
  wrappers must answer `ErrNoProvider` rather than dereference a nil
  provider, since a panic there takes down the process serving both compat
  shims and the web UI. Only two of them (`AddMagnet`, `Status`) had a test.
  `internal/debrid` coverage 61% → 86%.

- **Covered both compat shims' file-upload paths**, which had no tests at
  all despite being primary \*arr flows — plenty of indexers hand over a
  `.torrent` or `.nzb` rather than a link, and Sonarr/Radarr upload the file
  rather than passing a URL. Both were verified against the real deployment
  first (a genuine archive.org `.torrent` through the qBittorrent shim, an
  NZB upload through `mode=addfile`) and then pinned. Also covers an add
  carrying neither link nor usable file, and that the upload branch still
  enforces the API key rather than bypassing it. `internal/qbittorrent`
  68% → 73%, `internal/sabnzbd` 68% → 77%.

- **Covered the provider lifecycle** — add, configure, set-default, remove
  — which had no test at all despite being the surface the whole
  multi-account feature runs on. That absence is why the Type-dropping bug
  above survived: nothing exercised the path. Also pins that adding a
  duplicate name conflicts rather than silently overwriting an existing
  account's credentials, that an unknown type is refused rather than
  registered as a dead entry, that removing the default leaves a usable one
  behind, and that clearing a key leaves the provider registered so it can
  be reconfigured without a restart. `cmd/acervinode` 67% → 73%.

- **Covered the two queries behind the features that delete things.**
  `ListStuckDownloads` (which the watchdog auto-errors) and
  `ListErroredDownloadsEligibleForCleanup` (which cleanup deletes, files
  and provider-side download included) both had no test, and both default
  to disabled — so the first time anyone switched them on would have been
  the first time the query ran in anger. Both verified correct: only stale
  in-flight rows and only stale errored rows respectively, never
  `ready_for_import`, never a freshly-errored row someone hasn't seen yet.
  Also covers `ListDownloadsByProvider` directly, since until now it was
  only exercised through the importer — a dropped `WHERE` there would have
  surfaced as downloads wrongly flagged gone rather than a failing test.
  `internal/database` 73% → 78%.


### Added

- **A hint when base64 unwrapping is switched off**, and only then. Turning
  the switch off makes the feature disappear silently, which is a trap this
  project created for itself the moment the switch existed — a setting that
  changes behaviour invisibly is worse than no setting.

  Deliberately the only such hint. A paste can fail to unwrap for several
  reasons — cut short, nested past the depth cap, never encoded at all — and
  every one of them looks identical: nothing happens. Only this one is ours to
  explain. Guessing at "that looks encoded" for the others misfires on TOTP
  secrets, bare infohashes and MD5s, all of which are base64-shaped and none
  of which are encoded anything.

  No heuristic involved: it unwraps the same input with the switch on and
  compares the result. Writing that comparison the obvious way first — testing
  only whether enabling produced something — was wrong, and its own test
  caught it: percent-escapes unwrap either way, so it claimed the switch was
  to blame for a link that had unwrapped perfectly well.


- **Base64 link unwrapping can be switched off**, the counterpart to the base32
  switch and its opposite: opt-*out*, on by default, because unwrapping is
  useful and already shipped. Off, an encoded link stays as the text it looks
  like and you unwrap it yourself — for anyone who would rather nothing they
  paste is ever rewritten.

  Percent-escapes keep unwrapping regardless. They are the far less surprising
  of the two: an escape only turns a link back into the link it already was,
  where base64 turns text into something else entirely.

  Purely a browser-side convenience, and the setting says so — nothing on the
  server or at any provider base64-decodes an add either way. The client
  defaults it on before settings load, so the brief window before the call
  returns behaves like the common case rather than silently refusing.


- **Base32 infohashes are now opt-in**, off by default, with the trade spelled
  out in Settings. A v1 infohash has a 32-character base32 spelling that some
  older trackers still hand out — but that shape is indistinguishable from any
  other 32-character base32 string, so a two-factor secret or an API key pasted
  into the add box was read as a torrent and attempted as a download. The add
  failed at the provider rather than fetching anything, and the string went
  nowhere else, but it was still an add nobody meant to make.

  The switch is threaded as an option rather than read from a module-level
  flag, so the detection functions stay pure and the property sweep stays
  deterministic. Tests that rely on base32 now enable it explicitly, which
  documents the coupling.

  A member always sees the safe default: the setting is read from the same
  admin-only call as the Managed defaults. The server decides what an add
  actually does either way, so the worst case is a member unable to paste a
  base32 hash, not one being mis-added.


- **Batch file: add a whole list from a file.** A third mode on the add form,
  for a file that *lists* downloads rather than being one. It reads the file and
  queues its contents, so a `.txt` of links behaves exactly like pasting them —
  prose, bullets, numbering, per-line base64 and percent-encoding all handled,
  because it is literally the same extraction.

  It only opens a `.txt` or a file with **no extension at all**, which is why
  this is its own mode rather than a quiet addition to File(s). A batch file
  gets opened and everything inside queued, so "anything that might contain a
  link" is the wrong bar for what to read. The allow-list grows as formats are
  actually parsed — jDownloader's `.crawljob` and `.dlc` next — rather than by
  tolerating whatever turns up.

  Extensionless files are also why the picker sets no `accept` filter here:
  "no extension" cannot be expressed in one, and filtering would hide exactly
  the file the mode exists for. The limit is enforced on the filename after
  selection instead. Both file modes share one picker element, so switching
  between them re-judges the same selection rather than clearing it — which is
  what makes "switch to Batch file to add it" an instruction rather than a
  restart.

  Reading is defensive: strict UTF-8 decoding rejects binary before anything
  looks for links in it, a UTF-8 BOM is handled, a UTF-16 file is refused
  rather than half-read, at most 512 KB is read (trimmed back to a newline, so
  a truncated read cannot land mid-character), and links are deduped and capped
  across all uploaded lists *together* — several files are not a way around the
  100-item limit one paste gets.

  Verified end to end with a real extensionless file full of prose: three links
  extracted by the shipping code, all three added against the live instance,
  then cleaned up. Four rules mutation-checked — the extension allow-list, the
  last-dot rule, NZB detection running before the link scan, and the shared cap.


- **Base32 infohashes, and percent-encoded links.** Two more paste formats that
  previously went nowhere.

  A v1 infohash has a base32 spelling — 32 characters rather than 40 — that
  older trackers still hand out, and it was not recognised as a torrent at all,
  in the UI or the API. It is now converted to hex server-side, so the same
  torrent pasted either way lands on one canonical magnet. Uppercase only: 32
  mixed-case alphanumerics is the shape of every API key, session token and
  TOTP secret going, and claiming those are torrents would be worse than
  missing the odd lowercase hash. Verified live — adding Sintel by its base32
  hash returns the hex hash from TorBox.

  Percent-encoded links (`https%3A%2F%2Fhost%2Ffile.zip`, what you get copying
  out of a redirect URL) now unwrap the same way base64 does, including when
  the two are stacked or mixed in one batch.

  That second one is safe only because unwrapEncoded returns early on anything
  already usable, and mutation testing caught that this was **not actually
  pinned by a test**. The `%20` case that was covered passes either way — a
  raw space makes the result unrecognisable, so the landing rule rejects it
  regardless. `%2F` is the case that matters: an encoded slash is not a path
  separator, so decoding it yields a different URL that still looks valid and
  would have been accepted. There is now a test for precisely that.

  While in there, the control-character regex moved from literal control bytes
  to escapes, so `detect.ts` is no longer treated as a binary file by grep.
  Worth noting the lint warning this surfaced was pre-existing and merely
  invisible: the rule never saw the raw bytes. It is now suppressed explicitly,
  with the reason written down — matching control characters is the entire job
  there, since that is what tells a decode that produced binary from one that
  produced text.


- **The add form takes a whole list, and multiple files.** Paste a batch and it
  is cleaned to the links it holds, one per line, and the field grows to show
  them; paste a single link and the field is exactly what it was. Kinds are
  detected per item, so a magnet, an `.nzb` link and a hoster link in the same
  paste each route to their own endpoint. The file picker takes several files
  the same way, each identified from its own leading bytes, so one upload can
  mix `.torrent` and `.nzb`.

  Junk removal is emergent rather than a blocklist, which is the part worth
  keeping. Splitting on whitespace is safe by construction — a magnet
  percent-encodes spaces, a URL cannot hold a raw one, base64 has no whitespace
  and an infohash is hex — so prose falls away on its own: a word is just a
  token that fails to be, or to decode into, something recognisable. There is
  no "is this junk?" heuristic to get wrong.

  Each item is base64-decoded independently, so one line five layers deep and
  another in clear text both come out right. A single blob that decodes to a
  whole list is expanded into a batch. That last one meant relaxing the
  "a decode must land on one recognisable value" rule to "…or on a list of
  two or more", which in turn exposed a latent bug: `isRecognisable` anchored
  at the start but never required a single token, so a decoded list beginning
  with a magnet was swallowed whole as one enormous link. Both rules are
  mutation-checked.

  No new API surface: a batch is N calls to the three existing endpoints, three
  at a time. On a provider 429 the rest are abandoned rather than piled on and
  reported as not attempted. A partial result keeps the successes, rewrites the
  box to hold only the failures, and gives a reason per link.

  The field is now always a `<textarea>`, never an `<input>` swapped out for
  one when a batch appears — changing the element type remounts the node and
  drops focus mid-typing. At one row it is sized to exactly what the input was.
  Sanitizing runs on paste only; doing it per keystroke would eat a link
  halfway through being typed on a second line.

  Verified live: a mixed batch of four (magnet, bare infohash, two hoster
  links) fired three-at-a-time against the real instance, all four tracked with
  the right protocol.

  Known limitation: a URL hard-wrapped across lines by a plain-text email
  client arrives as two broken tokens and both are dropped. Rejoining cannot be
  done safely — a magnet fragment plus a stray word still looks like a magnet.


- **The add form unwraps base64-encoded links.** Paste a link that has been
  base64'd and it decodes in place, repeatedly if it was encoded more than
  once, until it reaches a magnet, infohash or URL. Standard base64 and the
  URL-safe alphabet, up to ten layers deep. Verified against a magnet nested
  five deep: 312 characters of base64 in, a working add out.

  A decode is only kept if it **lands** on something recognisable, and that
  rule is doing real work. A bare infohash is itself valid base64 — forty hex
  characters, the right alphabet, length divisible by four — and decodes
  cleanly into binary noise, so accepting decodes on their own merit would
  have silently destroyed the hash support added in the same session.
  Requiring the result to be a magnet, hash or URL means a decode must be
  going somewhere before it is taken.

  Shown rather than done silently: the field reports "decoded from base64
  ×5" and one click restores exactly what was pasted. Rewriting someone's
  input with no sign it happened and no way back is a poor trade for the
  convenience.

  A depth cap of ten bounds input that keeps decoding into more decodable
  nonsense, which would otherwise spin in the browser.

- **Torrents can be added by bare infohash.** Pasting a hash on its own —
  40 hex characters for v1, 64 for v2 — now works everywhere a magnet does,
  in the add form and through `POST /api/v1/downloads/torrent`.

  This closes an inconsistency rather than adding a convenience:
  `GET /api/v1/downloads/torrent/info` already accepted a bare `hash=` and
  previewed it happily, so you could look a hash up and then not add it.
  Providers want a magnet URI, and TorBox answered a bare hash with
  "Invalid Magnet Link" (HTTP 400). It is wrapped once, ahead of every
  provider, so nothing downstream needs to know.

  The add form recognises one too, as a *certain* detection rather than an
  assumption — nothing else pasted into that field looks like 40 or 64 hex
  characters and nothing else.

- **Lifecycle options for Managed downloads you add yourself.** A Managed
  download added through "+ Add" behaves like an \*arr-added one — fetched to
  disk automatically — but nothing owns it afterwards. No \*arr imports the
  files, and no \*arr triggers the tidy-up that assumes they were imported.
  Two options now fill that gap, with defaults in **Settings → Managed adds**
  and a per-download override on the add form itself:

  - **Delete from provider once fetched.** Frees provider quota as soon as the
    bytes are local, rather than leaving the copy until `cleanup_after_days`
    runs days later. Local files and the row are untouched.
  - **Keep local files.** Exempts the download from `cleanup_after_days`.

  Both default to **on**, unlike the retention settings, because they describe
  what a hand-added Managed download is for: you asked for these bytes on this
  disk, so keeping them is the point and the provider's copy has done its job
  once they are there. Neither removes anything the operator asked to keep.

  That second one closes a real data-loss path. Cleanup's premise is that an
  \*arr already imported the files elsewhere, so removing AcerviNode's copy is
  removing a redundant one. That premise holds for an \*arr grab and fails for
  a hand-added download: nothing imports it, so cleanup was deleting the files
  the operator had asked for. With `cleanup_after_days` enabled that was a
  timer, not a hypothetical.

  **\*arr-added downloads are untouched**, and by construction rather than by
  a conditional: `added_via` is `arr` for both kinds, so it cannot distinguish
  them. The new columns are nullable and only the native add endpoints ever
  write one, so every \*arr grab leaves them `NULL` and behaves exactly as
  before.

- **Documented what removing a provider leaves behind.** `providers.md` had
  "Adding a new provider" and no counterpart. Measured against a binary
  pointed at a config naming a provider type it had never heard of, with that
  provider set as the default: startup logs `ignoring provider entry` and
  carries on rather than refusing, the default repoints itself to a registered
  provider, and downloads tracked against the absent provider stay listed,
  keep an openable detail view, and still delete cleanly — `204`, row gone,
  local files removed, with a 30-day unconfirmed tombstone rather than the
  5-minute confirmed one.

  The caveat worth knowing: removal cannot clean up the provider's own copies,
  since the code to reach that service is gone. Delete downloads *before*
  dropping a provider whose account you still hold; afterwards is fine for one
  you're finished with.

- **`GET /api/v1/status` reports a goroutine count.** Memory can sit
  perfectly flat while goroutines accumulate against a wedged provider call,
  so RSS alone cannot rule out a leak — a soak against this instance had no
  way to see one, with thread count as the only proxy and it doesn't move
  reliably. A single `goroutines` gauge rather than mounting
  `net/http/pprof`, which answers this and much more: full stacks and heap
  contents are a lot of internal detail to expose on a service holding
  provider credentials.

- **Settings warns when a compat shim has nothing behind it.** A shim with no
  capable provider answers its \*arr app's Test perfectly — Test only probes
  version and config, neither of which depends on provider state — and then
  fails every grab with "no <kind>-capable provider configured". The gap
  between a green Test and a broken grab now shows where someone would see
  it. A notice rather than making Test fail: both shims mount
  unconditionally so an \*arr can be configured before a key is pasted, and
  failing Test would break that order.

- **"Reset provider" replaces "Remove provider" for the providers this build
  knows about.** Removing one was always a slight lie: the config entry went,
  but the provider was rebuilt from the known list on the next start and the
  card reappeared unconfigured. The button now says what actually happens.

  New `POST /api/v1/settings/providers/{name}/reset` clears the credentials
  and switches every supported kind back on, keeping the entry so the
  provider stays listed and can be set up again. It deliberately leaves
  `default_provider` alone — which provider new downloads go to is a decision
  about the instance, not a property of the one being tidied.

  `DELETE` is still offered, but only where it is truthful: a second account
  exists solely in config, so removing it really removes it. That card now
  reads "Remove account".

- **A web download whose file host the routed provider can't handle now
  falls through to one that can.** Kind-level routing picks a provider before
  the add and has no idea which hoster a link points at, so an add could fail
  against the default while a perfectly capable provider sat unused beside
  it. That is not a rare shape: which hosts a service covers varies a lot,
  and on AllDebrid varies by plan — a trial account covers five hosts against
  TorBox's ~160 — so with AllDebrid as default, most hoster links would have
  failed despite TorBox being able to take them.

  New `debrid.ErrHostNotSupported`, which each provider maps its own refusal
  onto: AllDebrid's `LINK_HOST_NOT_SUPPORTED` code, and TorBox's prose (it
  returns a plain 500 with the reason only in the message, so that match is
  deliberately narrow — if the wording ever changes this quietly stops
  classifying rather than misclassifying something else).

  Narrow on purpose. Only an unsupported host is retried, since any other
  failure could mean the add partly landed and re-sending it would risk a
  second copy. And a provider named explicitly is never swapped out — that is
  a request for a specific account, the same reason `readd` refuses to
  migrate a download. Verified live against both real accounts.

- **Provider capabilities can be switched on and off per provider.**
  Settings → Provider now has a "Handles" section with a checkbox per kind,
  and everything a provider's service supports is on by default — disabling
  is something you opt into, so no existing install changes behaviour.

  A kind switched off registers no wrapper at all, which is the same state a
  provider with no such service is already in. That means routing, polling
  and the add endpoints needed no new cases: adds of that kind fall through
  to whichever provider still handles it, and the kind stops being polled
  entirely. Verified live — `torbox/usenet` disappears from
  `GET /api/v1/status`'s polled providers the moment it's switched off, and
  returns when switched back on.

  Stored as `providers.<name>.disabled_kinds`, i.e. the *disabled* set, so
  absent means everything supported is on. That needs no migration and
  avoids the ambiguity of an empty enabled list, which would have to mean
  "all" rather than the "none" it reads as.

  Enabling a kind the service doesn't have is refused with `400` rather than
  accepted and ignored — reporting success would leave you believing usenet
  was on for a provider that has never had a usenet service.

  New `PUT /api/v1/settings/providers/{name}/kinds`, and
  `GET /api/v1/settings/providers` now reports `*_enabled` alongside
  `*_capable` so a client can tell "this service has no usenet" from "usenet
  is turned off". The header chips show all three states: normal, dashed and
  faded for switched off, struck through for unsupported.

  Useful for splitting kinds between two accounts on one service to keep
  their rate limits apart, or dropping polling for a kind you never use.

- **Provider cards show which kinds each provider actually handles.**
  Settings → Provider previously showed only status (Default / Configured),
  so nothing on the page explained why a usenet add never reaches AllDebrid
  — a question that came up more than once, since AllDebrid's own marketing
  reads as though it does usenet. Each card now carries Torrents / Usenet /
  Web links chips.

  Unsupported kinds are shown struck through rather than omitted: an absent
  chip is ambiguous, since "this provider has no usenet service" and "this
  didn't load" would look identical. Styled as quiet outlined text so it
  doesn't compete with the status pills beside it, and each chip carries a
  title explaining that adds of an unsupported kind route to another
  configured provider instead.

- **Scheduled database backups.** Everything AcerviNode knows lives in one
  SQLite file — configuration, download history, categories, login accounts
  and sessions — and until now nothing copied it anywhere. It now snapshots
  itself to `<data_dir>/backups` every `backup_interval_hours` (default 24),
  keeping the newest `backup_keep` (default 7). This is the one retention
  setting here that defaults to **on**: every other one removes something, so
  doing nothing is the safe default, whereas doing nothing here is the only
  case that has a cost.

  Snapshots use SQLite's `VACUUM INTO`, so they are consistent against the
  live database without stopping the service, and each is a self-contained,
  already-compacted file with no `-wal`/`-shm` sidecars. Pruning happens only
  after a *successful* backup, never before one — trimming first would mean a
  failed backup had also thrown away a good one — and a `backup_keep` of `0`
  retains everything rather than deleting the lot. Only files AcerviNode
  wrote (`acervinode-<timestamp>.db`) are ever pruned, so a snapshot renamed
  to keep it is left alone.

  Nothing is backed up at startup, deliberately: a restart loop would
  otherwise fill the directory with snapshots of a database nobody had a
  chance to change, pushing the useful older ones out of the retention
  window.

  New `GET`/`POST /api/v1/settings/backups` (admin only) list snapshots and
  take one on demand, plus a **Settings → Backup** tab. The API returns names
  and sizes only and never a snapshot's contents — one holds every login
  account and session, which would be a far larger disclosure than any other
  endpoint here makes. Restoring is manual by the same reasoning; see
  [Backups and restore](docs/installation.md#backups-and-restore).

- **`GET /api/v1/status` now reports per-provider detail**, alongside the
  existing per-kind aggregate. The aggregate answers "is this kind working
  at all" but cannot answer "which provider is struggling": with two
  configured, one failing every list while the other succeeds still leaves
  the kind looking healthy, because the healthy one keeps the timestamp
  moving. A new `providers` array carries one entry per provider/kind pair
  actually polled, each with its own last-successful-list time and
  rate-limit cooldown. Added rather than replacing `kinds`, so existing
  monitors keep working. The Settings page shows the breakdown under a kind
  only when more than one provider handles it — with a single provider it
  would just repeat the line above it.

- **A second account on the same service can be added from the web UI.**
  Previously that meant hand-editing `config.yaml` to add an entry with a
  `type` field: the UI could manage providers that existed but not create
  one. New `POST /api/v1/settings/providers` and
  `DELETE /api/v1/settings/providers/{name}`, with a per-provider
  "Add another account" action and a Remove button.

  Deliberately *not* a general "add a provider" form. Every supported
  service already has a card whether it's configured or not — a new
  provider type appears on its own the moment the build supports one — so
  for a first account there is nothing to add, only a key to fill in. The
  only thing that genuinely needs creating is a second account on a service
  already listed, which is offered from that service's own card: starting
  there means the service is implied, so there is no type to pick and no
  way to pick the wrong one. Entries still carry a `type` underneath, which
  is what makes "torbox-work" an independent provider with its own
  credentials, listing cache and rate-limit backoff.

  Removing a provider deliberately keeps any downloads already tracked
  against it: those are records of real things, and removing a provider is a
  configuration change rather than an instruction to discard history.

- **AllDebrid is now feature-complete for what the service actually
  supports**, rather than torrent-only:
  - **`.torrent` file uploads** (`/v4/magnet/upload/file`). Previously
    refused with "use a magnet link", on the reasoning that the endpoint
    couldn't be exercised without real torrent files. It can, and now is.
    One trap worth recording: this endpoint returns its results under
    `files`, not the `magnets` every other magnet endpoint uses, so
    decoding the usual key yields nothing and an add silently looks empty.
    Confirmed against the live API.
  - **Hoster links** (web downloads), via `/v4/link/unlock` plus the saved
    links list. AllDebrid's model is unlike a service that tracks a
    downloadable object: unlocking is synchronous, so an added link is born
    complete. Saving it to `/v4/user/links` is what gives it a durable
    presence the poller can enumerate, look up and delete.

  **AllDebrid rewrites links when it saves them** — a Mega link submitted as
  `mega.nz/file/ID#KEY` comes back from the listing as
  `mega.co.nz/#!ID!KEY`. Nothing in AllDebrid's documentation mentions this,
  and it is load-bearing: using the submitted link as the download's
  provider id would mean no later listing ever matched it, so every web
  download would look like it had vanished from the account moments after
  being added, and vanished-download detection would eventually flag them
  all as gone. `AddLink` matches back to the stored form by filename and
  size instead. Found live, and covered by a regression test.

  Usenet remains unsupported because AllDebrid has no usenet service at all,
  and zipped downloads because it has no endpoint that bundles links — both
  register nothing rather than erroring at call time, so AllDebrid simply
  never appears for a kind it can't do.

- **A warning when two providers are configured with the same credentials.**
  Multiple entries exist so several *different* accounts can be used at
  once; pointing two at one account instead makes both discover everything
  on it, so every download is adopted twice and deleting it through one
  entry strands the other's row as "no longer found in the provider's
  account". Observed exactly that while testing two entries against a single
  TorBox key. A warning rather than a refusal: it is a coherent thing to ask
  for even if it is rarely what someone means, and refusing to start would be
  a worse failure than saying so.

- **AllDebrid, the second debrid provider.** Torrent-only, and deliberately
  so: AllDebrid has no usenet service at all, and its hoster debriding is a
  synchronous `/v4/link/unlock` with no pollable object behind it — there is
  no "web download" to track the way AcerviNode's model expects, and
  inventing that lifecycle locally would be guesswork rather than
  integration. The registry already supported partial capability, so it
  simply never appears for the kinds it can't do. Verified live end to end
  against a real account: add, poll, fetch to disk through the unlock path,
  and delete.

  Shapes that differ from TorBox and are captured in the code rather than
  assumed: every response is wrapped in a `status` envelope and a **failure
  arrives as HTTP 200**, so nothing can be concluded from the status code;
  `/v4/magnet/status` is **discontinued** and only `/v4.1` answers;
  `/v4/magnet/instant` **no longer exists**, so `CheckCached` answers from
  what is actually on the account; a magnet's files are a separate call from
  its status, arrive as a **tree** that has to be flattened, and each file's
  "link" is itself locked until unlocked. A cached magnet arrives `Ready`
  having transferred nothing, so progress is forced to 100% for it rather
  than derived from bytes — otherwise a finished download reads as
  untouched.

- **Two accounts on the same provider.** `providers.<name>.type` separates
  an entry's name from its implementation, so `torbox` and `torbox-work` can
  both be `type: torbox` and be configured, defaulted and routed to
  independently. Previously a provider's name *was* its implementation, so
  there could only ever be one account per service.

- **`airlocked`, surfacing TorBox AirLock on every download.** TorBox v9
  (2026-07-01) added an `airlocked` boolean to all three `mylist`
  responses; AirLock is its permanent storage, which exempts an item from
  the 30-day retention policy that otherwise removes an inactive download
  from the account. Now parsed for torrents, usenet, and web downloads
  alike, carried on `debrid.DownloadStatus`, and reported by
  `GET /api/v1/downloads` as `airlocked`. Never persisted — cached in
  `database.DB.LiveStatus` the same way `eta_seconds`/`phase`/`seeders`
  already are, since it's the provider's own state and can change from
  outside AcerviNode at any time. The download detail view shows a
  "Storage: AirLock" row when it's set, and only then: an airlocked
  download is the exception, so a row saying "not airlocked" on everything
  else would be noise. Purely informational — AcerviNode never sets it
  (that needs TorBox's separate edit endpoints) and nothing branches on it,
  though it's directly relevant to vanished-download detection, which
  exists precisely to catch a download the retention policy removed.
  Attested by TorBox's changelog rather than its SDK docs, whose published
  model files still lag v9 and don't list the field at all.

- **A `Makefile`, so updating a from-source install can't silently skip the
  frontend build.** `go:embed` bakes in whatever's already on disk in
  `web/dist` at build time, not what's in git — the actual built frontend
  files are gitignored (only a placeholder `.gitkeep` is committed), so a
  plain `git pull && go build` compiles and runs *successfully* while
  silently serving whatever UI happened to already be sitting in
  `web/dist` from an earlier build, no error, nothing to indicate anything
  changed. Found live, updating a real deployment this way — the just-
  shipped Settings reorganization wasn't showing up because the frontend
  step of the update had been skipped, not because anything was actually
  broken. `make build` always runs `npm ci && npm run build` before
  `go build`, as one command, so there's no longer a multi-step sequence
  an update can partially skip; `make build-backend-only` is a separate,
  explicitly-named target for the "build elsewhere, copy just the binary
  to a Node-less production box" workflow. CI now runs `make build` itself
  rather than the equivalent steps inline, so a regression in the Makefile
  gets caught the same way any other regression would. See
  docs/installation.md#updating-an-existing-from-source-install.

- **Three settings gaps found by comparing AcerviNode's own settings
  against rdt-client's, field by field:**
  - **Per-file fetch filtering.** `min_fetch_file_size_bytes` skips a file
    smaller than a configured size (samples, junk); `include_file_regex`/
    `exclude_file_regex` skip a file by path pattern (unwanted types,
    languages) — any combination can be set at once, a file must satisfy
    all of them. Applied in `Importer.filterFiles`, right after a
    download's file list comes back from the provider and before any of
    them are fetched to local disk. Purely local — never changes what the
    provider itself considers part of the download, or what
    `GET /api/v1/downloads/{id}`'s own `files` list reports. Immediate
    follow-on, found comparing against [decypharr](https://github.com/sirrobot01/decypharr)'s
    own settings this time: `max_fetch_file_size_bytes`, the symmetric
    counterpart to the minimum — skips a file larger than a configured
    size (e.g. an oversized bonus feature bundled alongside the main
    file). Rejected if it would make `min_fetch_file_size_bytes` exceed
    it — a range that could never match anything.
  - **Stuck-download watchdog.** `stuck_download_timeout_minutes`
    auto-errors a download that's sat `queued`/`downloading` with no
    genuine change reported by the provider for too long —
    `Importer.checkStuckDownloads`, keyed on `updated_at` (only ever moves
    on a real state/progress/size/error change, never a no-op poll), not
    simply elapsed time, so a large download still actively transferring on
    a slow connection is never punished just for taking a while.
  - **Error-state cleanup.** `cleanup_error_after_days` automatically
    removes a download that's sat in `error` for too long — local files (if
    any), the provider-side copy, and the row itself, the same removal the
    existing `cleanup_after_days` retention policy already does for a
    finished download. Applies to **both** Managed and Manual downloads,
    unlike that policy's Managed-only scope, since an error already means a
    real dead end either way — previously an errored download (retry-
    exhausted, or a vanished Manual download) had no automatic cleanup path
    at all.

  All three default to disabled (`0`/empty), live-editable with no restart
  via Settings → General or `PUT /api/v1/settings/general`. See
  docs/providers.md#per-file-fetch-filtering (and the two sections
  immediately after it).

- **The web UI now detects when it's running a stale build and prompts you
  to reload.** It's a long-lived SPA — nothing re-fetches its own JS once
  loaded, so a tab left open across a deploy would silently keep running
  old code indefinitely, which is exactly how a just-shipped fix went
  unnoticed until a manual hard refresh. `App.tsx` already polls
  `GET /api/v1/version` every few seconds; the version seen on the first
  poll is now kept as a baseline, and a later mismatch shows a small
  dismissible "A new version of AcerviNode is available" banner with a
  Reload button — deliberately a prompt, not a forced reload, since the tab
  could be mid-form-fill or mid-edit in Settings when a deploy happens
  elsewhere. Deliberately not applied to the Downloads popup window, where
  a forced reload could interrupt an actual in-progress download — the
  exact thing that window exists to survive even the main tab closing.
  Verified live: deployed two builds with different stamped versions back
  to back without touching the open tab, and watched the banner appear on
  its own within one poll tick.

### Changed

- **Backups are now opt-in.** `backup_interval_hours` defaults to `0`; set a
  value to enable them. Previously the one retention setting that defaulted to
  on, on the reasoning that doing nothing here is the only case with a cost —
  losing the database loses every account, session, category and setting, and no
  provider hands any of that back. That reasoning still holds and the docs still
  say so; what changed is who decides. A snapshot is a copy of every login
  account and session written to disk on a schedule, which is a decision to make
  deliberately rather than inherit from a default.

  **On upgrade this turns scheduled backups off** for any install that relied on
  the default rather than setting the value explicitly. Set
  `backup_interval_hours` to keep them.


- **One add field instead of Torrent / Usenet / Web Link tabs.** The form now
  works out what an input is rather than asking first. A `magnet:` scheme, a
  `.torrent` or `.nzb` path, and an uploaded file's leading bytes are all
  identified with certainty; a file is read rather than trusted by name,
  since a browser will hand over `x.torrent` containing anything.

  One class genuinely cannot be told apart: an indexer API URL
  (`https://indexer/api?t=get&id=123`) and a hoster link are the same shape,
  with no way to distinguish them short of fetching. Those are assumed to be
  web links and **shown as an assumption** — the row reads "Looks like" rather
  than "Type" — and can be corrected in one click. A correction clears as soon
  as the input changes, so it never silently carries to the next paste.

  Detection drives what the tabs used to: which providers the picker offers
  (a usenet link now narrows it to providers that do usenet), which
  cached-check runs, whether a metadata preview is fetched, and which endpoint
  the add uses. Nothing changed server-side.

  Choosing between a link and a file upload stays a separate, explicit choice,
  because it is made before there is anything to detect. Web links have no
  file-upload variant at all, so uploading now simply rejects anything that
  isn't a `.torrent` or `.nzb`, rather than offering a Web Link tab that
  could never accept the file.

- **"Add another account" is now a quiet link rather than a fifth button.**
  A second account on one service is a rare, one-time action, and at button
  weight it sat level with "Test connection" — something pressed often — and
  pushed the action row to wrap onto a second line. Still a `<button>`, so it
  keeps keyboard focus and activation; looking like a link is a visual choice
  rather than a semantic one. The capability itself is unchanged.

- **One provider listing per interval now serves the importer and every
  connected *arr app**, instead of each fetching its own. The compat shims'
  reactive refreshes previously called `provider.List()` on every single
  request, so provider load scaled directly with how many *arr apps were
  connected and how fast they polled — the exact axis that trips TorBox's
  per-API-key rate limit, which stalls every kind at once (a kind in
  rate-limit backoff has its listing skipped entirely, so nothing advances).
  A single `debrid.ListCache` now lives on each shared `Dynamic*Provider`
  wrapper, which the importer and both shims already hold the same pointer
  to, with its TTL tracking `import_interval_seconds` and retuned live by
  `Importer.SetConfig`. Measured against the real API with four simulated
  *arr apps polling continuously for 30 seconds: **3 provider calls, 2,227
  requests served**, down from 32 — and that count no longer grows as more
  *arr apps are added. A reused response deliberately keeps its *original*
  fetch timestamp so `RefreshFromProvider`'s ordering guard still treats it
  as the older data it is; that requirement is why the cache isn't buried
  inside `List()` itself. The importer's fast per-download poll is untouched
  — it uses a targeted `Status()` lookup and exists to get fresh data on
  demand.

- **A provider rate limit on an add is now `429`, not `502`.** Every
  provider failure except "no provider configured" was reported as
  `502 Bad Gateway` by the native API's add/re-add endpoints, which tells a
  caller "upstream is broken" when what's actually true is "slow down and
  try again". That distinction stopped being academic in TorBox v9
  (2026-07-01), which meters `createtorrent` at 60/hour for *uncached*
  torrents (300/minute for cached ones), and in v8.4.1, which moved rate
  limiting from per-IP to per-API-key counted synchronously across TorBox's
  servers — so anything else using the same key draws from the same bucket,
  and hitting the limit is now a routine, recoverable condition rather than
  an exceptional one. `debrid.ErrRateLimited` (already plumbed through for
  the importer's own polling backoff) now maps to `429` with the provider's
  own detail message preserved. Both compat shims keep their existing
  protocol-shaped responses — real qBittorrent answers `Ok.`/`Fails.` and
  real SABnzbd `{"status": false, "error": ...}`, so neither has an HTTP
  status to carry this, and SABnzbd's already passes the provider's message
  through.

- **Settings reorganized: three new focused groups split out of a General
  tab that had grown to a 10-field "Import & cleanup" section covering five
  genuinely unrelated concerns.** Requested directly, after Settings itself
  hit the trigger an earlier decision had already flagged for revisiting
  the section-not-tabs layout ("revisit if Settings keeps growing" —
  Phase 3). Every feature landed this session (TLS, the file filters, the
  stuck-download watchdog, error cleanup) had nowhere better to go than
  that one catch-all, so it kept absorbing more. Split into:
  - **Import** — retry count, max concurrent downloads, fetch idle timeout.
  - **Filtering** — the min/max file size and include/exclude regex filters.
  - **Cleanup** — the two retention policies and the stuck-download
    watchdog.

  General now holds only what it always had otherwise: API key, instance
  identity (port/log level/download dir/permissions), and HTTPS. Provider's
  own "Polling & timeout" section deliberately stays where it is rather
  than merging into Import — it's specifically about how often AcerviNode
  talks to *this* provider (would need to be per-provider if multi-provider
  support ever lands), unlike Import's fields, which apply to the
  fetch-to-disk pipeline regardless of which provider a download came
  from. No backend change — purely a UI reorganization, same shared
  settings object and save path (`handleGeneralSubmit`) every section
  already used.

### Removed

- **`GetHosterList` and the `Hoster` type.** Fully implemented and tested
  against `GET /webdl/hosters`, but nothing ever called it outside its own
  test — no API endpoint, no UI, and its `Domains` field was never read
  anywhere. Noticed while auditing what TorBox v8.4.3 (2026-03-31) changed
  (that release added regex link-matching patterns to the endpoint), which
  made the dead code's cost concrete: keeping it current would mean
  modeling a field nothing consumes. Deleted rather than extended; TorBox's
  hoster list is still described in [Providers](docs/providers.md#web-downloads)
  and is a `git` revert away if Web Downloads ever grows link validation.

### Fixed

- **A frequently-restarted instance never backed up at all.** The scheduler
  waited a full interval from *process start* and deliberately never backed up
  at startup, so a restart loop could not fill the directory with snapshots of a
  database nobody had touched. The unintended consequence: restart more often
  than the interval and the clock never reaches it. Found on the development
  box, which had gone **three days** without a snapshot across a period when
  every setting in the database changed.

  The schedule is now timed from the newest snapshot on disk rather than from
  startup, which needs no new state — `List()` already reports when each was
  taken. Overdue backs up within a second; otherwise it waits out the
  remainder.

  The original guarantee survives intact for the case it was written for: a
  restart loop still cannot produce more than one snapshot per interval, because
  every restart sees the one the previous attempt just wrote. Verified live —
  the deploy took the overdue snapshot within seven seconds, and two further
  restarts produced none.

  A snapshot stamped in the future, from a clock corrected after the fact, waits
  at most one interval rather than parking the scheduler for however long the
  skew was.

  One test had to be rewritten rather than kept: `TestRun_DoesNotSnapshotAtStartup`
  asserted the absolute rule. It still passed after the change — but only by
  accident, sleeping 120ms against a one-second delay — which is worse than
  failing. It is now `TestRun_DoesNotSnapshotWhenARecentOneExists`, stating what
  the rule was actually protecting.


- **Two magnets for the same torrent counted twice.** Dedupe was on the whole
  link string, so the same infohash with two different `&dn=` display names was
  two batch items — the provider collapsed them, but "Add 30" then produced 29
  rows. Dedupe now keys on the infohash when the link has one, which also
  collapses a bare hash and a magnet for the same torrent.

- **An over-sized file whose links are space-separated was refused outright.**
  The read was cut back to the last newline, and a file with none had no safe
  cut. It now cuts at the last whitespace of any kind. A file that is one
  enormous token with no whitespace at all is still refused, deliberately: a
  truncated link that still parses is worse than no link.

- **Only one reason was given when files were rejected for several.** The
  most-actionable message won and the rest went unmentioned, which made those
  files look like the add had simply lost them. Every reason is now listed.


- **Six bugs from a hunt over the day's own changes.** Four were regressions
  introduced today; two were latent and only became reachable because of them.

  **A blank web-download link reached the provider.** `normalizeWebLink`
  returned its *untrimmed* input when the link was empty, so a whitespace-only
  value was not `""` by the time the handler tested it and sailed past the
  "link is required" check. Live before the fix: HTTP 502 carrying TorBox's own
  "the link you provided is invalid". After: 400, and nothing leaves the box.
  The existing test only covered `""`, never `"   "`.

  **The kind-correction chip did nothing for an uploaded file.** The batch
  refactor started reading `item.kind` from detection where the old code used
  `protocol`, which is what the chips actually display — so clicking "Usenet"
  on a `.torrent` selected the chip and changed nothing about the request. The
  UI was reporting a choice it then ignored.

  **Web Link was offered as a correction for an uploaded file**, which has no
  upload variant at all. Latent while the override was inert; fixing the above
  would have turned it into a real misroute, so both had to move together.

  **"Switch to Batch file to add it" was given for files Batch file refuses.**
  A list of links named `x.torrent` is rejected by both modes, and being told
  to switch to one that will also say no is worse than being told outright.

  **A URL ending in a bracket was truncated.** Wrapping punctuation was
  stripped from the end whether or not anything opened it, so
  `https://host/wiki/Thing_(disambiguation)` lost its final paren and would
  404, and a magnet's `&dn=Movie+[1080p]` lost the bracket off its display
  name. Unwrapping is now done only in matched pairs; a leading wrapper on its
  own is still dropped, since nothing legitimate starts with one.

  **MEGA host matching was case-sensitive**, so `MEGA.NZ` missed normalisation
  entirely. Only the scheme and host are folded — the `F` marking a folder
  share is MEGA's own and is always uppercase, so folding that too would read
  `#f!` as a folder link when it is not one.

  Every fix has a test that fails when only that fix is reverted, and the
  blank-link one is covered at the handler as well as the function.


- **Legacy MEGA links are rewritten into the form providers accept.** MEGA's
  pre-2020 links keep the node handle and decryption key in the URL fragment
  behind `!` markers (`https://mega.nz/#F!ID!KEY`); the modern form moved the
  handle into the path (`https://mega.nz/folder/ID#KEY`). Both open in a
  browser, because mega.nz's own front end rewrites the old shape client-side —
  but a debrid provider parses the URL rather than running that JavaScript, and
  a parser reading the path finds nothing there. Folder links, file links and
  the pre-2016 `mega.co.nz` domain are all handled.

  Done **server-side**, in the `webdl` handlers, so an add from Sonarr/Radarr
  or a direct API call benefits too — not just the web UI.

  It runs on the check-cached path as well, which it has to: TorBox keys a web
  download's cache entry on an MD5 of the link, so normalising the add and not
  the lookup would report on a string that never gets added. Both wiring points
  are covered by their own test, each of which fails if only that one is
  reverted.

  A legacy folder link naming a node *inside* the folder (`#F!ID!KEY!NODE`) is
  deliberately left alone: the modern equivalent is `/file/NODE` or
  `/folder/NODE` depending on what the node is, and nothing in the URL says
  which. Guessing would silently fetch the wrong thing, while leaving it fails
  loudly with the provider's own error.

  Verified live against TorBox: the legacy and modern forms of the same folder
  both report cached, while a wrong key and a made-up link both report not
  cached — so the check genuinely discriminates rather than answering yes to
  everything.


- **Two bugs in the base64 decode notice.** The "Decoded from base64 ×N"
  notice cleared itself on the very re-render the decode caused: after a
  successful decode the field no longer matches what was pasted *by
  definition*, and the stale-check read that as a user edit. Separately, the
  "use what I pasted instead" button restored the encoded text only for the
  effect to decode it straight back, so the button appeared inert.

  Both lived in component state where nothing could reach them, so the
  transition is now a pure function — `stepDecode`/`undoDecode` in
  `web/src/detect.ts` — driven to a fixed point in tests the same way React
  drives it. Reintroducing either bug fails the suite.

  Worth recording what was *not* wrong: the report that prompted this was a
  paste truncated to 41 characters, which is not valid base64 (41 % 4 = 1).
  The decoder correctly declined it and left the text alone. The add form now
  documents that complete input is required.


- **"Cached" showed when AcerviNode noticed a download, not when the provider
  cached it.** The detail view's `cached_at` records the first moment this row
  was seen provider-complete — a fact about your download. Read under a
  "Cached" label it says something else entirely, and TorBox had the real
  answer on the wire the whole time: a torrent added today reported
  `cached_at=2026-07-27`, because someone else's download put that content in
  the cache a month earlier. AcerviNode simply never parsed the field.

  Now parsed for torrents, usenet and web downloads, stored separately as
  `provider_cached_at`, and shown as its own **"Cached by provider"** row —
  present only when the provider reports one. AllDebrid unlocks links rather
  than maintaining a cache and reports nothing resembling it, which is exactly
  why the two are kept apart rather than merged with a fallback: one label
  meaning two things was the original bug. AcerviNode's own observation stays,
  relabelled **"Available"**.

- **`retry` refused any download that wasn't in `error`.** It re-runs the
  local fetch, so it applies to anything the provider has already finished —
  but it accepted only `error`, which left a download that was *wrong without
  having given up* with no way back. Hit live: a `ready_for_import` row whose
  files never landed answered `409`, and delete-and-re-grab was the only
  escape. That specific cause is fixed, but "wrong, yet not in error" is a
  shape that recurs.

  Now accepts `error`, `ready_for_import` and `provider_completed`, and still
  refuses `queued`/`downloading` — forcing a row the provider hasn't finished
  into `provider_completed` would have the importer fetch something that
  doesn't exist yet.

- **The delete tombstone expired before the provider's listing caught up.**
  A confirmed delete kept an item out of discovery for five minutes, on an
  earlier observation that TorBox's listing could "briefly" still show a
  just-deleted torrent. Timed properly against a real account, a confirmed
  delete took roughly **six** minutes to leave `mylist` — longer than the
  window meant to cover it, so discovery could re-adopt the download as a
  brand-new Manual one in the gap. Exactly the ghost the tombstone exists to
  prevent.

  Raised to an hour, about ten times the observed lag. Erring long costs
  nothing here: a `provider_download_id` that is genuinely gone never
  legitimately reappears, since a fresh add always gets a new one, so this
  only ever suppresses re-adopting the one defunct id.

- **A download whose provider listed no files was marked ready to import.**
  The importer guarded the case where *filters* removed every file, but not
  the case where the provider returned no file list at all. That walked
  straight through: destination directory created, nothing fetched,
  `ready_for_import` set, and the download reported to the \*arr app as
  complete.

  Sonarr then parked on "No files found are eligible for import" against an
  empty folder, permanently — and there was no way out through the API,
  since `POST /downloads/{id}/retry` only accepts rows in `error`.

  Found with a real Sonarr grab, against a torrent whose metadata had not
  resolved by the time the fetch ran. An empty file list is not a finished
  download; it now returns an error so the download goes through the ordinary
  retry backoff, which is right for something expected to resolve shortly.
  Every file being *filtered* out still completes, deliberately — that is a
  real answer about a real file list.

  Verified by re-running the same grab: local fetch progressed 17.8% → 100%,
  four files landed on disk, and Sonarr imported the 1.17 GB episode.

- **The three add endpoints disagreed about request encoding.** Torrent and
  usenet accepted only `multipart/form-data`, because they take `.torrent`
  and `.nzb` uploads; web downloads accepted only
  `application/x-www-form-urlencoded`, having no file variant. Sending the
  other one got a `400` from any of them.

  The web-download case was the worse of the two: `ParseForm` ignores a
  multipart body entirely, so the request arrived with an empty form and the
  handler answered `link is required` — blaming the caller's data for what
  was actually a `Content-Type` mismatch.

  All three now share `parseAddForm`, which tolerates exactly
  `http.ErrNotMultipart` and nothing else. `ParseMultipartForm` runs
  `ParseForm` before it looks for a multipart body, so the urlencoded fields
  are already parsed by the time it reports that error — accepting both
  costs one tolerated error and stays strict about every other parse
  failure. Strictly widening: nothing that worked before stops working.

- **The torrent metadata preview asks only the provider it was aimed at.**
  It briefly fell back to any provider that supported the lookup, so with
  AllDebrid as default a preview was answered by TorBox. The reasoning was
  that a magnet's metadata belongs to the torrent rather than an account —
  true of the data, but the *query* still goes to a specific service, which
  then knows a hash you were about to hand to someone else, with nothing
  visible to say so.

  That makes it a different trade from the add fallbacks, which are kept: those
  prevent a download from failing outright and surface as a download visibly
  filed under whoever took it, where this only saved a preview panel and
  crossed providers invisibly. Reverted.

  A `?provider=` override was added alongside the revert and then removed:
  nothing called it — the web UI passes only the magnet, and no \*arr touches
  this endpoint — so it was speculative surface, and a gated way back to the
  behaviour just removed. A preview a provider can't give is simply a preview
  you don't get.

- **`GET /api/v1/status` returned `"providers": null` instead of `[]`** when
  nothing was being polled — every kind switched off, or the importer not yet
  attached during startup. The field is documented as an array, and a monitor
  iterating it crashes on null rather than seeing an empty list; the burn-in
  script hit exactly that. Both return paths now produce `[]`. Same nil-slice
  trap the downloads list hit in Phase 3, reached this time through a state
  the new capability switches made possible from the UI.

- **A release landed one folder deeper than it should.**
  `resolveDestDir` already ends the destination with the download's own name,
  and some providers report each file with that same folder in front of it —
  TorBox's file `name` is `Big Buck Bunny/Big Buck Bunny.mp4` while its
  `short_name` is just the file. The two stacked, so a release landed at
  `<category>/<name>/<name>/file` instead of the `<category>/<name>/file`
  real qBittorrent produces. Sonarr scans `content_path` recursively so
  imports still worked, which is why this went unnoticed, but the layout was
  wrong and differed between providers for the same torrent.

  Only the exact duplicate is dropped: a top-level folder that genuinely
  differs from the download name is kept, deeper structure is preserved, and
  a flat file list is untouched. Guarded against turning a contained path
  into an escaping one.

- **An add could fail against a default provider that had no credentials,
  while a configured one sat unused.** A provider stays registered without a
  key on purpose — that is what lets one be pasted in without a restart — so
  `DefaultNameFor`, which asks "is this registered for the kind", happily
  returned a provider that could only fail. Measured before changing it: with
  an unconfigured default and a configured provider beside it, an add
  returned `503` and the working provider was never tried.

  Routing now prefers a provider that actually holds credentials. Contained
  to `resolveAddProvider`: `DefaultNameFor` has fourteen callers and some
  aren't routing at all — both compat shims use it to *key cached live
  status* for rows with no provider recorded — so changing it would have
  quietly re-keyed those lookups.

  `default_provider` is deliberately left where the operator put it, and the
  compensation is logged rather than silent. When the default is reset or
  removed, the UI now asks which provider should take over instead of
  guessing: resetting to re-key a provider and resetting to abandon it look
  identical from the outside and want opposite handling, so that one
  ambiguous moment gets a question. Declining is a real choice — adds keep
  falling through meanwhile.

- **A deployed UI change could stay invisible in the browser.** The frontend
  was served with no cache headers at all, so browsers applied heuristic
  caching to `index.html` — the one file whose contents change at a fixed
  URL, and the one that points at the current fingerprinted bundle. A stale
  index went on requesting the previous bundle indefinitely, so a deploy
  simply never arrived. Found the honest way: a UI change was live on the
  server and still not visible after reloading.

  `index.html` and client-side routes are now `Cache-Control: no-cache`,
  which still allows a conditional request, so an unchanged index costs a 304
  rather than a full download. Fingerprinted assets under `assets/` are
  `max-age=31536000, immutable` — their name changes whenever their contents
  do, so a cached copy can never be wrong.

  A missing asset is also a `404` now instead of the SPA fallback. Answering
  `/assets/index-OLDHASH.js` with `index.html` handed the browser HTML under
  a JavaScript name, which fails on a MIME mismatch rather than saying the
  file is gone — the wrong answer for exactly the stale page this affects.
  Routes without a file extension still fall back to the app, so deep links
  and hard refreshes are unchanged.

- **A clean restart logged three or four `ERROR` lines.** Stopping the
  service cancels the tick's context and closes the database underneath
  whatever was mid-flight, so the provider listing failed with "context
  canceled" and the tick itself with "sql: database is closed". Nothing was
  wrong, but it made counting errors in the log useless as a health signal —
  which matters now that `GET /api/v1/status` exists to be monitored.

  Tick-path failures are now reported through `logTickError`, which stays
  quiet when the tick's context has been cancelled. Keyed on the context
  rather than the error text, because the errors vary (`context.Canceled`,
  `database/sql`'s unexported closed-DB error, whatever a provider returns
  when its request is cut short) while the cause doesn't. The importer's tick
  context is the long-lived one and never gets a deadline of its own, so a
  cancelled context there always means shutdown rather than a timeout — a
  genuine failure on a live tick still logs exactly as before. Verified: a
  restart now produces zero errors.

- **Clearing a provider's API key destroyed the rest of its entry.** It
  deleted the whole `providers.<name>` block, so everything else the entry
  held went with it. Two consequences, both found by bug-testing the new
  capability switches rather than by inspection:

  - Disabled kinds came back on. The running process still showed them off,
    so the UI and `config.yaml` disagreed until a restart resolved it the
    wrong way.
  - A **second account was destroyed outright**. Its `type` lives only in
    that entry, and `providerEntryNames` only revives names that are known
    provider types — so `torbox-work` simply ceased to exist on the next
    start, despite the documented promise that a cleared provider "stays
    registered, so it can be configured again later".

  The key is now emptied in place. Clearing a key means "unconfigure this",
  which an entry with no key already says; deleting the entry is what
  `DELETE /api/v1/settings/providers/{name}` is for.

- **Changing a provider's API key erased its type, breaking two-accounts-
  per-service on the next restart.** `SetProviderAPIKey` wrote a fresh
  `config.ProviderConfig` holding only the key, dropping `Type`. A second
  account (say `torbox-work` with `type: torbox`) kept working in memory, so
  nothing looked wrong — but `config.yaml` had lost the type, and the next
  restart resolved it from the entry's own name, found no provider called
  `torbox-work`, and the account silently disappeared. The entry is now
  updated in place. Confirmed live before fixing: the `type:` line vanished
  from `config.yaml` the moment the `PUT` returned `204`.

- **The duplicate-credential warning only fired at startup.** Adding a second
  account through the UI is the single most likely moment to paste the same
  key twice, and that path never checked — so two entries would quietly
  discover identical downloads until the next restart explained why.
  `AddProvider` and `SetProviderAPIKey` now run the same check.

- **`POST /api/v1/downloads/{torrent,usenet,webdl}` take form bodies, not
  JSON**, and the docs only said so for `webdl`. Since every other `POST` in
  the API is JSON, assuming JSON is natural — and gets a bare
  `400 invalid request body`. Now stated explicitly, with a worked `curl`
  example. (Documentation only; the endpoints were always form-based, which
  is what lets them accept a `.torrent`/`.nzb` upload in the same shape.)

- **Changing your own password logged you out of the browser you changed it
  in.** `handleSetUserPassword` passed `""` as `revokeUser`'s except-token,
  revoking every session for the account including the caller's own — the
  direct opposite of what its own comment said it did, and the same shape as
  the self-demote session bug. Now excepts the caller's token, which is
  correct in both directions: your own session survives, while an admin
  resetting someone else's holds a token matching none of the target's
  sessions, so those are still all revoked. Caught during burn-in, when the
  request right after a self password change came back `401`.

- **`POST /api/v1/settings/users` accepted any role string and silently
  downgraded it.** `config.AddUser` maps anything that isn't exactly `admin`
  to `member` — the right fail-safe for storage, but as an API answer it
  meant `"role":"Admin"` created a member account and returned `200`.
  `PUT .../role` already refused the same input; the two now agree. An
  omitted role still means `member`. No privilege escalation either way: the
  normalization always failed safe.

- **A cold torrent metadata preview died with our timeout instead of
  TorBox's explanation.** `torrentinfo` searches the BitTorrent network, and
  a hash TorBox hasn't seen takes it ~33s to give up on — past the 30s
  default request deadline, so the first preview of any uncached torrent
  returned `context deadline exceeded`. TorBox is now asked to give up a few
  seconds before we do, so the caller gets its real reason ("Could not
  download full metadata for the torrent within the alloted timeout").
  Verified live: 26.6s and a useful message, against 30s and an opaque one.

### Security

- **Documented that a `member` account can reach the provider API key.** Not
  through Settings — every credential-bearing endpoint refuses them — but a
  provider download link carries `token=<api key>` in the URL, and resolving
  those links is exactly what the member tier exists to allow. Confirmed the
  token is load-bearing rather than incidental: stripping it gets a flat
  `400 missing field 'token'` from TorBox's CDN, so AcerviNode cannot remove
  it while still handing back a working link. Now stated plainly in the API
  docs, the roles section and the README, since it changes what creating a
  member account means.

  Revisited afterwards to check whether it could simply be closed. Two facts
  measured against the CDN: it rejects `Authorization: Bearer` (`400`), so
  `?token=` is the only auth and no URL rewriting avoids carrying the key;
  and it fully supports Range (`206`, `accept-ranges: bytes`, correct
  `content-range` on a ~1GB file), so AcerviNode *could* proxy the transfer
  with seeking and resume intact. Proxying is therefore viable rather than
  impossible — declined on cost, since it would put AcerviNode in the middle
  of every byte and give up the direct CDN connection. Recorded in the docs
  as a deliberate trade-off so it can be revisited, rather than as a
  limitation.

- **A refresh pass could flag another provider's downloads as vanished.**
  The importer listed tracked rows by *kind* alone, so polling AllDebrid
  handed it every TorBox torrent row too — absent from AllDebrid's listing by
  construction, and flagged `no longer found in the provider's account`
  within three ticks. Healthy downloads, on an account that was never asked
  about them. Both compat shims already grouped by provider; the importer's
  bulk pass was the one caller that didn't.

  The mass-vanish guard masked this whenever the wrongly-missing fraction
  exceeded its 50% threshold, which is how it survived the multi-provider
  work — it only bites *below* that line, e.g. five AllDebrid rows against
  two TorBox rows, where 28% sails under the guard and the TorBox rows get
  flagged. Found by probing the real two-provider instance while
  investigating the freeze above, not by inspection.

  Fixed in two places deliberately: `ListDownloadsByProvider` scopes what the
  importer reads, and `RefreshFromProvider` skips rows whose provider doesn't
  match `RefreshOptions.Provider` regardless — the failure mode is flagging
  live downloads as gone, so a future caller forgetting to scope should be
  harmless rather than destructive. Identity is now consistently the
  `(provider, provider_download_id)` pair, which also rules out two
  providers' id spaces colliding into a false match. The mass-vanish fraction
  is scoped the same way, so another provider's rows can't trip the guard on
  a provider that is answering perfectly.

- **The mass-vanish circuit breaker could never conclude, freezing downloads
  forever.** The breaker exists so a provider listing that comes back
  successful-but-empty can't flag every tracked download as gone. The gap:
  an account the user genuinely emptied produces byte-for-byte that same
  listing, and there was no time bound — so the breaker distrusted it
  indefinitely and the rows froze. Never progressing, never flagged missing,
  never cleaned up.

  Found live, not by inspection. A real instance whose TorBox account had
  been emptied from elsewhere (an entirely ordinary thing to do — the
  provider's own site, or a second AcerviNode on the same account) had three
  rows stuck indefinitely, two showing 0% for over eight hours, while the
  guard's warning fired **6,409 times in ten hours — 73% of every line in
  the log**, drowning the signal it existed to raise. Worth noting the same
  shape of freeze is already recorded in the guard's own code comment as
  having been fixed once before, via a different route in.

  `massVanishDecision` now tracks how long each `(provider, kind)` scope has
  been failing the guard and, after `massVanishMaxDuration` (30 minutes),
  hands the listing back to normal missing-detection. At the default
  10-second poll that's ~180 consecutive listings all agreeing the account is
  empty. Releasing isn't itself the destructive step:
  `missingDetectionThreshold` still requires three further consecutive misses
  per row, so a listing that recovers in the meantime costs nothing.

  Tracked per scope rather than globally — one provider's empty listing says
  nothing about another's, and a shared clock would let a healthy provider
  keep resetting a jammed one's. A healthy listing clears the history
  outright rather than pausing it, so the next anomaly gets its own full
  grace period instead of inheriting a stale clock.

  The warning is now throttled to one per scope per 5 minutes, with the
  hand-back announced exactly once on the transition — a warning repeating
  every tick forever is a quieter signal, not a louder one.

  New `listing_anomalous_since` per provider/kind on `GET /api/v1/status`.
  This is the one failure state nothing else revealed: lists succeeding, no
  rate limit, everything looking healthy, and nothing actually reconciling.

- **A fresh clone couldn't build.** `web/dist/.gitkeep` has been negated in
  `.gitignore` since the frontend landed, and both `development.md` and
  `installation.md` tell you a committed `.gitkeep` is what keeps `go build`
  working before `npm run build` has ever run. It was never actually tracked,
  so it didn't. Verified against a real clone: without it `web/dist` doesn't
  exist and `go build ./...` fails outright with `pattern all:dist: no
  matching files found`, because `go:embed` needs the directory to exist even
  when empty. Found while auditing the docs for claims that aren't true —
  this one was a repo bug wearing a documentation bug's clothes.

  `npm run build` then deletes it again on every frontend build (Vite empties
  `dist`), which is presumably how it went missing in the first place, so the
  build script now recreates it afterwards rather than leaving the invariant
  to be re-broken silently.

- **Second documentation pass: verified the claims, not just the coverage.**
  The first pass checked that things were *documented*; this one checked that
  what they say is *true*, by running the assertions against the code and the
  live instance.

  - **`max_concurrent_downloads` was documented as "clamped up to 1 rather
    than rejected".** It is rejected — `config.Validate` refuses anything
    below 1, and `UpdateGeneral` validates its candidate before saving.
    Confirmed live: `PUT` with `0` returns HTTP 400 and leaves the value
    alone. Corrected, and noted that `backup_interval_hours`/`backup_keep`
    are the only two settings with no range validation at all (a negative
    disables/retains, matching `0`).
  - **`GET /api/v1/settings/providers` returns a `type` field** the documented
    shape omitted — the name/type split is what the two-accounts-per-service
    UI runs on, so leaving it out of the response shape hid the mechanism.
  - **`/api/v1/health`, `/api/v1/version` and `/api/v1/providers` had prose
    descriptions but no response shapes.** All three now show their actual
    JSON, including that a non-release build reports `0.0.0-dev`.
  - Stale code comments corrected where they were provably false: the
    `api.Settings` interface was still described as "narrowly scoped to
    TorBox, the only provider that exists today" directly above a fully
    generic, name-taking interface, and `Settings.tsx` still deferred a
    decision to "if Phase 4 (multi-provider) ever unblocks". It shipped.
  - One web-UI tab reference was wrong (`Settings → Providers`; the tab is
    `Provider`).

  Verified accurate and left alone, so the next audit can skip them: all 18
  config defaults, every `Validate` rule, the tombstone grace periods (5
  minutes confirmed / 30 days unconfirmed), session TTL and cookie
  attributes, PBKDF2 parameters (SHA-256, 600,000 iterations), the five
  download states, every qBittorrent route and SABnzbd mode, the release
  tarball layout the install steps depend on, the exact set of
  unauthenticated endpoints, and the `ListCache` design notes.

- **Documentation audit: filled in what shipped undocumented, corrected what
  had gone stale.** The multi-provider refactor, AllDebrid and backups all
  landed without the docs catching up. Checked mechanically against the code
  rather than by reading — config keys diffed against the `Config` struct,
  documented routes against the registered ones, body fields against
  `GeneralUpdate` — plus a link checker over every internal link and anchor.

  Wrong in a way that would have cost someone real time:

  - `PUT /api/v1/settings/general`'s documented body was missing **8 of its 24
    fields**, in a request the same sentence describes as a full replacement.
    Following the docs would have silently zeroed the cleanup, filter and
    (once added) backup settings. The list is now complete and spells out that
    an omitted field overwrites rather than being ignored.
  - `GET /api/v1/setup/status` was documented — and commented in `auth.go` —
    as also requiring no provider configured. `SetupNeeded()` checks only for a
    login account, deliberately: an instance with a key but no account is still
    a fresh install, and the code says so. Doc and comment corrected to match.
  - The README's provider table said AllDebrid had no Web Downloads support,
    and prose claimed its hoster debriding "doesn't fit" the model. It shipped
    working; the table and prose were left over from the earlier assessment.
  - `docs/index.md` still described the local mount/import step as a later
    phase needing Linux-specific filesystem tricks. It shipped as plain HTTP
    with no FUSE, which `development.md` had been contradicting for a while.
  - `quickstart.md` told you to enter your API key in the web UI. Login has
    been mandatory since Phase 9 — there is no API-key-only way in — and a
    fresh instance shows a setup wizard. It also still called multi-provider
    support a blocked future phase.
  - `providers.md` claimed the settings API was deliberately TorBox-shaped
    "since TorBox is the only provider that exists", named two interfaces where
    there are five, and its add-a-provider guide predated the registry.

  Missing entirely: AllDebrid had **zero** mentions anywhere under `docs/`.
  Added a full provider section (endpoints, the four things its own
  documentation gets wrong, how its shapes differ from TorBox), a
  `debrid.Registry` section covering per-kind routing, two accounts per
  service and per-`(provider, kind)` backoff, config rows for its credentials,
  and a note in the SABnzbd doc that the shim needs a usenet-capable provider.
  Also added the 6 endpoints that were never in the API table, `/api/v1/status`'s
  per-provider array, `default_categories_seeded`, and the one exception to the
  admin-only rule (any account may change its own password).

  One broken anchor fixed (`providers.md#status-monitoring`, which gained an
  endpoint suffix its links never followed). Internal links and anchors now
  verify clean.

- **AllDebrid sheds load with 503, not 429**, so a rate limit went
  unrecognised. Confirmed live: 80 concurrent requests produced no 429s at
  all and 21 nginx "503 Service Temporarily Unavailable" pages — AllDebrid's
  edge turns requests away before they reach the application, so the body is
  HTML with no error code to match on, and the previous handling reported it
  as a generic upstream failure that `internal/importer` did not back off
  for. A 503 now maps to the rate-limit sentinel. Deliberately narrower than
  "any 5xx": 500/502/504 read as a genuine fault rather than a deliberate
  "not now", and reporting those as a rate limit would put a misleading
  "rate-limited until" banner in front of a real outage.

- **An AllDebrid rate limit signalled by HTTP status was missed entirely.**
  Rate limiting was recognised only from the error code inside the response
  envelope, but AllDebrid caps requests at 12/second and 600/minute and
  answers **HTTP 429** when either is crossed — and a limiter sitting in
  front of the application returns that with no parseable body at all. A
  bare 429 therefore surfaced as `unexpected end of JSON input`, which
  unwraps to nothing, so `internal/importer` never backed off and kept
  hammering the very limit it had just hit. Now checked on status before the
  body is parsed, so the shape of the response can't affect whether the
  backoff happens. TorBox already worked this way; this brings AllDebrid in
  line.

  AllDebrid's quota errors (`FREE_TRIAL_LIMIT_REACHED`, `MUST_BE_PREMIUM`,
  `LINK_HOST_LIMIT_REACHED`) are deliberately *not* treated as rate limits:
  backing off would hide them behind a silent pause and a "rate-limited
  until" banner, when they are things only the account holder can resolve
  and are far more useful as the error text on the download itself.
  `MAGNET_TOO_MANY_ACTIVE` stays a rate limit by contrast, because it is a
  concurrency cap that clears on its own as transfers finish.

- **Account status is now reported per provider, inside that provider's own
  card.** It was a single instance-wide panel showing whichever provider was
  the default, with the cooldown warning hardcoded to say "TorBox" — so with
  two providers configured, one account's plan, expiry and restrictions were
  displayed under a heading that could have meant either, and the other's
  weren't shown at all. Each account has its own plan and its own
  restrictions; none of it generalises. New endpoint
  `GET /api/v1/settings/providers/{name}/account`; the existing
  `GET /api/v1/settings/account` stays, reporting the default provider, for
  anything already pointed at it. The warning now names the provider that
  actually applied the restriction. `Account` is also available on all three
  `Dynamic*Provider` wrappers rather than only the torrent one, since an
  account belongs to a provider rather than to a kind — a provider that
  doesn't do torrents still has a plan worth reporting. The per-provider
  fetches settle together, so one unreachable provider doesn't delay
  another's panel.

- **The Provider settings tab had grown an extra layer.** Generalising the
  single TorBox panel into a per-provider list wrapped everything in one
  "Providers" card, so reaching a provider meant expanding that first and
  then finding it inside — where before, TorBox was simply there. Each
  provider is now its own card again, exactly the shape the TorBox one
  always had: name, a Default/Configured badge, and its own expander, with
  AllDebrid sitting beside it rather than nested under a heading. Account
  and polling status moved into their own card rather than being buried in
  the same collapsible. Display names are prettified (`torbox` → TorBox),
  but only for known services — an entry name that isn't recognised is shown
  as-is, since a second account on the same service is a distinct entry and
  guessing at its label would be worse than showing what it's actually
  called.

- **A torrent in an unfamiliar state was reported as failed.** TorBox's
  torrent state mapping treated anything it didn't recognise as an error,
  deliberately — the reasoning being that TorBox documents an explicit error
  state, and a stalled torrent is a dead end worth surfacing rather than
  waiting on forever. The reasoning was sound; the default was not. Any
  state TorBox reports that the list hasn't seen — a new one, a renamed one,
  or simply one nobody wrote down — became a failure, and a download that
  was progressing perfectly well was shown as broken.
  Found on two real downloads sitting in `error` for hours. Their actual
  state was `checking` — TorBox's own spelling for hash-checking, with
  `active: true` — which the list didn't have, since it only carried
  qBittorrent's `checkingDL`/`checkingUP`/`checkingResumeData` variants.
  Failure is now something the provider has to actually say, matched on
  substrings (`fail`, `error`, `invalid`, `missing`, `dead`, `stall`), and
  anything else unrecognised is queued. That is how `mapUsenetState` has
  always worked, so this brings torrents in line with usenet rather than
  inventing a new rule. A download that genuinely stalls is still caught,
  both by the explicit `stall` substring and by the stuck-download watchdog
  (`stuck_download_timeout_minutes`), which is a better tool for it than
  guessing from an unfamiliar status string. Both affected downloads
  recovered on their own on the first poll after deploying, with no retry
  needed.

- **AllDebrid's fast per-download poll never worked.** `magnets` in a
  `/v4.1/magnet/status` response is an array when listing but a bare
  *object* when the query is filtered to a single id, so decoding it as an
  array made every `Status` call fail. The failure was near-invisible:
  the bulk listing still worked, so downloads still progressed — just a
  whole poll interval slower than they should have, with a decode error in
  the log after every add. Both shapes are now accepted. Confirmed against
  the live API; not something AllDebrid's docs mention.

- **A torrent-only default provider broke usenet and web downloads
  entirely.** "Default provider" is one setting across every kind, but
  providers differ in what they support — so making AllDebrid the default
  resolved *usenet* adds to it too, and they failed with "no usenet-capable
  provider configured" while TorBox sat configured right beside it. Both
  compat shims and the native add endpoints were affected; from Sonarr's
  side it looked like usenet had simply stopped working. The default is now
  resolved per kind (`Registry.DefaultNameFor`): the configured default when
  it supports that kind, otherwise the first registered provider that does.
  Found live by setting AllDebrid as the default and watching SABnzbd break.

- **An explicit Managed add could silently stay Manual.** The native API's
  add endpoints returned an already-tracked download unchanged when the
  provider deduped to it, ignoring `added_via=arr` — answering "added, and
  it's Manual" to a request that said Managed, so it was never auto-fetched.
  The compat shims already claimed the row in that situation
  (`InsertOrClaimForArr`); the native path now delegates to the same logic,
  so both agree. Found live: re-adding an already-tracked magnet with
  `added_via=arr` returned `200` and left the row Manual.

- **The default provider could resolve to one with no credentials.** With
  more than one provider *type* known, every type is registered whether or
  not it has a key — the wrapper is the slot a key gets set into — so the
  registry's "first registered" fallback could land on an unconfigured
  provider while a configured one sat beside it, and every add would fail
  against it. The fallback now prefers a configured provider, and only falls
  back to registration order when nothing is configured at all.

- **A degraded provider could mark downloads as vanished while they were
  still on the account.** Two separate holes, both found during a real
  multi-day TorBox outage that flagged live downloads as "no longer found in
  the provider's account":
  - **Both compat shims could trigger the detection.** Their reactive
    refresh runs on every *arr poll — far more often than the importer's own
    pass, with no rate-limit backoff and no view of whether the provider has
    been answering reliably — so a listing that came back short there eroded
    `missing_count` quickly. That contradicted what the surrounding code
    already claimed: the bulk pass "owns deciding a download is genuinely
    gone". Detection is now opt-in (`database.RefreshOptions.DetectMissing`)
    and only the importer's bulk poll opts in.
  - **The mass-vanish guard only catches a *mass* vanish.** It trips when
    more than half of tracked rows disappear at once, so a provider shedding
    rows a few at a time stays under the threshold indefinitely — exactly
    what a degraded provider does. The bulk poll now additionally requires
    `trustedListStreak` consecutive successful listings from that provider
    before concluding anything, and any failure resets the streak. A
    sustained outage therefore never reaches the threshold and never flags
    anything, rather than quietly eroding toward it.

  The two guards are deliberately complementary: `missingDetectionThreshold`
  requires a row to be absent from several consecutive listings, and this
  requires those listings to have come from a provider that was actually
  answering reliably.

- **The rest of the per-download provider calls now route by provider too.**
  The first pass covered deletes and fetches, found by grepping the two
  named lookups — which missed every site that inlined the same switch
  instead of calling a helper. Six more were doing exactly that: the native
  API's file list, file link and zip link (all three take a specific
  download and resolve its provider from its kind alone), plus the
  qBittorrent shim's file list and delete and the SABnzbd shim's delete.
  All are the same bug class: a `provider_download_id` handed to a provider
  that never issued it. The three API paths now share one `providerFor`
  lookup, which collapsed three near-identical switches into a single
  resolution, and the shims — which hold exactly one provider, so have
  nothing to look up — guard on `ownsDownload` instead. The add paths are
  deliberately untouched: a brand-new download has no provider yet, so
  choosing one is a separate question.

- **Provider calls are now routed by the download's own provider, not just
  its kind.** Every row already records which provider it came from
  (`downloads.provider`), and the API reported it — but nothing used it to
  *route a call*. Deletes and fetches resolved purely by kind, which is
  identical behaviour while one provider is configured and wrong the moment
  two are: a `provider_download_id` means nothing to a different account, so
  the call would at best fail and at worst act on an unrelated download that
  happened to share the id. `deleterFor` (`internal/api`) and `providerFor`
  (`internal/importer`) now resolve by provider *and* kind, skipping the
  call and logging when a row belongs to something else; the local row is
  still removed, since the provider-side delete has always been
  best-effort. The bulk polling paths keep a kind-only lookup, which is
  correct — they are inherently per-kind, not per-download. This also fixes
  a real single-provider case: a download added under one API key, still
  tracked after the key was swapped for a different account, would
  previously have had its delete or fetch aimed at the new account using an
  id it has never heard of. Groundwork for multi-provider support, and
  correct on its own merits today.

- **A download deleted while the provider was unreachable came back as a
  ghost.** The delete tombstone that stops `discoverManual` re-adopting a
  just-deleted item lived for five minutes, on the reasoning that a
  `provider_download_id` that's genuinely gone never legitimately reappears.
  That quietly assumed the provider-side delete *succeeded*. It's
  best-effort by design — a provider outage or rate limit must never leave a
  row the user can't remove — so when it fails the item really is still on
  the account, and the short window doesn't prevent the ghost, it just
  delays it. A failed delete now records a 30-day tombstone
  (`unconfirmedDeleteGracePeriod`), deliberately longer than TorBox's own
  retention so an orphan ages off the account before the tombstone lapses;
  each tombstone carries its own `expires_at` rather than sharing one global
  grace period. All four delete paths (native API, both compat shims, and
  the importer's retention cleanup) pass through whether the provider
  confirmed. Found during a live burn-in, not by inspection: two downloads
  were deleted while the account happened to be rate-limited, both provider
  deletes returned `429`, and both reappeared as Manual downloads once the
  five minutes were up — including one that had been Managed, which came
  back in the wrong tab.

- **Frozen downloads now say why, instead of just looking broken.** While a
  kind is in provider rate-limit cooldown, `internal/importer`'s
  `refreshKind` skips that kind's listing entirely, so every download of
  that kind stops advancing — progress, state and speed all freeze at
  whatever was last polled. Nothing in the downloads view said so, which
  makes it indistinguishable from AcerviNode being broken, and is exactly
  how it gets reported ("state seems to get stuck and is laggy"). The
  cooldown was already exposed on `GET /api/v1/status` and already surfaced
  on the Settings page for the *provider account's* own `cooldown_until` —
  but a user watching stalled downloads has no reason to go to Settings.
  Both download tabs now poll `/api/v1/status` alongside the downloads
  themselves (a purely local read, no provider call) and show a banner
  naming the time polling resumes. Found during a live burn-in: a sustained
  concurrent poll load tripped TorBox's real rate limiter, all three kinds
  backed off together — the limit is per API key across its servers — and
  the UI gave no indication whatsoever.

- **Every *arr poll triggered its own full provider listing, and all that
  redundant work serialized against everything else.** Three reactive
  refreshes — the qBittorrent shim's `/torrents/info`, and SABnzbd's
  `mode=queue` and `mode=history` — each made an unconditional
  `provider.List()` call inside the HTTP handler, on every single request,
  with no reuse or coalescing of any kind. In a real setup that multiplies
  fast: Sonarr, Radarr, Readarr and Lidarr each poll on their own schedule
  and a single *arr app hits more than one of those endpoints per cycle, so
  the same full account listing was fetched from the provider many times a
  minute — each response then written back through
  `database.RefreshFromProvider`. Because the database deliberately runs on
  a single connection (`SetMaxOpenConns(1)`), that write traffic serializes
  against every other query, including the web UI's own polling. The result
  is app-wide sluggishness and state that looks stuck, for usenet and
  torrents alike, rather than merely slow compat shims — which is exactly
  how it was reported. Those three paths now share `debrid.ListCache`,
  which reuses a listing for a couple of seconds and makes concurrent
  callers share one in-flight call instead of each starting another. A
  reused response deliberately keeps its *original* fetch timestamp, so
  `RefreshFromProvider`'s ordering guard still treats it as the older data
  it genuinely is and it can't overwrite fresher state. Failed fetches are
  never cached. `internal/importer`'s own polling is deliberately left
  alone: it's already interval-driven and deduplicated by construction, and
  the fast per-download poll depends on fetching fresh data on demand — this
  only removes redundancy that had no design intent behind it.

- **The vanished-download circuit breaker could jam on permanently,
  disabling missing-download detection and logging a warning every tick.**
  `isSuspectedMassVanish` counted *every* Manual row toward its
  missing-fraction, including rows already in `error` —
  but `handleMissingFromProvider`, the only thing that guard protects,
  skips an already-errored row entirely. That asymmetry made the guard
  unrecoverable: a row flagged `error` is absent from every future listing
  forever (it's genuinely gone, or it belongs to a provider account that's
  since been swapped out), so once enough rows had errored, the fraction
  could never fall back below the threshold. Missing-detection stayed off
  for that kind for good, and the "suspected mass-vanish" warning fired on
  every single poll. Observed on a real instance: 335 identical warnings,
  `tracked_rows=4 statuses_returned=1`, where three of those four were
  long-dead rows left behind by an API-key rotation. Already-errored rows
  are now excluded from both halves of the fraction, matching exactly what
  `handleMissingFromProvider` acts on; a genuine mass-vanish among healthy
  rows still trips it.

- **A deleted download leaked its cached live status forever.**
  `database.DB.refreshState` — the map backing both the refresh-ordering
  guard and the `LiveStatus` cache — is keyed by download id, and nothing
  ever removed from it. Every download ever deleted left its entry behind
  for the lifetime of the process, growing without bound on a long-running
  instance with ordinary add/remove churn. `DeleteDownload` now forgets it.

- **A stock systemd install could never start.** `packaging/acervinode.service`
  set `ProtectSystem=strict` with `ReadWritePaths=/var/lib/acervinode` only,
  on the stated assumption that AcerviNode "only needs to read its config
  under /etc/acervinode". It doesn't: every live settings change is persisted
  by rewriting `config.yaml` in place, and one such write happens during an
  ordinary startup, before the server is even listening, when the one-time
  default-category seeding runs. With `/etc/acervinode` read-only that write
  fails, the process exits 1 having served nothing, and `Restart=always`
  turns it into a permanent restart loop — following
  [Installation](docs/installation.md)'s own steps exactly produced a service
  that never came up. Found by doing precisely that on a clean machine.
  `/etc/acervinode` is now in `ReadWritePaths`. Two documentation gaps in the
  same steps are fixed alongside it: `install` doesn't create parent
  directories, so `/opt/acervinode` has to be in the `mkdir -p` (the
  documented order had `install` writing into it first), and the newly-created
  `config.yaml` has to be `chown`ed to the service account for the same reason
  the unit change is needed — an unwritable config is the restart loop again,
  by a different route. An existing install needs the unit re-copied and
  `systemctl daemon-reload`; a binary update alone never replaces it.

- **A download an *arr app asked for could end up in the Manual tab,
  never auto-fetched.** Reported as "sometimes managed move to manual."
  `added_via` is only ever written at insert and nothing updates it, so no
  row was actually changing tabs — instead the *arr add was *losing a race
  for the row*, and the Manual copy was the one that survived. Both compat
  shims persisted an add with a plain `InsertDownload`, which trips the
  `(provider, provider_download_id)` UNIQUE constraint whenever a row for
  that provider id already exists. Two real ways it does:
  - **The importer's discovery pass adopted the item first.** `refreshKind`
    read its snapshot of tracked rows *before* the provider `List()` call —
    a network round-trip that can take seconds — so an *arr add landing in
    that window was missing from the snapshot, and `discoverManual` saw the
    provider's copy of that brand-new Managed download as untracked and
    adopted it as Manual. Whichever insert lost then failed: either the
    adoption (harmless, logged) or the shim's own (the *arr add fails
    outright, and the surviving row is Manual). This is the intermittent
    one — it needs an add to overlap a discovery tick, which is exactly why
    it only happened "sometimes."
  - **TorBox deduped by content.** It hands back the `torrent_id` it
    already has for a hash, so re-adding through *arr something already
    tracked as Manual (added via "+ Add", or discovered earlier) collided
    with that existing row every time, not just under a race.

  Fixed on both sides. `refreshKind` now reads its tracked-rows snapshot
  *after* `List()` returns, which narrows the window to an add landing
  mid-call; and both shims now go through `InsertOrClaimForArr`, which
  *claims* an existing row instead of colliding with it — promoting it to
  `added_via = arr` and stamping the category/save_path the *arr app asked
  for, on the principle that an explicit *arr request outranks a passive
  discovery. The promotion is deliberately one-way: nothing ever demotes a
  Managed row, so the two can't oscillate. Category/save_path are only
  overwritten when non-empty (an empty `save_path` silently breaks *arr's
  import step), and a missing `hash` is filled in but an existing one never
  replaced, since the qBittorrent shim is keyed on infohash. SABnzbd's
  `nzo_id` is an AcerviNode row id, so a claimed add now correctly reports
  the *existing* row's id rather than a freshly generated one pointing at a
  row that doesn't exist. Regression tests drive both shims end to end and
  fail against the old code with the exact user-visible symptom — a
  `400 Fails.` from the qBittorrent add and a `status:false` from
  SABnzbd's.

- **A usenet download's ETA/speed could stay frozen at a stale value
  (e.g. "1s") the entire time it showed "Processing".** Found live,
  immediately after the phase-badge fix below shipped: TorBox's own `eta`/
  `download_speed` fields don't necessarily reset to 0 the moment a usenet
  download finishes transferring and moves into a phase — AcerviNode was
  passing whatever it last reported straight through. Both describe the
  transfer itself, which is already over by the time any phase is active,
  so they're structurally meaningless there regardless of what the
  provider happens to still report — now suppressed in both the table and
  detail view whenever a phase is present, not just gated on state.

- **Neither the downloads table nor the detail view's state badge showed a
  usenet download's phase** (verifying/repairing/extracting/processing) —
  only a small, easy-to-miss inline annotation next to the progress percentage
  did, and the table didn't even have that. Found live, in two stages: a real
  Sonarr-driven usenet download stayed at a plain "Downloading" badge in the
  table the entire time TorBox was actually processing it, while Radarr's own
  queue view (via the SABnzbd shim, a separate code path) correctly showed
  "Processing" — then, after adding the same inline annotation the detail
  view already had, a second real download made it obvious the badge itself
  should say it too, not just a muted aside next to the progress bar.
  `PHASE_LABELS` moved from `DownloadDetail` into `format.ts`, and
  `StateBadge` now takes an optional `phase` prop: while `state` is
  `downloading`, it shows the phase label (e.g. "Processing") instead of a
  generic "Downloading", with no change to the underlying coarse state
  machine — `phase` stays a deliberately separate, non-persisted signal (see
  `debrid.DownloadStatus.Phase`'s own doc comment for why), this only changes
  what the badge displays. Verified live end to end: watched a real ~25GB
  usenet download's badge go from "Downloading" to "Processing" the moment
  TorBox finished the transfer and started post-processing, in both the
  table and the detail view.

- **A download whose very first observed status was already
  `provider_completed` (TorBox's common instant-cache case) never got a
  `cached_at` timestamp**, showing "Cached —" in the web UI forever despite
  sitting at 100% progress since the moment it was added. Found live via a
  screenshot of exactly that. `UpdateDownloadStatus` only sets `cached_at`
  on a state *transition*; a row born already `provider_completed` never
  transitions into it, so that path never fired. `InsertDownload` now
  stamps it at insert time too, and a new migration (`0010`) backfills
  every existing affected row from its `added_at`. See
  docs/api.md#download-json-shape.

- **App-wide hanging/stuttering, investigated directly rather than from a
  specific repro — three compounding causes found and fixed:**
  - `internal/database` opened SQLite with its own defaults (a rollback
    journal, `synchronous=FULL`) — a full fsync of the whole database file
    on every single write. Combined with the single connection every
    operation already serializes through (`SetMaxOpenConns(1)`, by design —
    see docs/providers.md), how long any one write took was directly how
    long *everything else* queued behind it — including the web UI's own
    list poll — had to wait. Now `journal_mode=WAL` +
    `synchronous=NORMAL`, which doesn't change the single-connection design
    at all, just makes each individual write meaningfully faster (measured
    live: ~0.2ms/write → ~0.06ms/write, single `sqlite3` process, 200
    writes against a real copy of this project's own database). See
    docs/providers.md#wal-mode-and-why-every-writes-speed-matters-here-specifically.
  - The web UI's own list poll and the download detail view's poll both
    used `setInterval`, which fires on a fixed cadence regardless of
    whether the previous request finished. The detail view's own endpoint
    can block for up to `provider_request_timeout_seconds` (30s) when the
    provider is slow — with `setInterval`, a single slow poll didn't just
    run late, several more piled up behind it every 4 seconds, each its own
    live provider call. Both now use a self-rescheduling `setTimeout`
    instead, so a slow poll is at worst late, never compounding. See
    docs/providers.md#polling-loops-wait-for-the-previous-request-not-a-fixed-clock-tick.
  - Every streamed download (both the per-row/detail-view path and the
    Downloads popup window) updated React state — and, for the popup, also
    posted a cross-window message — on *every single chunk* received, with
    no throttling at all. On a fast connection this could fire hundreds of
    times a second for the whole length of a large download, each one
    triggering a full re-render (of the downloads table too, for the
    per-row path). Throttled to at most 5 updates/second, with an
    unconditional final flush per file so displayed progress is never stale
    at a file boundary.

- **Demoting your own admin account kept full admin access through your
  already-open browser session, indefinitely.** Found by code inspection
  while auditing for other bugs: a session's role is cached at login and
  never re-derived from config on later requests, so `handleSetUserRole`
  must explicitly revoke an account's existing sessions when its role
  changes — but it excepted the *caller's own* session token from that
  revocation, the same way `handleSetUserPassword`/`handleMakeDefaultUser`
  correctly do (where nothing about the caller's own session actually goes
  stale). For `handleSetUserRole` specifically, that meant an admin
  demoting their own non-Default account kept exactly the access the
  demotion was supposed to remove, contradicting the handler's own comment
  ("end them so a demoted member can't keep using an admin session it
  already holds"). Fixed: revokes the target account's sessions
  unconditionally, including the caller's own if they targeted themselves —
  the web UI already handles the resulting `401` gracefully (bounces to the
  login screen within one background poll tick), no frontend change needed.
  No handler-level test had covered this endpoint at all before now. See
  docs/providers.md#auth-login-accounts-and-roles.

- **The download detail modal showed nothing but a bare header while
  loading, indistinguishable from a hung/broken modal.** `GET
  /api/v1/downloads/{id}` blocks server-side on a live provider file-list
  call before responding — normally fast, but it can take up to
  `provider_request_timeout_seconds` when the provider itself is slow.
  Noticed live: right after a server restart, TorBox's own `mylist` API was
  intermittently timing out (confirmed via the log, not a rate limit — no
  `429`s anywhere), and clicking a download during that window showed an
  empty box for up to 30 seconds. Fixed: a "Loading…" placeholder now shows
  while the request is in flight.

- **Deleting a download while `internal/importer` was still fetching it to
  disk had no way to interrupt that fetch.** Identified by code inspection
  while reviewing the Add-to-Managed feature above: the fetch goroutine held
  its own snapshot of the download row for the whole transfer with no
  awareness of a concurrent delete, so it could keep writing after the row
  (and, with `deleteFiles=true`, the local files `RemoveLocalFiles` had just
  removed) was already gone — recreating exactly what the delete was
  supposed to clean up. Fixed: a new `Importer.CancelFetch(id)`, tracked via
  a per-download registry (`Importer.activeFetches`), interrupts the
  in-flight fetch and blocks until it has genuinely stopped before
  `handleDeleteDownload` touches anything else. The same registry closes a
  related hazard as a side effect: a fetch outliving one
  `import_interval_seconds` tick (a large multi-file torrent) could
  otherwise get picked up a second time by the next tick, running two
  concurrent fetches into the same directory. Live-verified: deleted a
  2.4GB Managed torrent partway through its fetch with `deleteFiles=true` —
  no orphaned files remained and the row disappeared from the API
  immediately. See docs/providers.md#canceling-an-in-flight-fetch-on-delete.

- **The nav bar's "+ Add"/"+ Add to Manual" button stayed visible on the
  Settings tab**, where opening its modal made no sense (there's no download
  list to add into). Now hidden whenever the Settings tab is active.

- **The nav bar's "+ Add" button relabeled "+ Add to Manual"** (and its
  modal's heading to match) — it's always visible regardless of which tab
  is active and always creates a Manual download, never a Managed one
  (those only ever come from Sonarr/Radarr via the compat shims), which
  the generic "+ Add" label didn't make clear, especially confusing while
  looking at the Managed tab.

- **`import_fetch_timeout_seconds` is now an idle/stall deadline, not a
  total-transfer one.** Previously it was a single deadline covering an
  entire file's fetch from start to finish — a large file on a slow
  connection that was genuinely, continuously transferring (never stalled)
  could still get killed and retried just for taking too long in absolute
  terms, forcing the value to be raised arbitrarily high to accommodate the
  largest expected file rather than actually catching a hung connection.
  Fixed: `internal/importer` now resets the deadline every time bytes are
  actually received (covering the connect-and-wait-for-headers phase too,
  not just the body) — a transfer that's slow but steadily active is never
  affected by this however long the whole download takes; only a
  connection that's actually gone quiet trips it. Same config field, same
  default (600s) — pure behavior fix, nothing to migrate. See
  docs/configuration.md and the Settings UI's relabeled "Import fetch idle
  timeout" field.

- **Every browser-side download path had no idle/stall protection at
  all** — the same class of bug as the backend fix above, just on the
  frontend. A stalled provider CDN link left a progress bar (or, for a
  single-file download, the browser itself) frozen forever with no error
  and no way to notice short of watching it stop moving; the Downloads
  popup window's own manual Stop button was the only recourse for that one
  path, and neither the in-tab streaming path nor the plain per-file
  "Download" button (and its Firefox/Safari individual-files fallback) had
  even that. Fixed the same way across all three: `fsAccess.ts`'s
  `writeFileToDirectory` (the streamed-to-folder paths) resets a
  600s-default idle deadline on every chunk actually written, a new
  `readBlobWithIdleTimeout` gives `forceDownload` (the plain-download path,
  previously a bare `await response.blob()` with zero protection) the same
  per-chunk protection, and a new `fetchWithIdleTimeout` covers the
  connect-and-wait-for-headers phase before either of those for all three
  paths. A transfer that's slow but genuinely still receiving data is
  unaffected either way.

- **Every NZB-sourced Managed import failed outright in Radarr/Sonarr** with
  "Access to the path ... is denied," even though the download showed
  Completed. Root cause, diagnosed live from a real Radarr log: AcerviNode's
  own process (a dedicated systemd user, not root, and not necessarily the
  same user/group an *arr app actually runs as — e.g. a separate Docker
  container with its own PUID/PGID) created every download directory as
  `0755` — writable only by that one user. Real SABnzbd's completed-item
  reporting always tells Radarr/Sonarr it's safe to move a file
  (`CanMoveFiles` is unconditionally true there, confirmed against their
  real source), so it always attempted a real move/hardlink out of that
  directory, which needs *write* access on whichever directory directly
  contains the file — not just read access to the file itself.
  **Torrents silently avoided the exact same wall** rather than actually
  working around it: Radarr only allows a move for a qBittorrent item when
  it's reported as paused after reaching its own configured seed limit, a
  state AcerviNode never reports (no real local seeding at all — TorBox
  handles that server-side), so every torrent import fell back to
  copy-only — needing only read access, but silently doubling disk usage
  per import in the process. Fixed: every directory AcerviNode creates for
  a download is now `0777` (world-writable), and an already-existing
  directory from before this fix gets corrected retroactively the next
  time anything is fetched into it — zero configuration needed on a fresh
  install. (Shipped as `0775`/group-writable first, requiring the user to
  add their *arr app's user to AcerviNode's group — corrected to `0777`
  after a live report showed that coordination doesn't hold up cleanly
  across every real deployment, e.g. Proxmox/NAS setups with LXC UID-
  namespace remapping. See docs/installation.md if you'd rather use the
  group-based approach instead — AcerviNode's own systemd `User=`/`Group=`
  is a plain override, matching your stack's PUID/PGID the same way
  linuxserver.io-style containers do.)

- **A completed torrent import always silently copied the file instead of
  hardlinking it — doubling disk usage on every single Managed torrent
  download — and "Remove completed downloads" never actually cleaned one
  up, even with that Sonarr/Radarr setting enabled.** The follow-up flagged
  (but not yet fixed) alongside the directory-permissions fix above.
  Confirmed against Sonarr/Radarr's real source: `CanMoveFiles`/
  `CanBeRemoved` only become true for a qBittorrent item reported as
  `pausedUP`/`stoppedUP` with its seed limit reached — AcerviNode was
  reporting a completed torrent as `uploading` (never actually true; it has
  no real local seeding at all — TorBox handles that server-side) and
  never reported `ratio`/`ratio_limit` at all. Fixed: `GET
  /api/v2/torrents/info` now reports `pausedUP` for a completed torrent
  (still `DownloadItemStatus.Completed` either way — this doesn't change
  whether Sonarr/Radarr consider it done) and `ratio`/`ratio_limit` as an
  explicit `0`/`0`, which unconditionally satisfies Sonarr/Radarr's own
  seed-limit check regardless of a user's configured seed-ratio settings.
  SABnzbd-sourced downloads already had both conditions satisfied
  unconditionally on Sonarr/Radarr's side — this closes the matching gap
  for torrents.

- **A brand new category typed into Radarr's SABnzbd download client got
  rejected outright by Radarr's own "Test" step.** Real SABnzbd (and this
  shim, faithfully) has no API to create a category on the fly — Sonarr/
  Radarr's `TestCategory()` (confirmed against their real source) explicitly
  requires a category to already exist server-side before accepting it, and
  AcerviNode's only category tracking was reactive (populated the moment
  something used it), which can't help since Test runs *before* anything is
  ever added — a real chicken-and-egg gap, not something a user could work
  around from Radarr's side at all. Fixed: the web UI's Settings →
  Categories section (`PUT /api/v1/settings/categories/path`) now registers
  a category with both compat shims as a side effect of saving it, even with
  no path override (now optional) — the AcerviNode-side equivalent of
  pre-creating the category in real SABnzbd's own admin UI first. Every
  registration is also now persisted and re-seeded into both shims on
  startup (previously only a path override survived a restart at all, and
  even that never made it back into the shims' own category lists — a bare
  registration would have silently reverted on the very next restart). See
  docs/sabnzbd-api.md#categories.
- **SABnzbd `mode=history` was missing `bytes` (size)**, a field Sonarr's
  real source (`SabnzbdHistoryItem`/`Sabnzbd.cs`'s `GetHistory`) reads
  directly into a download's reported size — every completed/failed item
  showed size `0` in Sonarr/Radarr's Activity view. Found during a full
  qBittorrent/SABnzbd API-parity audit against both real API docs and
  Sonarr/Radarr's actual download-client source.

### Added

- **The "+ Add" form now shows whether a magnet/URL/link is already cached
  on the provider before you commit to adding it, and previews a torrent's
  real name/size/file list/seeders straight from the BitTorrent network.**
  `CheckCached` existed at the client layer from early on but was never
  actually wired up anywhere — found by asking directly what the official
  TorBox API has that AcerviNode doesn't. Closed for all three kinds
  (`GET /api/v1/downloads/{torrent,usenet,webdl}/check-cached`), plus a new
  `GET /api/v1/downloads/torrent/info` metadata preview
  (`debrid.TorrentInfoProvider`, an optional capability the same shape as
  `AccountProvider`). Found live along the way: TorBox's own docs describe
  the `checkcached` hash parameter as comma-separated, but a comma-joined
  value consistently timed out against the real API — repeated `hash=`
  params (what the existing, never-actually-exercised torrent
  implementation already happened to send) is what genuinely works,
  confirmed against two real hashes at once. Live-verified end to end: a
  known-cached test torrent correctly reported `cached: true` with its real
  name/size/seed-peer counts/6-file list, and a fabricated hash correctly
  reported both `cached: false` and a routine `available: false` preview.
  See docs/api.md#cached--metadata-previews.

- **An admin can now add a download straight into the Managed pipeline**,
  not just Manual — requested directly after relabeling the "+ Add" button
  made the Manual-only behavior obvious enough to notice as a real gap. The
  "+ Add" modal gains a Managed/Manual toggle, visible only to admins
  (members have no Managed access at all, so they never see it — enforced
  server-side too, not just hidden in the UI: a member requesting it via
  the API directly gets `403`), defaulting to whichever tab the button was
  opened from. Picking Managed reveals a category field and, once
  submitted, the download behaves exactly like a real Sonarr/Radarr add —
  auto-fetched to `download_dir` (or the category's own override) by
  `internal/importer`, showing up in the Managed tab from then on. New
  optional `added_via` field (`"arr"` or `"manual"`, default `"manual"`) on
  all three add endpoints — see docs/api.md#adding-downloads-directly.
  Live-verified against the real account: added a real torrent with
  `added_via=arr` and a category, watched it land in a freshly-created
  category folder under `download_dir`, and confirmed the file actually
  wrote to disk.

- **New `provider_request_timeout_seconds` setting, plus two more
  Provider-tab additions.** Three related follow-ups from the same TorBox
  outage that motivated the idle-timeout fix above:
  - The debrid provider's own API calls (list, status, add, delete,
    account) were bounded by a hardcoded 30s `http.Client.Timeout` in
    `internal/debrid/torbox`, with no way to change it at all. Now a real,
    live-editable setting (`provider_request_timeout_seconds`, default 30s)
    — unlike the fetch timeout above, this one stays a plain
    **total-request** deadline on purpose: a provider API response is a
    small JSON payload, not a multi-gigabyte file, so there's no
    legitimate "slow but actively trickling" case to protect against here.
    Changing it rebuilds the provider from the current key with the new
    timeout baked in, the same as changing the key itself already does —
    no restart needed.
  - The Provider tab now shows a **Status** panel — `GET /api/v1/status`'s
    data (background-polling liveness, each kind's last successful sync,
    any active rate-limit cooldown, error counts), previously only
    reachable via `curl`.
  - The account panel shows **"Checking account status…"** while that
    (separately-fetched, sometimes slow — see the fix above) call is still
    in flight, instead of just silently showing nothing until it resolves
    or forever if it never does.

- **`import_interval_seconds`/`fast_poll_interval_seconds` moved from
  Settings → General to Settings → Provider**, alongside the TorBox account
  status and connection test — both are about how often AcerviNode polls
  the configured debrid provider, not the instance itself, and now sit next
  to the cooldown/rate-limit information they're most relevant to. Same
  fields, same `PUT /api/v1/settings/general` endpoint — a pure UI
  reorganization, nothing to migrate.

- **Every category is now editable and deletable, including the pre-seeded
  *arr-app defaults.** Previously, `defaultArrCategories` (Radarr's
  "movies"/"radarr", Sonarr's "tv"/"tv-sonarr", Lidarr's "music"/"lidarr",
  Readarr's "Readarr"/"readarr") were force-registered directly into both
  compat shims on *every* startup, bypassing `config.yaml` entirely — a
  default a user deleted would always silently reappear on the next
  restart, making deletion pointless. Fixed: this seeding now runs exactly
  once, ever (a new `default_categories_seeded` flag), folding each default
  into `category_paths` exactly as if a user had registered it by hand —
  from that point on it's indistinguishable from any other category. New
  `DELETE /api/v1/settings/categories/{category}` forgets a category
  entirely (its path override and its registration with both shims); if an
  *arr app is still actively configured with it, it simply gets
  re-registered next time it's declared again, same as a real
  qBittorrent/SABnzbd install. The Settings → Categories page now shows
  every known category (defaults, reactively-discovered, and manually
  registered alike) as a full row with an editable path and a delete (✕)
  button, instead of only showing categories that already had a path
  override — a purely reactive or pre-seeded one used to be invisible there
  except as plain text in a "currently known" summary line. See
  [SABnzbd API](docs/sabnzbd-api.md#categories).

- **Bulk select/delete/retry in the downloads table.** A checkbox column
  (row + header select-all/indeterminate) and a contextual action bar that
  only appears once something's selected — the table looks identical to
  before this existed the rest of the time. Delete is available on both
  tabs; Retry only on Managed (matching the existing per-row Retry button's
  own Managed-only scoping — a Manual download has nothing for Retry to
  act on) and only counts/acts on the selected rows actually in `error`
  state. Both loop the existing single-item endpoint rather than needing a
  new batch API, same precedent as "Download all" looping the per-file link
  call. Selection is scoped to whichever tab is currently visible (cleared
  on tab switch) and automatically drops any id that stops existing (e.g.
  deleted through its own row's ✕ while still checked).

- **New `GET /api/v1/status` endpoint** reports whether AcerviNode's own
  background polling is actually alive and making progress — built
  proactively, picked directly off the roadmap's own "Path to daily-driver
  parity" list, since the only way to know the tick loop was stuck before
  this was manually tailing the log. Reports `last_tick_at` (proves the tick
  loop itself hasn't stalled or crashed, regardless of what any single
  provider kind found), plus per-kind (`torrent`/`usenet`/`webdl`)
  `last_successful_list_at`, `rate_limited_until`, and `error_count`.
  Deliberately doesn't fold in TorBox's own `cooldown_until` even though
  both were motivated by the same incident: a listing call that succeeds
  but finds nothing new still advances `last_successful_list_at`, so the
  two answer genuinely different questions ("is polling itself working" vs.
  "is the provider account restricted") — see
  docs/providers.md#status-monitoring-get-apiv1status. Meant for an external
  monitor (Uptime Kuma, Healthchecks.io, ...) to poll and alert on;
  authenticated the same as `/version`/`/providers`, not admin-only and not
  fully open like `/health`.

- **`download_dir_mode` and `fast_poll_interval_seconds` are now real,
  live-configurable settings** (`config.yaml`/env var/`PUT
  /api/v1/settings/general`/the web UI's General tab), not fixed
  behavior. `download_dir_mode` (default `"0777"`, matching the fix above)
  lets you tighten download directory permissions back down if `0777`
  doesn't fit your setup — e.g. AcerviNode's own systemd `User=`/`Group=`
  already matches your *arr stack. `fast_poll_interval_seconds` (default
  `3`, unchanged from its previous hardcoded value — tuned live against a
  real provider to stay responsive without risking a rate limit) exposes
  `internal/importer`'s active-download poll cadence, previously a fixed
  constant, for a user who wants to widen it themselves (e.g. routinely
  many downloads active at once). Both apply immediately, no restart
  needed.

- **The Settings page's TorBox account panel now surfaces `cooldown_until`
  when TorBox sets it** — a real, undocumented field on TorBox's own `GET
  /user/me` found live while investigating a report that every download's
  progress had frozen for hours. Root cause: not a regression in
  AcerviNode (confirmed by replicating its exact outbound request by hand
  against TorBox directly and reproducing the same empty result), but a
  TorBox-side account restriction — while `cooldown_until` was set to a
  future time, every one of TorBox's own listing endpoints silently
  returned zero items instead of erroring, which AcerviNode's existing
  mass-vanish safeguard correctly avoided misreading as "everything
  disappeared," but with no visible explanation for why nothing was
  updating. AcerviNode doesn't change any polling behavior based on this
  field — it's display-only — but a warning banner now appears on the
  Settings page whenever it's active, so this is diagnosable at a glance
  next time instead of requiring a manual log/API investigation. See
  docs/providers.md#cooldown_until--a-real-undocumented-account-restriction
  for the full writeup, including the caveat that TorBox doesn't document
  this field anywhere and the causal mechanism isn't independently
  confirmed on their end.

- **The downloads table's per-row speed now sits on its own line below the
  progress bar, with the ETA shown inline next to the percentage instead**
  (previously both speed and percentage were crammed onto one line, and
  ETA wasn't shown in the table at all, only the detail view) — easier to
  read at a glance across many active downloads.

- **A Managed download's progress bar now shows real progress while it's
  being fetched to local disk, instead of freezing at 100%.** Once a
  provider itself finishes a download, AcerviNode still has to actually
  copy the file(s) to local disk (Completed Download Handling) before an
  *arr app can import it — a real, sometimes lengthy step for a large file
  that previously had no visible progress at all, since the provider's own
  progress is already 1.0 by that point. `internal/importer` now tracks
  live bytes-written progress per download (throttled, never persisted —
  the same "fast-moving, ephemeral" treatment as ETA/speed) and it
  substitutes in for the reported progress everywhere: the web UI's
  downloads table and detail view (no frontend changes needed — same
  field, just a more accurate value during this phase), the native API,
  and both compat shims' own progress fields — Sonarr/Radarr's Activity
  view shows real fetch progress too, not just AcerviNode's own UI.
- **Every well-known \*arr-app default category is now pre-registered with
  both compat shims automatically, on every startup** — closing the
  remaining friction from the category fix above for the common case: a user
  who never customizes an \*arr app's own default category field hits zero
  setup friction at all, no visit to AcerviNode's Settings page needed.
  Confirmed directly against each app's real source
  (`SabnzbdSettings.cs`/`QBittorrentSettings.cs`), not guessed: Radarr
  defaults to `"movies"` (SABnzbd) / `"radarr"` (qBittorrent), Sonarr to
  `"tv"` / `"tv-sonarr"`, Lidarr to `"music"` / `"lidarr"`, Readarr to
  `"Readarr"` / `"readarr"` — Readarr's SABnzbd default really is
  capitalized (category comparisons are case-sensitive). A fully custom
  category name still needs the one-time manual registration — see
  docs/sabnzbd-api.md#categories.
- **Four more real qBittorrent endpoints Sonarr/Radarr call under specific
  optional client settings**: `POST /api/v2/torrents/setCategory` (a
  separate "post-import category," different from the add-time one),
  `setShareLimits` (per-release seed ratio/time criteria), `topPrio`
  ("Recent/Older Priority" set to "First"), and `setForceStart` ("Initial
  State" set to "Force Start") — confirmed against Sonarr/Radarr's real
  source that none of these are called on a default setup, but a user who
  enables one would have hit a 404 before this. `setCategory` actually
  updates the tracked download's category (and auto-registers the name,
  matching this shim's existing permissive philosophy rather than real
  qBittorrent's stricter "category must already exist" 409); the other
  three are accepted as no-ops — AcerviNode has no seeding, priority-queue,
  or paused-state concept for them to apply to.

- **A usenet download that genuinely failed (e.g. TorBox's own "failed
  (Repair failed, not enough repair blocks (165 short))") could still show
  as "Repairing" in the web UI.** The user supplied a real NZB specifically
  to test error surfacing, live-confirming the state mapping, `error_message`
  (native API), and SABnzbd's `fail_message` (what an *arr app actually
  reads) all already worked correctly end-to-end — but TorBox's own raw
  failure string contains the word "repair", which `usenetPhase`'s substring
  match happily matched regardless of whether the download had actually
  failed. Fixed by only computing `Phase` for a download in
  `StateDownloading` — it was only ever meant to describe an in-progress
  sub-phase, not something a terminal error/completed state should report at
  all. `RawState`/`error_message`/`fail_message` are unaffected, since the
  full detail there is exactly what's needed.

- **A download's reported progress/state/size could freeze on stale data,
  even though the database and TorBox itself had already moved on.** Found
  live watching a real, genuinely uncached torrent download: `GET
  /api/v2/torrents/info` stayed stuck reporting 13.9% while the same
  download's own database row (and TorBox's own API, queried directly) had
  already reached 50%+. Root cause: `database.RefreshFromProvider` had no
  ordering guard — with multiple independent pollers now hitting the same
  download (each compat shim's own reactive refresh, `internal/importer`'s
  bulk tick, and its fast per-download poll), a slower provider request that
  started earlier could finish — and write — after a faster one that
  started later, silently regressing the row back to stale data with
  nothing to ever correct it. The connection pool's single connection
  serializes the resulting `UPDATE`s so they can't corrupt each other, but
  serialization alone doesn't preserve the order the underlying data was
  actually fetched in. Fixed by threading a `fetchedAt` timestamp (captured
  when each poller *starts* its provider call, not when it returns) through
  every `RefreshFromProvider` call site, and rejecting a write whose
  `fetchedAt` is older than the last one already applied for that specific
  download.

### Added

- **A Manual download's detail view now shows when it became available
  on the provider, not just whether files have ever been fetched to local
  disk.** Asked directly by the user after noticing the detail view had no
  timestamp for a torrent already `Available` (cached on TorBox, waiting to
  be manually downloaded) — the existing `completed_at` never fires for a
  Manual download that's never actually pulled to disk, and `updated_at`
  is written on every state-machine change (hash/source backfills, missing-
  count resets, ...), not specifically on becoming cached, so neither was a
  reliable proxy. Added a new `cached_at` column, set once — the first time
  a row is observed as `provider_completed` — by the same
  `UpdateDownloadStatus` call every refresh path already goes through, and
  cleared by `ReAddDownload` (a fresh provider-side download hasn't been
  observed cached yet). Exposed as `cached_at` on `GET /api/v1/downloads`
  and shown as a new "Cached" row in the web UI's download detail view,
  next to "Completed".

- **The native API and web UI now show the same live data the compat
  shims do — ETA, torrent seeds/peers/speed, usenet post-processing
  phase — instead of none of it at all.** `GET /api/v1/downloads` gained
  `eta_seconds`/`seeders`/`leechers`/`download_speed_bytes`/`phase`; the
  downloads table shows live speed inline while actively downloading; the
  detail view adds ETA, speed, swarm info (torrent), and phase (usenet)
  rows, all only while a download is actually in progress. Backed by a new
  in-memory `database.DB.LiveStatus` cache — populated as a side effect of
  whichever poller (either compat shim's own refresh, `internal/importer`'s
  bulk tick or fast per-download poll) already fetches this data — rather
  than adding yet another synchronous provider call per API request, which
  right after fixing a real concurrent-poller race (see Fixed below) would
  have been exactly the wrong direction. Also adds real SABnzbd's own
  aggregate `kbpersec` field to `mode=queue` (confirmed against SABnzbd's
  real API docs: speed is queue-wide there, not per-slot — there's no
  per-item field to match even if AcerviNode wanted one), and models
  usenet's own `download_speed` on `internal/debrid/torbox`'s
  `UsenetDownload` (present on TorBox's real SDK schema but unmodeled until
  now, the same gap the torrent side had).

- **Real qBittorrent's own swarm visibility — `num_seeds`/`num_leechs`/
  `dlspeed` — now appears in `GET /api/v2/torrents/info`.** Found live
  while watching a real, genuinely uncached torrent download (TorBox's own
  instant-cache path never exercises this): TorBox reports `seeds`/`peers`/
  `download_speed` on every torrent (confirmed against its official SDK
  schema), but neither `internal/debrid/torbox`'s own `Torrent` model nor
  anywhere downstream of it ever captured or surfaced these at all. Sonarr/
  Radarr's own `QBittorrentTorrent` model doesn't read these fields, so this
  doesn't change *arr behavior — it's for anyone else inspecting the API
  directly, or a real qBittorrent-compatible client pointed at AcerviNode.

- **A usenet download actively being verified/repaired/unpacked by TorBox
  could show up as `Failed` while it was still legitimately in progress.**
  TorBox's usenet service runs its own SABnzbd-style post-processing
  server-side before a download is retrievable, surfaced through a
  `"Direct Unpack: <phase>"` family of `download_state` strings (confirmed
  against TorBox's own help center, plus a real production bug hit by a
  comparable project, Viren070/AIOStreams #903) that AcerviNode's shared
  torrent/usenet state mapping — ported from decypharr, which doesn't route
  usenet through TorBox at all — never recognized, so any unmatched state
  fell through to its "treat as error" default. Fixed with a usenet-specific
  `mapUsenetState` that uses `download_present`/`download_finished`/`active`/
  `progress` as the authoritative signals instead of an exact-string
  whitelist, so an as-yet-unseen phase name can't trip this again. Also adds
  real SABnzbd's own `Verifying`/`Repairing`/`Extracting`/`Moving` status
  strings to `/queue` (Sonarr/Radarr's own `SabnzbdDownloadStatus` enum
  already supports all four, confirmed against their real source) instead of
  a flat `Downloading` for the whole post-processing sequence. See
  docs/providers.md#usenet-post-processing-states.
  **Live-verified afterward** against two real usenet downloads (a small
  file, then a 6.8GB DVD9 boxset) submitted directly by the user for this
  purpose: neither actually hit the documented `"Direct Unpack: <phase>"`
  family — both instead sat at a distinct, real, previously-unseen
  `"processing"` state for several minutes — proving the fallback logic
  handles a genuinely unrecognized phase correctly (the whole point of not
  using an exact-string whitelist). `"processing"` is now also surfaced
  explicitly as `Verifying` (its closest safe real-SABnzbd equivalent)
  rather than staying in the generic `Downloading` bucket.

- **No completed Managed torrent could ever actually be imported through the
  qBittorrent shim.** Found while auditing what information AcerviNode
  should be passing through to the qBittorrent/SABnzbd compat APIs, then
  confirmed directly against Sonarr's and Radarr's real source (identical in
  both): `GET /api/v2/torrents/info` never sent a `content_path` field —
  only `save_path` — but Sonarr/Radarr's own `GetItems` exclusively resolves
  a completed download's import location from `content_path`, and a missing
  field decodes to `null` in their model, which read as "not equal to
  `save_path`" (the sanity check meant to catch a real misconfiguration) —
  so it used that `null` to build the import path regardless. The SABnzbd
  shim already reported the equivalent field (`storage`) correctly; this was
  qBittorrent-only. Fixed by reporting AcerviNode's own `save_path` (already
  the real per-download content root) as `content_path`, with the response's
  `save_path` synthesized as its parent directory so the two are never equal.
  See docs/qbittorrent-api.md.

- **`deleteFiles=true`/`del_files=1` never actually deleted local files, on
  any of the three delete surfaces (native API, qBittorrent shim, SABnzbd
  shim).** Every delete handler passed the flag straight to `provider.Delete`,
  but TorBox's own implementation ignores it entirely and only removes the
  provider-side copy — the only thing that ever called `os.RemoveAll` was the
  unrelated automatic retention/cleanup policy. Fixed with a new
  `Importer.RemoveLocalFiles`, wired through the `Settings` interface so all
  three delete handlers can reuse it without duplicating
  `internal/importer`'s destination-directory logic. See
  docs/providers.md#local-file-deletion.

- **A Managed download could silently turn into a Manual one.** Root cause:
  `internal/qbittorrent`'s and `internal/sabnzbd`'s own delete handlers never
  recorded a delete tombstone the way the native API's `handleDeleteDownload`
  already did. TorBox's own listing endpoint can briefly still show a torrent
  right after its delete call already succeeded (a previously-confirmed,
  documented race) — without a tombstone, any delete through either shim
  that landed in that window (a user, or an *arr app's own routine "remove
  completed download" cleanup step after import) left the item vulnerable to
  the very next background discovery tick re-adopting it fresh as a
  brand-new Manual download. Fixed by giving both shims' delete handlers the
  same `database.RecordDeletedDownload` tombstone call the native API
  already had. See docs/providers.md#managed-vs-manual.

- **A Managed download still took roughly 2x longer than rdt-client to be
  noticed as finished and fetched to local disk**, even after the `limit=1000`
  fix below. Confirmed via a controlled, same-account, same-content
  comparison against rdt-client's real Managed-equivalent auto-fetch path:
  174.6s vs under 91s for the same already-cached file. Root cause: a
  download's local state only ever advanced on the next
  `import_interval_seconds` tick (10s by default) — a download that finished
  moments after a tick simply waited for the next one, and timing
  instrumentation confirmed the entire delay lived there, not in the actual
  file fetch (microseconds once triggered). Shortening the bulk interval
  directly was tried and rejected: even a 2-second interval immediately
  tripped TorBox's real rate limit.

  Fixed by adding a fast, independent per-download poll (`Importer.runFastPoll`,
  every 3s) that checks only the Managed downloads currently
  queued/downloading, one at a time, via a genuinely cheap targeted lookup —
  TorBox's own `mylist` endpoints return a single object instead of the full
  account listing when filtered by `id` (confirmed against TorBox's official
  SDK docs), the same technique a reference implementation
  ([decypharr](https://github.com/sirrobot01/decypharr)) uses for its own
  active-download polling. `Provider.Status`/`UsenetProvider.Status`/
  `WebDownloadProvider.Status` (already called by every add endpoint to
  confirm a fresh add) switched from a full list-and-scan to this targeted
  call too, an incidental speedup for those call sites as well. A live test
  of the fast poll surfaced a related real bug: `Files()` was still built on
  the slower full listing, which can lag behind the id-filtered lookup for a
  torrent only moments old — causing a spurious "not found" on the very
  first fetch attempt right after the fast poll noticed it (self-healed via
  retry, but needlessly). `Provider.Files`/`UsenetProvider.Files`/
  `WebDownloadProvider.Files` switched to the same targeted lookup too. See
  docs/providers.md#fast-per-download-poll.

- **Every TorBox `mylist`/`getqueued` poll was measurably slower than
  necessary.** Reported directly: "rdt-client communicates with TorBox so
  much faster." `ListTorrents`/`ListUsenetDownloads`/`ListWebDownloads`/
  `ListQueued` always set `bypass_cache=true` (correctly — otherwise a
  freshly added item is simply absent from the response for up to 600
  seconds) but never sent a `limit` param at all. rdt-client's own TorBox
  client always sends `limit=1000` alongside `bypass_cache`. Verified live
  against a real account: response bytes were identical either way (well
  under the cap), but omitting `limit` was consistently 2–4x slower per
  call across repeated back-to-back requests — genuinely more server-side
  work without a `LIMIT` clause, not a transfer-size effect. All four calls
  now send `limit=1000` to match.

  Along the way, a much bigger-looking lead (AcerviNode's usenet `List()`
  apparently seeing only 1 of "590" real items per poll) turned out to be a
  false alarm from a flawed manual `grep -o '"id":' | wc -l` item count,
  which counts every nested `id` field in the JSON (e.g. inside each
  download's own file list), not top-level array length — re-parsed
  properly, the real count matched what AcerviNode itself reported all
  along. No parsing bug exists; the mass-vanish warnings seen while
  investigating were the safety mechanism correctly doing its job on
  genuinely-removed leftover test data. Documented as a lesson in the
  project's own notes, not just quietly dropped.

- **A Managed download whose adding *arr app never supplied an explicit
  save_path never actually got imported, with no visible error** — reported
  live via LibriNode + AcerviNode on shared storage ("worked fine with
  rdt-client, so it's AcerviNode"). Root cause: `internal/importer`'s
  `resolveDestDir` computes a fallback destination
  (`download_dir`/category override + name) whenever a row's `save_path`
  is empty, but only ever used that path locally to write files — it was
  never written back to the database row. Real SABnzbd's own `addurl`/
  `addfile` API has no `save_path` parameter at all, so this hit *every*
  SABnzbd-added download; qBittorrent adds could hit it too whenever the
  caller didn't send one. Both compat shims report `save_path`/`storage`
  straight from that column (`handleInfo`/`handleProperties`,
  `sabnzbd.handleHistory`) for the *arr app's own import step to read — an
  empty value meant Sonarr/Radarr/LibriNode saw the download as
  "Completed" with nothing to actually scan, so it silently never
  imported. New `database.UpdateDownloadSavePath`, called from
  `processDownload` the moment a fallback path is actually used, persists
  it going forward.

- **Adding a magnet-only torrent through the qBittorrent shim failed with
  `HTTP 400: Unsupported Media Type` whenever the client sent a plain
  `application/x-www-form-urlencoded` POST instead of `multipart/form-data`**
  — reported live via LibriNode ("qbit is being a pain," a Prowlarr result
  failing to add). Confirmed against real qBittorrent's own request parser
  (`src/base/http/requestparser.cpp`) that it accepts *both* content types
  for `/api/v2/torrents/add` — urlencoded is entirely normal for a
  magnet-only add with no file to upload. `handleAdd` called
  `ParseMultipartForm` and treated *any* error as fatal, including the
  routine `http.ErrNotMultipart` it returns for a non-multipart body — even
  though `ParseMultipartForm` already calls `ParseForm` internally first,
  so `r.FormValue` works fine either way. Now only a genuine parse failure
  (not "wasn't multipart") is treated as an error. Verified live: the exact
  request LibriNode sends now returns `Ok.` instead of a 400.

- **Every Sonarr/Radarr "Test" against the SABnzbd compat shim crashed
  outright** — reported live as "Test was aborted due to an error: Object
  reference not set to an instance of an object." Root cause: `GET
  /api?mode=get_config`'s category objects never included a `dir` field.
  Confirmed against Sonarr's actual source (`SabnzbdProxy.GetCategories`,
  called unconditionally by `TestCategory` on every Test) that it runs
  `category.Dir.TrimEnd('*')` on *every* category returned — including the
  built-in `"*"` one — with no null check; a missing key deserializes to a
  null C# string, and `.TrimEnd` on that throws the exact unhandled
  exception reported. New `dir` field (empty string — AcerviNode doesn't
  manage per-category directories the way real SABnzbd does) fixes it.
  `TestSonarrCallSequence`'s `get_config` check only asserted the request
  succeeded, never decoded the response shape — exactly how this shipped
  unnoticed; it now decodes generically and asserts every category has a
  `dir` key, so a missing field can't silently pass again.

- **Every Sonarr/Radarr "Test" against the qBittorrent compat shim failed
  outright**, regardless of how correctly everything else was configured —
  found live, reported as "failed test." Root cause: `GET
  /api/v2/app/preferences` didn't exist in `internal/qbittorrent` at all (a
  plain 404). Confirmed against Sonarr's actual source
  (`QBittorrentProxyV2.GetConfig`, called by `TestConnection`) that this is
  the *first* request a real "Test" makes, before checking categories or
  anything else — so the whole flow aborted at step one. New
  `handleGetPreferences` reports `save_path` (AcerviNode's `download_dir`)
  plus fixed "disabled" values for every seeding/ratio/queueing field
  AcerviNode has no concept of (TorBox handles seeding, not AcerviNode).
  `TestSonarrCallSequence` — which already claimed to replicate Sonarr's own
  Test sequence — didn't include this call at all, which is exactly how it
  shipped unnoticed; now it does. Verified live: the endpoint 404'd before
  the fix, returns a real response after.

- **The first-run setup wizard's account-creation step was a dead end if the
  instance turned out to already be set up** — found live: a second tab (or
  a stale reload) still showing the wizard submitted step 0 against an
  instance a first tab had already claimed, got a 403 back, and just sat on
  a red error with no Back/Skip nav to escape it. `SetupWizard` now takes an
  `onAlreadySetUp` callback and detects this specific case (`ApiError` with
  `status === 403`) instead of treating it as a generic failure — `App.tsx`
  wires it to the same re-check `onDone` already does, landing on the login
  form instead. Verified live: two tabs racing the real wizard, the second
  one now correctly ends up at the login form rather than stuck.

### Added

- **Native, self-signed, auto-generated HTTPS support.** Requested after
  live-testing on a Proxmox VM reachable only over a plain LAN IP: the web
  UI's folder-picker download mode (the browser's File System Access API)
  requires a secure context (HTTPS or `localhost`) and simply doesn't exist
  otherwise — no client-side workaround exists, confirmed against MDN/Chrome
  docs, and confirmed the HTML `download` attribute can't fake folder nesting
  either (browsers sanitize `/` to `_` in it, deliberately). Considered and
  dropped a reverse proxy: a real, warning-free certificate needs a public
  domain to validate against, which a private LAN IP doesn't have, so
  self-signed is unavoidable either way.

  **Dual-listen, always** — the existing plain-HTTP listener on `port` keeps
  running completely unchanged; when `tls_enabled`, a second listener on
  `tls_port` (default `8443`) serves the exact same handler over HTTPS.
  Nothing already pointed at `http://...` (Sonarr/Radarr, scripts,
  bookmarks) is ever affected by turning this on or off. New
  `internal/tlscert` package generates an ECDSA P-256 self-signed
  certificate (10-year validity, no rotation logic) covering every local
  interface IP, loopback, `localhost`, and the machine's hostname — same
  auto-generate-on-first-need precedent as the existing API key. An operator
  can supply a real certificate instead via `tls_cert_file`/`tls_key_file`
  (config/env only, same treatment `data_dir` already gets).

  **One-click restart.** Settings changes needing a restart (`port`,
  `tls_enabled`/`tls_port`, ...) already persisted before this — now a
  **Restart now** button (`POST /api/v1/settings/system/restart`) actually
  does it too, reusing `run()`'s existing shutdown path (`signal.NotifyContext`'s
  own `stop` function, wired in as the trigger — zero new shutdown plumbing).
  `packaging/acervinode.service` changed `Restart=on-failure` to
  `Restart=always`, since the endpoint's clean exit is exactly what
  `on-failure` doesn't restart. The UI is honest when nothing's actually
  supervising the process (checks systemd's `INVOCATION_ID`) rather than
  showing a confident "restarting…" that never comes back — found live,
  testing against a plain `nohup`'d instance, which is exactly that case.

  **Regenerate certificate** button (Settings → General, mirrors "Regenerate
  API key") for when the cert's baked-in SANs no longer match how the
  instance is reached (e.g. its LAN IP changed after a DHCP renewal).

  **First-run setup wizard** gained an HTTPS step between TorBox and Done,
  showing the literal `https://<host>:<port>` URL to visit afterward as
  text — the restart doesn't move the current browser tab there on its own.

  **Verified live** on a scratch instance (never the real ones): confirmed
  `showDirectoryPicker` is genuinely unavailable over `http://<lan-ip>` and
  available over `https://<lan-ip>` with the same self-signed cert (SANs
  correctly covering the real interface IP, not just `localhost`); confirmed
  the regenerate-certificate button actually changes the cert on disk;
  confirmed an unsupervised restart correctly stops the process and says so
  rather than claiming success. A real layout bug this surfaced: the HTTPS
  checkbox inherited `.general-form input`'s text-field padding, rendering
  as an oversized box — fixed. See
  [Providers](docs/providers.md#tls-self-signed-https).

### Changed

- **Settings reorganized into grouped tabs, *arr-style** — requested
  directly ("make the settings more organized like librinode"). Read
  LibriNode's actual `SettingsView.tsx` rather than guessing at the pattern:
  a `settingsGroups` array (name/icon/one-line blurb) driving a `subnav` of
  tab buttons, only the active group's content rendered below, and a
  `Section` helper (title + optional help + children) breaking a crowded
  card into labelled blocks. AcerviNode's version is deliberately smaller —
  five groups (General, Provider, Categories, Downloads, Security) instead
  of LibriNode's six, since there's one provider here, not a full media
  manager's worth of libraries/quality profiles/indexers. The General
  group's previously-undifferentiated wall of inputs (API key + all eight
  general-settings fields in one flat form) now reads as three sections:
  API key, Import & cleanup, Instance.

### Added

- **Optional login accounts with two roles (admin/member), on top of the API
  key** — requested directly ("maybe we should have login, users and first
  time setup wizard"), with roles specifically "because of manual download
  ability and possible future additions." Researched LibriNode's actual real
  implementation first rather than designing from scratch (`internal/api/
  auth.go`, `internal/config/config.go`'s `AuthSettings`/`UserAccount`,
  `App.tsx`'s gating, the full `SetupWizard.tsx`) — same PBKDF2-SHA256 hash
  format, same in-memory session store, same first-run wizard trigger logic,
  matching its actual feel rather than approximating it.

  The API key is completely unaffected and stays the root-equivalent master
  credential — Sonarr/Radarr and scripts keep using it exactly as before. No
  login accounts means auth is disabled entirely (`AuthSettings.Enabled()`),
  which is how an *upgrade* stays inert: confirmed live against the real WSL
  instance, `auth_enabled` read `false` and everything continued working
  unchanged after deploying this.

  **Roles**: `admin` does everything (Settings, user management, both
  Managed and Manual tabs). `member` is scoped to Manual downloads only —
  no access to the *arr-driven Managed pipeline (interfering with something
  Sonarr/Radarr is actively tracking is a bigger deal than a member managing
  their own manual grabs) and no Settings access. Enforced server-side, not
  just hidden in the UI: `downloadByID` (the single choke point every
  single-download handler routes through) 403s a non-admin touching a
  Managed row; `handleListDownloads` forces `added_via=manual` for a
  non-admin regardless of the request's own query param; every
  `/api/v1/settings/*` route is `requireAdmin` except password
  self-service.

  **First-run setup wizard**: claims a genuinely fresh instance (no login
  account *and* no provider configured — deliberately not also a database
  check, since every download requires an active provider already) in one
  step — Account → TorBox key (skippable, with a live Test) → Done. Much
  shorter than LibriNode's own 6-step wizard since AcerviNode's whole setup
  surface is one provider.

  **The Default account** (whoever the wizard created, or anyone since
  promoted) can't be removed or demoted — guarantees a login-enabled
  instance always has at least one admin able to sign in.

  New: `internal/config/auth.go` (roles, accounts, all the mutation rules —
  16 tests), `internal/api/auth.go` (hashing, sessions, every handler — ~35
  tests spanning password hashing, session lifecycle, `requireAuth`/
  `requireAdmin`, setup/login/logout, user management, and member row-level
  enforcement), `cmd/acervinode` wiring (6 tests). Frontend:
  `SetupWizard.tsx`, `LoginForm.tsx`, `SecuritySettings.tsx` (Settings →
  Security), `App.tsx`'s gating rewritten to insert setup/login ahead of the
  existing API-key prompt rather than replacing it, Managed/Settings tabs
  hidden for a member.

  **Verified live end to end** on a genuinely separate scratch instance (own
  `config.yaml`/data dir) — never the real one, deliberately: adding even
  one test user to the real instance would have become permanently
  un-removable (the Default-account protection) and flipped it into
  requiring login for an instance actually in use. On the scratch instance:
  fresh install correctly reported `setup needed: true`, `POST /setup`
  created the first admin and signed in, a `member` account correctly got
  403 from Settings and 200 (empty) from downloads, correctly reached the
  provider layer (503, no TorBox key there) rather than being blocked by
  auth when adding a webdl link, and logout correctly ended the session
  (401 afterward). The real instance was only ever hit with safe, read-only
  checks confirming `auth_enabled: false`/`setup needed: false`/existing API
  key still works. See
  [Providers](docs/providers.md#auth-login-accounts-and-roles).

### Changed

- **Login is now mandatory for the web UI; the old API-key-only
  `ApiKeyGate` browser prompt is gone.** Once a real admin account existed
  on the real instance (from live-testing the setup wizard), there was no
  longer any reachable state where the dashboard needed to fall back to a
  raw API key — a login-enabled instance can never go back to having zero
  accounts (the Default-account protection). Simplified accordingly:
  `SetupNeeded` is now just `!AuthEnabled()` instead of also checking
  whether TorBox was configured; `App.tsx`'s gating dropped the
  `auth_enabled`-conditional branches entirely (`ready`/`activeKey`/
  `isAdmin` no longer branch on it); `ApiKeyGate.tsx` and the unused
  `getStoredApiKey`/`storeApiKey`/`clearStoredApiKey` helpers were deleted.
  The API key is unaffected as the master credential for Sonarr/Radarr and
  scripts — this only removes a browser-only bypass a human could use
  instead of signing in.

  Also fixed a real bug this surfaced: the Downloads popup window
  (`DownloadWindow.tsx`, used for streamed folder downloads) required a
  *stored* API key to fetch file links, which is never set for anyone who
  only ever signs in with a username and password — the popup would show
  "Not signed in" for every session-only user. It now relies on the same
  cookie every other authenticated call already uses.

### Fixed

- **A fresh install never showed pre-existing TorBox downloads in the
  Manual tab, even once it recognized the account.** Found live: a fresh
  Proxmox install. Root cause: `Importer.discoverManual`'s first-ever run
  for a provider+kind always took the conservative "established instance"
  branch — recording everything currently unmatched into a permanent
  ignore-list instead of adopting it — regardless of whether the instance
  had ever tracked anything before. That's the right call for this feature
  (or a newly added second provider) landing on an instance that's already
  been running a while, but wrong for a genuinely fresh install, where
  there's no pre-existing history to protect and the account's current
  contents are exactly what a first-time user expects to see. New
  `database.HasAnyDownloads` (has this database ever tracked a single
  download, of any kind) lets `discoverManual` tell the two cases apart;
  computed once per `refreshStatuses` tick, before any kind's own discovery
  runs, so torrent/usenet/webdl agree on the answer within the same tick
  rather than the first kind processed making the database non-empty for
  the next one. See [Providers](docs/providers.md#completed-download-handling-internalimporter).
- **Two real layout bugs found visually verifying the Settings reorg with a
  scripted Playwright pass** (chromium-cli wasn't available in this
  environment, so a throwaway local Playwright + a scratch instance stood
  in for it — same never-touch-the-real-instance discipline as every other
  live-tested feature here). Screenshots caught what `tsc`/build alone
  never would have:
  - The General group's form lost its `general-form` wrapper class in the
    reorg, so `.settings-card form`'s row layout (meant for the small
    inline TorBox-key form) applied instead — the Import & cleanup and
    Instance sections rendered as two overlapping columns with the Save
    button floating over them. Fixed with a dedicated
    `.settings-form-stack` class forcing column layout back.
  - Pre-existing, unrelated to the reorg but on the same page: Security's
    user rows used `flex: 1` (a 0% flex-basis) on the name/badge span,
    which let it shrink below its own content width inside the narrow
    `.settings-card` — the "default" badge visibly overlapped the role
    `<select>` instead of the row wrapping. Fixed with `flex: 1 1 auto` so
    the row wraps onto a second line instead of compressing content into
    an overlap.
- **Account-creation forms (Setup wizard, Settings → Security's "Add account")
  had no `autoComplete` hints, so the browser's own saved-credential autofill
  silently repopulated the username/password fields** — found live-testing
  the setup wizard on a scratch instance ("user name and password sticks
  around here even after reboot"), which looked like state persisting across
  restarts but wasn't. Fixed with `autoComplete="off"`/`"new-password"` on
  every account-creation input; `LoginForm.tsx` got the opposite treatment
  (`autoComplete="username"`/`"current-password"`) since a genuine sign-in
  form *should* offer saved-credential autofill.
- **Settings → Security's "Add account" form only had one password field**,
  unlike the setup wizard's own account step — a typo went straight to a new
  account with no way to catch it. Added a matching confirm-password field
  with the same match validation the wizard already had.

- **`data_dir` was editable in Settings but silently didn't move the
  database** — found by the user looking at a screenshot of their own
  Settings page. `PUT /api/v1/settings/general` already correctly required
  a restart to apply a `data_dir` change, but nothing about the UI (or the
  restart-required message) warned that a restart doesn't move
  `acervinode.db` to the new path — `main.go` just opens/creates one fresh
  wherever the configured path points. Editing it and restarting would look
  like every download and all local history had vanished, with the real
  data sitting untouched but unread at the old path. The web UI now shows
  `data_dir` read-only (with an explanation, and where to actually change
  it — `config.yaml`/`ACERVINODE_DATA_DIR`, after moving the file
  yourself), rather than inviting an edit that has a much bigger blast
  radius than it looks like. The API itself is unchanged — `data_dir` is
  still part of `GeneralUpdate`'s contract for a caller that wants it, the
  UI just no longer offers a casual path to it. See
  [Configuration](docs/configuration.md).

### Added

- **Consolidated the download UX into one dialog, and fixed a real
  cross-browser download bug along the way.** Three separate "download
  everything" paths existed (the table row's default-mode button, the
  detail view's always-both-visible "Download all"/"Download all (zip)"
  buttons, and a Settings → Downloads preference controlling only the row's
  default) — replaced with a single `DownloadOptionsDialog`, opened
  identically from the table row and the detail view, showing every mode
  this browser can actually do (folder streaming is simply absent, not
  shown-then-disabled, on a browser without `showDirectoryPicker`) as
  explicit radio choices, and remembering the last one picked as next
  time's default instead of that living in a separate Settings dropdown.

  While investigating this, found that the per-file "Download" button and
  the Firefox/Safari "individual files" fallback both used a plain
  `window.open` — and since the provider's per-file link (unlike the zip
  link) carries no `Content-Disposition: attachment` header, this rendered
  the file inline (played the video, showed the image) instead of
  downloading it, in *every* browser, not just Firefox/Safari. New
  `fsAccess.forceDownload` (fetch → blob → synthetic `<a download>` click)
  fixes this everywhere: blob URLs are always same-origin, so the
  `download` attribute reliably applies regardless of the provider link's
  own cross-origin status — a plain `<a download>` pointed directly at a
  cross-origin URL is unreliable (several browsers ignore the attribute
  cross-origin and just navigate instead). Deliberately *not* used for the
  zip link — it already downloads reliably as a plain link since the
  provider's own response sets `Content-Disposition`, and blob-buffering a
  potentially multi-GB zip in memory first would be a real regression.

  Also directly investigated (at the user's prompting) whether Firefox or
  Safari could get genuine folder-write parity with Chromium at all, even
  via browser-specific detection — they can't: neither engine implements
  the underlying capability, there's no flag or experimental opt-in, and
  the only real way to add it would be a companion browser extension (a
  fundamentally different, much bigger undertaking — separate codebases,
  store review/signing, ongoing maintenance across two more platforms —
  than a UI consolidation pass). Explained to the user and ruled out rather
  than pursued.

  Settings → Downloads now only manages the remembered default folder;
  the mode-choice dropdown that used to live there is gone, replaced by the
  dialog's own memory. Same caveat as every other frontend feature this
  session: type-checks and builds clean, confirmed live-serving the
  updated bundle (the deployed JS bundle was checked directly for the new
  dialog's copy), but not confirmed by an actual browser click-through —
  that needs a real user gesture (the folder picker, the download prompts)
  this environment can't provide. See
  [ROADMAP](ROADMAP.md#path-to-daily-driver-parity-replacing-rdt-client--decypharr).

- **First three items off the daily-driver parity punch list**: requested
  directly ("complete the first 3 tasks in Path to daily-driver"), all
  scoped, implemented, and tested independently.
  - **Mass-vanish circuit breaker** — the vanish-detection debounce only
    ever protected against one item briefly disappearing from an otherwise
    normal listing, not against a provider listing coming back
    successful-but-empty or truncated (a partial outage, a transient
    backend bug), which would otherwise have flagged every tracked Manual
    download `error` at once within the same few ticks. `isSuspectedMassVanish`
    (`internal/database`) now refuses to run missing-detection for a whole
    pass when more than half of at least 3 tracked Manual downloads for a
    kind are missing at once — found rows in the same pass still update
    normally, only the missing-side detection is suppressed.
  - **Rate-limit-specific backoff** — a provider rate limit (`429`) used to
    retry on every single tick regardless, which can itself extend how long
    the rate limit lasts. `debrid.ErrRateLimited` is a new provider-agnostic
    sentinel (`torbox.APIError.Unwrap` resolves to it for a `429`
    specifically, recognizable via `errors.Is` through however many wrapping
    layers); `internal/importer` now backs off that kind's own polling
    specifically (30s base, doubling per consecutive hit, capped at 5
    minutes — deliberately much shorter than the 1-hour per-download fetch
    backoff, since a rate limit is a short provider-side condition, not a
    download-specific failure), scoped independently per kind
    (torrent/usenet/webdl) so one kind's rate limit doesn't pause the
    others. Motivated directly by a real incident, not a hypothetical: a
    burst of manual live testing sustained a real TorBox 429 for several
    minutes straight earlier this session.
  - **Retention/cleanup policy** — nothing previously removed a completed
    download automatically; local disk usage and the `downloads` table both
    grew without bound. New `cleanup_after_days` config (0/disabled by
    default — the only setting in this config where 0 is a meaningful valid
    value) has `Importer.cleanupOldDownloads` (runs last in `Tick`) remove a
    **Managed** download's local files, provider-side copy (best-effort),
    and row once it's sat in `ready_for_import` (already handed off to
    Sonarr/Radarr) for at least that many days. Deliberately never touches a
    Manual download — that's the ongoing "available, not yet grabbed" state
    for something the user hasn't downloaded, not something safe to
    auto-delete. Reuses the same delete-tombstone race-avoidance a
    user-initiated delete gets (`database.RecordDeletedDownload`), since
    this runs on `Tick`'s own independent schedule. Surfaced in the web
    UI's Settings → General as "Clean up Managed downloads after (days, 0 =
    off)".

  All three verified live against the real WSL instance in addition to new
  unit tests at every layer (mass-vanish: 4 tests; rate-limit: sentinel
  unwrap + 4 importer-level backoff tests; cleanup: config validation/env
  tests, a DB eligibility-query test, and 4 importer-level tests including
  the empty-`Name`-skips-file-removal safety guard and the
  provider-delete-failure-still-cleans-up-locally best-effort case).
  `cleanup_after_days` was set live to a real value via the settings API,
  confirmed round-tripping through `GET`/`PUT /api/v1/settings/general`,
  then restored to disabled (the account had no eligible Managed downloads
  at the time, so nothing was ever actually cleaned up live — the positive
  path is unit-test-verified only). See
  [Providers](docs/providers.md#retentioncleanup-policy).

### Fixed

- **A just-deleted download could reappear as a "ghost" Manual download** —
  found live while verifying the Source-backfill feature below: deleting a
  download, then immediately checking again, sometimes showed it right back
  as "Available," even though it was genuinely gone from the provider. Root
  cause: a provider's own delete isn't always instantly reflected in its own
  listing endpoints (confirmed live against a real account — TorBox's
  `mylist` could still briefly show a torrent right after its delete call
  returned success), and `internal/importer`'s discovery step runs on its own
  schedule, independent of any specific delete request — a tick landing in
  that narrow window sees the still-technically-present item with no local
  row anymore and adopts it fresh, as if it were new. `handleDeleteDownload`
  now tombstones every real delete (new `deleted_downloads` table, migration
  `0007_deleted_downloads.sql`) before removing the local row;
  `discoverManual` skips adopting anything tombstoned within the last 5
  minutes (generous on purpose — a deleted `provider_download_id` never
  legitimately reappears, since a fresh add always gets a new one, so this
  only ever blocks re-adopting the exact same now-defunct id). Verified live:
  the exact repro (add directly through TorBox, discover it, delete it
  through AcerviNode, immediately recheck) no longer reappears. New tests at
  every layer: the tombstone round-trips and is scoped per provider+kind,
  old tombstones get pruned automatically, `discoverManual` actually skips a
  tombstoned item end-to-end, and `handleDeleteDownload` records one on a
  real delete. See
  [Providers](docs/providers.md#managed-vs-manual).

### Added

- **Stored NZB files enable Re-add for file-uploaded usenet downloads** —
  direct follow-on to the Source-backfill entry below, prompted by the user
  asking to store NZB info specifically so no orphaned file could ever be
  left on disk. A usenet download added through AcerviNode's own "+ Add"
  form as an uploaded `.nzb` (not a URL) had no `Source` and, unlike a
  torrent (covered by the hash-reconstructed magnet) or a discovered NZB
  (nothing was ever uploaded to AcerviNode for one), no way to backfill one
  either — the raw bytes only ever existed in that one request. Chose to
  store them directly on the `downloads` row as a `BLOB`
  (`source_file`/`source_file_name`, migration `0008_source_file.sql`)
  rather than a separate file on disk specifically so the file's lifecycle
  is tied to the row's: deleting the download (`DeleteDownload`) removes
  the stored file atomically with it, with no separate cleanup step and no
  way for a stray file to survive a deleted row. The blob itself is
  deliberately excluded from the normal list/detail read path
  (`downloadColumns`/`scanDownload`) — only the cheap `source_file_name` is
  included there, enough to compute `has_source` without paying for the
  file bytes on every poll; the actual bytes are fetched via a dedicated
  `GetSourceFile`, called exactly once, only when `handleReAddDownload`
  actually needs to resubmit them. `handleAddUsenet`'s file-upload path now
  stores the bytes right after a successful add; `handleReAddDownload`
  falls back to `AddNZBFile` with the stored bytes when `Source` is empty.
  `has_source` (and so the web UI's Re-add button) now reflects either
  `Source` or a stored file being present. New tests covering the full
  round trip: storing via a real file-upload add, fetching back via
  `GetSourceFile`, confirming the blob is absent from a normal row read
  while the filename isn't, and `handleReAddDownload` actually calling
  `AddNZBFile` (not `AddNZBURL`) with the right bytes for a file-based row.
  See [Providers](docs/providers.md#re-add-for-a-file-uploaded-nzb-not-discovered).

- **Extended Re-add to Manual downloads, and a streamed "Download all" button
  in the detail view** — immediate follow-on to the vanish-detection feature
  below, prompted directly by the user after seeing a healthy download's
  detail view and asking for both. The vanish-detection feature can now put
  a Manual download into `error` state, but there was no recovery action
  shown for Manual at all (Retry/Re-add were still gated to
  `added_via === 'arr'` only) — a real gap the feature itself just created.
  `POST /api/v1/downloads/{id}/readd` already worked for any kind/added_via
  server-side (it only ever checked `state === error` and a stored `Source`,
  never `added_via`); the restriction was purely a frontend condition. Split
  the web UI's Retry/Re-add block so Retry stays Managed-only (there's
  genuinely no local fetch to retry for a Manual download) while Re-add now
  shows for *any* download in error state that has a stored source link,
  gated on a new `has_source` field (`GET /api/v1/downloads[/{id}]`) rather
  than blindly showing a button that would 400 for a discovered download
  with no original link ever known. Also closes a smaller pre-existing gap
  the same way: a Managed download added via an uploaded file (no source
  either) previously showed a Re-add button that would have failed the same
  way — now correctly hidden for that case too.

  Separately, the detail view's Files section gained a "Download all" button
  (streamed straight to a local folder, or a tab-per-file fallback) next to
  the existing "Download all (zip)" — the exact same entry point
  (`handleDownloadAll`) the downloads table's per-row button already used,
  reused rather than duplicated, so it gets the same mode preference/folder-
  picker-dialog/Downloads-popup-window behavior for free. Previously this
  streamed option only existed as a row-level action in the table, not in
  the detail view itself.

  **Not verified by a real browser click** — the button renders and the
  plumbing type-checks/builds clean, and `has_source` was confirmed live
  against real account data (a genuinely stored Mega link vs. a genuinely
  empty source on different real rows), but actually clicking through the
  new "Download all" button's folder picker/streaming behavior in the
  detail view specifically hasn't been.

- **Backfilled `Source` for discovered downloads, so Re-add can actually work
  for them** — direct follow-on, prompted by the user asking "can we store
  the nzb info for retry?" after seeing a discovered download with no Re-add
  button at all (per the entry above, that's because a discovered download
  never had a `Source` recorded — there was no add request through
  AcerviNode to capture a link from). Researched what each of TorBox's three
  services actually exposes, confirmed live rather than assumed: a torrent
  needs nothing from TorBox at all — a magnet is always reconstructable from
  just its `hash` (`magnet:?xt=urn:btih:<hash>`), confirmed that TorBox
  itself doesn't reliably record one anywhere retrievable (a real
  magnet-added torrent's `mylist` entry had both `magnet` and `original_url`
  null); usenet and webdl `mylist` responses both include an undocumented
  `original_url` field, populated for a URL-based add and `null` for a
  file-upload-based one (confirmed live for both, including uploading a real
  NZB file provided directly for this test). `debrid.DownloadStatus` gained
  an `OriginalURL` field carrying whichever applies per provider adapter;
  `internal/importer.discoverManual` sets a newly-discovered row's `Source`
  from it immediately, and a new `database.BackfillSource` (wired into
  `RefreshFromProvider`, mirroring the existing hash/name backfill's
  empty-only gating) retroactively fills it in for a row already tracked
  before the provider happened to report one. The real, unavoidable limit:
  once a download has already vanished from the provider entirely, there's
  nothing left to backfill from — Source can only be recovered while the
  download is still visible in a listing, and a discovered download
  originally added via a file upload never had a URL to begin with.

  **Verified live end to end, the most thorough verification of any single
  change this session**: a real Creative Commons torrent (Big Buck Bunny)
  was added directly through TorBox, bypassing AcerviNode entirely, and
  correctly discovered with `Source` already set to the reconstructed
  magnet; deleted directly from TorBox to simulate a genuine vanish;
  AcerviNode's own background polling flagged it `error` on its own,
  unprompted; and `POST .../readd` was called for real and *actually
  successfully resubmitted* the reconstructed magnet, landing a fresh,
  real `queued` torrent on the account — not just a mocked assertion that
  the right string got stored. A real NZB file confirmed the file-upload
  case's `original_url: null` directly. Along the way, briefly investigated
  what looked like real data loss (several of the user's own real,
  in-progress downloads had vanished from the local database) — turned out
  to be the user themselves cleaning up their own downloads through the web
  UI while this testing was happening in parallel on the same live
  instance, not a bug; confirmed directly with the user before continuing
  rather than assumed. See
  [Providers](docs/providers.md#re-add-for-a-discovered-download).

- **Proactively detect a vanished Manual download** — closes a
  previously-documented, long-standing gap (ROADMAP.md's Phase 7): a Manual
  download whose provider item disappears (deleted directly through the
  provider's own site, or genuinely expired — confirmed as a real, recurring
  scenario twice this project, once for a torrent and once for a usenet
  download) used to just sit looking "Available" forever, since
  `internal/importer`'s fetch-retry path (what catches this for a Managed
  download within a few ticks) never applies to a Manual one at all.
  `RefreshFromProvider` now increments a new `downloads.missing_count` column
  (migration `0006_missing_count.sql`) each time a tracked `AddedViaManual`
  row is absent from a *successful* provider listing, flagging it `error`
  (`"no longer found in the provider's account"`) once it's been missing for
  3 consecutive ticks — a debounce, not a single-miss rule, since a row can
  legitimately be briefly absent from a provider's own listing endpoints
  right around the moment it starts being tracked (the same class of
  eventual-consistency gap the hash/name backfill bug fix below already had
  to account for). Deliberately scoped to Manual only (a Managed row's
  counter never moves) and deliberately not sticky (never touches
  `RetryCount`, so a download that reappears later self-heals automatically,
  reusing the existing provider-error-recovery logic unchanged). No frontend
  changes needed — the existing error badge/message display and the
  `added_via === 'arr'`-only Retry/Re-add gating already handle this
  correctly. Full test coverage: a Managed row is never flagged, a single
  miss doesn't flag a Manual row, the exact threshold-th miss does, an
  early reappearance resets the counter, a flagged row self-heals, and an
  already-errored row isn't double-flagged. See
  [Providers](docs/providers.md#proactively-detecting-a-vanished-manual-download).

- **TorBox Web Downloads + account status**, built autonomously per explicit
  user go-ahead ("Web Downloads (the real ask) + a small TorBox account-status
  display (UserService), skip the rest"), after evaluating all 7 of the
  TorBox SDK's service categories against AcerviNode's actual purpose and
  ruling out the other 6 (Integrations pushes files the wrong direction for
  this project's model, RssFeeds duplicates Sonarr/Radarr's own job,
  Notifications/General aren't relevant, Queued is already implemented).
  "Mega API support" turned out to mean TorBox's generic hoster-debrid
  service, not something Mega-specific — confirmed by fetching TorBox's real
  OpenAPI spec directly (`https://api.torbox.app/openapi.json`) rather than
  trusting the SDK's docs a second time, since those had already been wrong
  once this project (`usenetdownload_id`'s real type).
  - New `debrid.WebDownloadProvider` interface (`AddLink`/`Status`/`List`/
    `Files`/`RequestDownloadLink`/`RequestZipDownloadLink`/`Delete` — link-only,
    no file-upload variant, matching TorBox's own `createwebdownload` which has
    none either) and `debrid.AccountProvider` (`Account`, one method) — both
    optional, structural interfaces like `UsenetProvider`, not every provider
    needs either. `torbox.WebDownloadProvider` and `torbox.Provider.Account`
    implement them; `DynamicWebDownloadProvider` mirrors the existing
    Dynamic*Provider live-swap pattern, and `DynamicTorrentProvider.Account`
    delegates to its inner provider via a type assertion rather than adding a
    whole fourth Dynamic wrapper for one read-only call.
  - New `downloads.kind = 'webdl'` (migration `0005_webdl_kind.sql` — SQLite
    can't `ALTER` a `CHECK` constraint in place, so this recreates the table
    with the widened constraint, same pattern as any other CHECK-widening
    migration). Every `webdl` row is always `added_via: manual` — there's no
    *arr-facing shim for this kind, so nothing else could add one.
  - New endpoints: `POST /api/v1/downloads/webdl` (link-only,
    `application/x-www-form-urlencoded`, mirrors the torrent/usenet add
    endpoints otherwise — dedup, provider-status fallback, `readd` support)
    and `GET /api/v1/settings/account` (plan tier, subscription state, premium
    expiry, lifetime bytes downloaded — always HTTP 200, `available: false`
    with a reason if nothing's configured or the provider doesn't support it,
    the same "routine, not fatal" stance as the rest of the settings API).
  - `internal/importer` gained `SetWebDownloadProvider` (a post-construction
    setter, not a third `New()` param — every existing test call site would
    otherwise need an argument it doesn't care about) so both proactive
    status-refresh and discovery (a hoster link added directly through
    TorBox's own site, not through AcerviNode) work for `webdl` exactly like
    they already do for torrent/usenet.
  - Web UI: a third "Web Link" tab in "+ Add" (link-only, no mode toggle since
    there's no file-upload variant to offer), and a TorBox account section in
    Settings showing plan/subscription/premium-expiry/total-downloaded, fed by
    the new endpoint.
  - **Everything was eventually confirmed live**, including the two things
    that briefly looked unverifiable — two earlier attempts at a safe test
    web download failed (a GitHub raw-file link came back
    `UNSUPPORTED_SITE`, PixelDrain's anonymous upload now requires its own
    API key), but `archive.org` turned out to be one of the ~160 supported
    hosters itself, and a small public-domain test audio file it hosts
    (`archive.org/download/testmp3testfile/mpthreetest.mp3`) made a real,
    safe end-to-end test possible after all: `createwebdownload`'s response
    field `webdownload_id` — documented as a string, initially handled
    defensively via a `flexibleID` type pending confirmation — was confirmed
    via a raw API call to be a JSON number (`{"webdownload_id": 1462379,
    ...}`), the same mismatch `usenetdownload_id` turned out to have;
    `flexibleID` was then simplified back to a plain `float64` field, same as
    every other provider-assigned id. `RequestWebDownloadZipDownloadLink` was
    confirmed live too: the resolved URL served a real `application/zip` with
    the correct `content-disposition`. The full add → status → files →
    per-file-link → zip-link → delete cycle was run end to end through
    AcerviNode's own live API against that test file, and the provider-side
    delete was independently confirmed by querying TorBox's own
    `webdl/mylist` directly afterward — genuinely gone from the account, not
    just the local row. See [Providers](docs/providers.md#web-downloads).
  - Also confirmed live against the real account: Mega is active among
    TorBox's ~160 supported hosters (`GET /webdl/hosters`), a real (if
    since-expired) Mega folder download already existed in the account's own
    history — which is what confirmed `mylist`'s actual JSON shape (including
    a legitimate file `id: 0`) — and `GET /user/me`'s real response has far
    more fields than either the SDK's docs or its own Go types declare,
    confirming the account is a real Pro (`plan: 2`) subscription.
  - New tests throughout: TorBox client (`httptest` fakes for
    `CreateWebDownload`/`ControlWebDownload`/`RequestWebDownloadLink`/
    `ListWebDownloads`/`GetHosterList`/`GetUserData`), a
    `webdl_provider_test.go` mirroring the existing provider tests,
    `DynamicWebDownloadProvider`/`DynamicTorrentProvider.Account` tests in
    `internal/debrid`, `internal/api` handler tests for
    `handleAddWebDownload`/`handleGetAccountStatus`, an `internal/importer`
    test proving `SetWebDownloadProvider` wiring discovers a manually-added
    web download, and an `internal/database` regression test confirming a
    `kind='webdl'` row actually inserts and round-trips (not just that the
    migration runs without erroring).

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
