# Installation

## From a release (Linux amd64/arm64)

Grab a `.tar.gz` from [Releases](https://github.com/obstruct-exit-emit/AcerviNode/releases)
— it contains the `acervinode` binary (single self-contained file, UI included)
plus a [systemd unit](../packaging/acervinode.service).

```sh
tar -xzf acervinode-v*-linux-amd64.tar.gz   # or -linux-arm64
sudo install -m 755 acervinode-*/acervinode /opt/acervinode/acervinode
sudo useradd -r -s /usr/sbin/nologin acervinode
sudo mkdir -p /etc/acervinode /var/lib/acervinode
sudo chown acervinode:acervinode /var/lib/acervinode
sudo install -m 644 acervinode-*/acervinode.service /etc/systemd/system/acervinode.service
```

Create `/etc/acervinode/config.yaml` (see [Configuration](configuration.md)), then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now acervinode
```

### Upgrading an existing install

Replacing `/opt/acervinode/acervinode` with a newer binary and restarting the
service is normally all that's needed. The one exception: a binary update never
touches an already-installed `/etc/systemd/system/acervinode.service` — if a
release's own copy of that file changes (as it did to pick up `Restart=always`,
needed for the settings UI's "Restart now" action to actually work), re-copy it
and reload systemd once:

```sh
sudo install -m 644 acervinode-*/acervinode.service /etc/systemd/system/acervinode.service
sudo systemctl daemon-reload
sudo systemctl restart acervinode
```

## From source

Requirements: Go 1.25+, Node 22+ (for the frontend).

```sh
git clone https://github.com/obstruct-exit-emit/AcerviNode.git
cd AcerviNode
cd web && npm install && npm run build && cd ..
go build ./cmd/acervinode
```

This produces a single `acervinode` binary (`acervinode.exe` on Windows, for local
development — production deployment targets Linux, see below). See
[Development](development.md) for the full build/test loop.

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

Every directory AcerviNode creates under `download_dir` is group-writable
(`0775`) specifically so a Managed download's completed-import step can
move or hardlink files out of it — real SABnzbd's own "completed" reporting
always tells an *arr app it's safe to move a file, and Sonarr/Radarr take
it up on that (see [Providers](providers.md#completed-download-handling-internalimporter)
for the full story, including a real bug this was found fixing: NZB
imports failing outright with "Access ... is denied"). That only actually
works if your *arr app's own user is a member of AcerviNode's group
(`acervinode` by default) — otherwise it's still only writable by
AcerviNode itself, the same problem this section exists to avoid.

- **Native install** (Sonarr/Radarr also run as a plain system user):
  `sudo usermod -aG acervinode <that user>`, then restart the *arr app's
  own service so the new group membership takes effect.
- **Docker** (the common case — AcerviNode itself isn't packaged as a
  Docker image, but most *arr apps are): find AcerviNode's GID
  (`getent group acervinode`) and add it to the container, e.g.
  `docker run --group-add <gid> ...` (or the equivalent `group_add:` entry
  in a Compose file), alongside whatever bind mount gives the container
  access to `download_dir` in the first place.

A directory created before this fix existed is corrected automatically
the next time anything is fetched into it — no manual `chmod` needed for
those.

## Windows note

The backend is plain Go and builds fine on Windows for local development — there's
no packaged Windows install target (not currently planned, see the
[roadmap](../ROADMAP.md)).
