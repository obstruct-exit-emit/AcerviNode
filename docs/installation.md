# Installation

## From a release (Linux amd64/arm64)

Grab a `.tar.gz` from [Releases](https://github.com/obstruct-exit-emit/AcerviNode/releases)
— it contains the `acervinode` binary (single self-contained file, UI included)
plus a [systemd unit](../packaging/acervinode.service).

```sh
tar -xzf acervinode-v*-linux-amd64.tar.gz   # or -linux-arm64
sudo useradd -r -s /usr/sbin/nologin acervinode
sudo mkdir -p /opt/acervinode /etc/acervinode /var/lib/acervinode
sudo install -m 755 acervinode-*/acervinode /opt/acervinode/acervinode
sudo chown acervinode:acervinode /var/lib/acervinode
sudo install -m 644 acervinode-*/acervinode.service /etc/systemd/system/acervinode.service
```

Create `/etc/acervinode/config.yaml` (see [Configuration](configuration.md)) and
make it writable by the service account:

```sh
sudo chown acervinode:acervinode /etc/acervinode/config.yaml
sudo chmod 600 /etc/acervinode/config.yaml
```

That ownership isn't optional. AcerviNode persists every live settings change
by rewriting this file — the TorBox API key entered in the Settings UI, a
category's save-path override, a changed poll interval — and it writes during
an ordinary startup too, when the one-time default-category seeding runs
before the server is even listening. A `config.yaml` the service can't write
makes it exit non-zero having served nothing, which `Restart=always` turns
into a restart loop. Then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now acervinode
```

### Upgrading an existing install

Replacing `/opt/acervinode/acervinode` with a newer binary and restarting the
service is normally all that's needed. The one exception: a binary update never
touches an already-installed `/etc/systemd/system/acervinode.service` — if a
release's own copy of that file changes (as it did to pick up `Restart=always`,
needed for the settings UI's "Restart now" action to actually work, and again
to add `/etc/acervinode` to `ReadWritePaths` — without which the service
can't rewrite its own `config.yaml` and won't start at all), re-copy it and
reload systemd once:

```sh
sudo install -m 644 acervinode-*/acervinode.service /etc/systemd/system/acervinode.service
sudo systemctl daemon-reload
sudo systemctl restart acervinode
```

## From source

Requirements: Go 1.25+, Node 22+ (for the frontend), `make` (or run the two
commands it wraps directly — see below).

```sh
git clone https://github.com/obstruct-exit-emit/AcerviNode.git
cd AcerviNode
make build
```

This produces a single `acervinode` binary (`acervinode.exe` on Windows, for local
development — production deployment targets Linux, see below). See
[Development](development.md) for the full build/test loop.

`make build` is exactly equivalent to running these two commands, in order:

```sh
cd web && npm ci && npm run build && cd ..
go build ./cmd/acervinode
```

### Updating an existing from-source install

**Always rebuild the frontend before the backend, every time — including
an update, not just the first build.** `go:embed` bakes in whatever's
already on disk in `web/dist` at the moment `go build` runs, not what's in
git — the actual built frontend files are gitignored (only a placeholder
`web/dist/.gitkeep` is committed), so `git pull` alone never updates them.
A plain `git pull && go build`, skipping the frontend step, compiles and
runs *successfully* — there's no error — it just serves whatever UI
happened to already be sitting in `web/dist` from an earlier build, with
nothing to indicate anything's stale. Found live, updating a real
deployment this way.

```sh
git pull
make build
sudo systemctl restart acervinode   # or however this install is run
```

`make build` closes the gap above by always doing both steps as one
command, so there's no longer a multi-step sequence an update can partially
skip. If you don't have `make` available, just run the two commands from
the section above yourself, in that order, every time.

If you're building on a machine that deliberately has no Node.js (e.g. a
minimal production box) — build the full thing elsewhere (a dev machine,
or even a throwaway container) and copy over just the resulting
`acervinode` binary; nothing else about the binary depends on where it was
built. `make build-backend-only` is the equivalent shortcut for a machine
that already has an up-to-date `web/dist` some other way and just needs
the Go side rebuilt — skips the frontend step entirely, embedding whatever
is currently on disk as-is. It's a separate, explicitly-named target on
purpose: skipping the frontend has to be typed on purpose, not stumbled
into via a forgotten step.

## Running it

```sh
./acervinode
```

By default AcerviNode binds `:7846` and looks for `config.yaml` in the working
directory (see [Configuration](configuration.md) for every key). At minimum you'll
want a TorBox API key set before the provider comes up.

## Linux deployment

AcerviNode targets Linux for real deployments, the same as LibriNode: a single
binary, run under systemd (see the release steps above, or
[packaging/acervinode.service](../packaging/acervinode.service) directly if
building from source). The unit is hardened (`ProtectSystem=strict`,
`NoNewPrivileges`, a dedicated `acervinode` user) with write access scoped to
`/var/lib/acervinode`.

### A custom `download_dir` needs its own `ReadWritePaths`

If `download_dir` (see [Configuration](configuration.md)) points outside
`/var/lib/acervinode` — the common case for routing downloads to a separate
disk or an existing media-library mount — the packaged unit's
`ReadWritePaths=/var/lib/acervinode` won't let AcerviNode write there at
all under `ProtectSystem=strict`. Add the path explicitly:

```
ReadWritePaths=/var/lib/acervinode /path/to/your/download_dir
```

in `/etc/systemd/system/acervinode.service`, then `sudo systemctl daemon-reload
&& sudo systemctl restart acervinode`.

### Letting Sonarr/Radarr move files out of `download_dir`

Every directory AcerviNode creates under `download_dir` is world-writable
(`0777`) specifically so a Managed download's completed-import step can
move or hardlink files out of it — real SABnzbd's own "completed" reporting
always tells an *arr app it's safe to move a file, and Sonarr/Radarr take
it up on that (see [Providers](providers.md#completed-download-handling-internalimporter)
for the full story, including a real bug this was found fixing: NZB
imports failing outright with "Access ... is denied").

World-writable, not just writable by AcerviNode's own user, is deliberate:
an *arr app almost never runs as the same user AcerviNode does — most
setups have it in a separate Docker container with its own PUID/PGID, and
even matching group IDs across genuinely separate deployments is real,
ongoing coordination (worse under Proxmox/NAS setups with LXC UID-namespace
remapping). The standard self-hosted-media-stack answer to this — giving
every container the same PUID/PGID, the convention
[rdt-client](https://github.com/rogerfar/rdt-client)'s own Docker image
uses — isn't something AcerviNode can ask of apps it doesn't package itself.
This is the zero-configuration equivalent: nothing to set up on a fresh
install, and it only loosens these specific per-download directories,
nothing else AcerviNode manages. A directory created before this fix
existed is corrected automatically the next time anything is fetched into
it — no manual `chmod` needed for those.

If you'd rather not have these directories world-writable (e.g. a
genuinely multi-tenant box), the alternative is making AcerviNode run as
the *same* user/group your `download_dir` and the rest of your media stack
already use — override `User=`/`Group=` in
`/etc/systemd/system/acervinode.service` (`sudo systemctl edit acervinode`),
matching whatever UID/GID your Docker containers' own `PUID`/`PGID` are set
to. AcerviNode doesn't need anything special for this — plain systemd
identity, same as any other service.

## Backups and restore

AcerviNode keeps everything it knows in one SQLite file — configuration,
download history, categories, login accounts and sessions. It snapshots that
file to `<data_dir>/backups` every `backup_interval_hours` (24 by default),
keeping the newest `backup_keep` (7 by default). Under the packaged systemd
unit that resolves to `/var/lib/acervinode/backups`, which the unit's
`ReadWritePaths` already covers.

Snapshots are taken with SQLite's `VACUUM INTO`, so they are consistent
against a running database — no need to stop the service to take one — and
each is a self-contained, already-compacted file with no `-wal`/`-shm`
sidecars to keep alongside it.

Take one on demand from **Settings → Backup**, or:

```bash
curl -X POST http://localhost:7846/api/v1/settings/backups   -H "Authorization: Bearer $ACERVINODE_API_KEY"
```

**Restoring is deliberately manual.** There is no restore endpoint: putting a
different database under a running process is not something to trigger from a
web button by accident, and the correct sequence involves stopping the service
anyway.

```bash
sudo systemctl stop acervinode
# Keep the current file rather than overwriting it outright.
sudo mv /var/lib/acervinode/acervinode.db /var/lib/acervinode/acervinode.db.bak
sudo cp /var/lib/acervinode/backups/acervinode-20260824-001259.db         /var/lib/acervinode/acervinode.db
sudo chown acervinode:acervinode /var/lib/acervinode/acervinode.db
sudo systemctl start acervinode
```

Two things worth knowing before you do:

- **The snapshot carries its own accounts and sessions.** Restoring an older
  one restores the logins as they were then — a user added since will be gone,
  and a password changed since reverts. Treat a snapshot as the credential
  material it is when copying it off the box.
- **`config.yaml` is a separate file** and is not part of the snapshot. A
  restore brings back the database, not `/etc/acervinode/config.yaml`; back
  that up alongside it if you want the pair.

Copy snapshots somewhere off the machine if they are meant to survive losing
it — the API lists names and sizes but deliberately never serves a snapshot's
contents, precisely because of what it holds.

## Windows note

The backend is plain Go and builds fine on Windows for local development — there's
no packaged Windows install target (not currently planned, see the
[roadmap](../ROADMAP.md)).
