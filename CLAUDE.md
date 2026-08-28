# Working on AcerviNode

Read this before touching anything. The `docs/` tree explains **what the system
does**; this file explains **how to change it without breaking things**, and
concentrates on what you could not infer from the code alone.

---

## What it is, in one paragraph

A self-hosted download client for **debrid services** (TorBox, AllDebrid). It
impersonates qBittorrent and SABnzbd so Sonarr/Radarr/Lidarr/Readarr can hand it
grabs, sends them to a debrid provider, waits for the provider to finish, then
fetches the resolved files to local disk over plain HTTP. One static Go binary
with an embedded React dashboard and a pure-Go SQLite database. ~36k lines of
Go, ~9k of TypeScript. Linux + systemd is the packaged deployment.

**Managed vs Manual** is the central distinction. Managed downloads (added by an
\*arr app, or by hand through the UI marked as Managed) are auto-fetched to
disk. Manual ones — added directly, or *discovered* already sitting in the
provider account — are never auto-fetched; you browse and grab files on demand.
Almost every behavioural question ("does cleanup touch this?", "does the
watchdog?") resolves to which of the two it is.

## Read these, in this order

| Doc | What you get |
| --- | --- |
| `README.md` | Feature list, and an honest "what this is and isn't" |
| `docs/providers.md` | The provider layer, routing, and every hard-won provider quirk |
| `docs/api.md` | `/api/v1`, the add endpoints, and the add form's client-side behaviour |
| `docs/configuration.md` | Every setting, its default, and whether it needs a restart |
| `docs/development.md` | Build, test, deploy |
| `ROADMAP.md` | What is built, what is deliberately not, and the parity list against rdt-client/decypharr |
| `CHANGELOG.md` | Why things are the way they are. Long, and worth grepping before proposing a change |

---

## The seams

- **`internal/debrid`** defines `TorrentProvider`, `UsenetProvider`,
  `WebDownloadProvider`. A new provider implements these and nothing structural
  changes. `debrid.Registry` indexes providers **by name, not by service type**
  — that split is what allows two accounts on one service (`providers.<name>.type`).
- **`internal/importer`** is the tick loop: refresh status from providers, fetch
  completed downloads to disk, run cleanup/watchdog. Most background behaviour
  lives here.
- **`internal/api`** is `/api/v1` plus the two compat shims
  (`internal/qbittorrent`, `internal/sabnzbd`).
- **`web/src/detect.ts`** is the add form's whole brain — type detection,
  encoding unwrapping, batch parsing. Pure functions, heavily tested. See the
  invariants below before editing it.
- **Settings** flow: `internal/config` holds the struct, `cmd/acervinode/settings.go`
  persists and applies live, `internal/api` exposes them, `web/src/components/Settings.tsx`
  renders them. Adding one means touching all four plus `web/src/api.ts`.

Sentinel errors matter: `debrid.ErrNoProvider`, `ErrRateLimited`,
`ErrHostNotSupported`, `ErrTorrentInfoUnsupported`. They are resolved through
`errors.Is` across wrapping layers — map new provider failures onto them rather
than inventing parallel ones.

---

## Invariants — rules that look arbitrary and are not

Break one of these and something subtle goes wrong later. Each was learned by it
going wrong.

**`detect.ts` must stay pure.** Behaviour that is a user preference is threaded
through `DetectOptions`, never read from module state. A module-level flag would
make the property sweep order-dependent and non-deterministic.

**A decode is only kept if it *lands* on a magnet, infohash or URL.** A bare
40-hex infohash is itself valid base64 and decodes to binary noise. Accepting
decodes on their own merit destroys it. Two guards exist: `isRecognisable`
returns early, and `decodeOnce` rejects control characters. Do not remove either.

**`isRecognisable` requires a single token.** The scheme tests only anchor at the
start, so without the whitespace check a decoded *list* beginning with a magnet
is swallowed whole as one enormous link.

**`sanitizeBatch` must be idempotent.** The input field is rewritten with its own
output, so a value that changes again on the next pass is a link that differs
from the one shown. Two bugs have already lived here.

**`normalizeWebLink` must return the trimmed value when empty.** Callers test the
result against `""` to decide whether a link was supplied; returning the
untrimmed input let a whitespace-only link reach the provider.

**Add and check-cached must normalise identically.** TorBox keys a web
download's cache entry on an MD5 of the link, so normalising one and not the
other answers about a string that never gets added.

**`added_via` cannot distinguish a hand-added Managed download from an \*arr
grab** — both are `arr`. The distinction is that the add endpoints record
`delete_after_fetch`/`keep_files` only when the caller supplies them, so an
\*arr grab leaves them `NULL`. The stored NULL *is* the distinction; do not
"fix" it with a special case.

**Explicit `provider=` is never redirected.** The add path falls back across
three axes (kind → file host → credentials), but only for a provider *it* chose.
A caller who named one asked for that account specifically.

**Migrations are forward-only and numbered.** Never edit a shipped migration.
Several tests assert the count; update them when you add one.

**SQLite is one connection with WAL.** Slow writes block everything, which is why
WAL exists. Do not add a second connection without understanding why there is one.

---

## How to work here

**Verify live. Never guess.** Provider documentation is wrong often enough that
this is the project's defining discipline. Three real examples: TorBox's docs
claim comma-separated hashes for check-cached (repeated params is what actually
works), AllDebrid's docs misdescribe their own response shape, and TorBox has an
undocumented `cooldown_until` account restriction that looks exactly like a bug
in our polling. Every one was found by making a real call.

**Every fix gets a failing test first, then a mutation check.** Write the test,
watch it fail, fix it, then revert *only the fix* and confirm the test fails
again. A test that passes either way protects nothing. This has caught bad fixes
repeatedly — including one where the test I wrote alongside a detector proved the
detector wrong before it shipped.

**Report what actually happened.** If a check was skipped, say so. If a claim is
reasoning rather than observation, mark it. "Local rows: 0" is *not* proof the
provider is clean — a provider-side dedupe returns 200 with no local row, so
things can sit in the account with nothing tracking them.

**Prefer pinning to changing.** Several behaviours here are genuinely ambiguous
(an uppercase A–F-only MD5 is indistinguishable from base32; the same infohash
with two display names). Those are tests that record the decision, not bugs.

---

## The environment

- Source lives on Windows at `c:\Code\AcerviNode`; it builds and runs under WSL
  at `/mnt/c/Code/AcerviNode`.
- Go is not on the default PATH in a non-login shell:
  `export PATH=$PATH:/usr/local/go/bin`.
- The running instance is a **systemd service** on `:7846`, binary at
  `/opt/acervinode/acervinode`. `sudo` needs a password — **ask the user for it,
  and never write it, an API key, or a provider token into a file.** Credentials
  live in the instance's own `config.yaml`.

**Deploy sequence — the frontend must be built first**, because the Go binary
embeds it:

```sh
cd web && npm run build            # embedded by web/webui.go
cd .. && go build -o /tmp/acervinode ./cmd/acervinode
sudo systemctl stop acervinode
sudo install -m 0755 /tmp/acervinode /opt/acervinode/acervinode
sudo systemctl start acervinode
```

`systemctl restart` alone will not pick up a new binary if you only ran
`go build ./...` — that compiles without producing the artifact that gets
installed.

---

## Traps that have actually cost time

**A shipped UI change can look absent for two different reasons.** `index.html`
is served `no-cache` and `assets/*` `immutable`, so a stale bundle is usually
*not* the cause. The two real causes have been: the edit silently not applying,
and **CSS specificity** (`.settings-card button` beat `.link-button`). Verify by
grepping the *source*, then the *served bundle*, with a string unique to the
change — grepping a non-unique string has produced false confirmation twice.

**Editing with Python patch scripts: assert in both directions.** Confirm the new
text is present *and* the old text is gone. Match on trimmed line content rather
than exact indentation — indentation in this repo is inconsistent in places, and
an unasserted `str.replace` that silently no-ops is the single most common way
work here goes wrong.

**Bash heredocs eat backslashes.** `\\n` inside `<<'PYEOF'` has repeatedly become
a real newline, producing unterminated string literals. For any patch containing
backslashes, either write the script to a file first or build the backslash with
`chr(92)`.

**Rate-limit backoff blanks polling for a whole kind**, which presents as the UI
freezing rather than as an error. Check `GET /api/v1/status` before diagnosing a
"stuck" download.

**Provider-side state outlives local rows.** Deleting a download locally does not
always remove it at the provider, and the poller will re-discover it. After any
bulk test, verify the provider is clean, not just the database.

---

## Testing

```sh
cd web && npx tsc -b && npm run lint && npx vitest run   # 244 tests, 4 suites
go build ./... && go vet ./... && go test ./internal/... ./cmd/... -timeout 600s
```

Always pass `-timeout 600s` to Go tests and never wrap the run in an outer
`timeout` — doing so has produced a falsely green report.

The four frontend suites are example-based, **property/fuzz**, **boundary**, and
**adversarial**. The property sweep is the highest-yield: 12 invariants over
thousands of generated inputs, and it found two idempotence bugs nothing
hand-written would have. Run it deep with
`npx vitest run detect.property --testTimeout=180000`.

When a fuzz test fails, **suspect the generator first.** Two of its first
failures were harness bugs that read exactly like product bugs.

---

## Deliberate non-goals

Do not propose these; they are settled decisions, not oversights.

- **No Docker image, no Windows packaging.** Linux tarball + systemd.
- **No mount, nothing served.** Everything is fetched to local disk. This is the
  biggest difference from decypharr and it is a choice — see `ROADMAP.md` for the
  avenues if it is ever revisited.
- **Real-Debrid, Premiumize and Debrid-Link are unwritten** purely because no
  account exists to verify against. Writing them from documentation alone would
  produce three plausible implementations and no idea which work.
