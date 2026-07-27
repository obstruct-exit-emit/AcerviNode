# Installation

AcerviNode is pre-1.0 and not yet publishing binary releases — build it from
source. Binary releases (Linux amd64/arm64, with a systemd unit) are tracked in the
[roadmap](../ROADMAP.md), Phase 5.

## From source

Requirements: Go 1.25+.

```sh
git clone https://github.com/acervinode/acervinode.git
cd acervinode
go build ./cmd/acervinode
```

This produces a single `acervinode` binary (`acervinode.exe` on Windows, for local
development — production deployment targets Linux, see below).

## Running it

```sh
./acervinode
```

By default AcerviNode binds `:7846` and looks for `config.yaml` in the working
directory (see [Configuration](configuration.md) for every key). At minimum you'll
want a TorBox API key set before the provider comes up.

## Linux deployment

AcerviNode targets Linux for real deployments, the same as LibriNode: a single
binary, run under systemd. A packaged systemd unit will ship alongside the first
tagged release (Phase 5 on the [roadmap](../ROADMAP.md)); until then, a minimal unit
file looks like:

```ini
[Unit]
Description=AcerviNode
After=network.target

[Service]
ExecStart=/opt/acervinode/acervinode
WorkingDirectory=/opt/acervinode
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Windows note

The backend is plain Go and builds fine on Windows for local development — there's
no official Windows deployment target yet, same posture as LibriNode.
