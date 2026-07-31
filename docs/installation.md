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

## Windows note

The backend is plain Go and builds fine on Windows for local development — there's
no packaged Windows install target (not currently planned, see the
[roadmap](../ROADMAP.md)).
