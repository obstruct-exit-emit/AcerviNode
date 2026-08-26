# AcerviNode documentation

AcerviNode is a debrid download client: it presents itself to Sonarr, Radarr, and
[LibriNode](https://github.com/obstruct-exit-emit/LibriNode) as a normal qBittorrent
or SABnzbd download client, then resolves everything it's handed through a debrid
provider instead of doing real P2P or NNTP work.

**Supported providers: TorBox and AllDebrid**, with any number of accounts on
each — see [Providers](providers.md). Other debrid services are named here only
as examples of the category, not as things AcerviNode talks to today.

- **New here?** Start with [Installation](installation.md), then
  [Quickstart](quickstart.md).
- **Configuring it?** See [Configuration](configuration.md).
- **Wondering how the *arr integration works?** See
  [qBittorrent API](qbittorrent-api.md) and [SABnzbd API](sabnzbd-api.md).
- **Adding a provider or contributing code?** See [Providers](providers.md) and
  [Development](development.md).
- **What's built, what's not?** See the [Roadmap](../ROADMAP.md).

## Why it exists

*arr apps only know how to talk to download clients that speak a protocol they
recognize — qBittorrent's Web API or SABnzbd's API, among others. Debrid services
speak neither. AcerviNode is the translation layer:
it implements those two client protocols for real, and behind them, calls a debrid
provider's actual API to add, track, and resolve downloads. Your \*arr app never
needs to know the difference.

This is the same trick [decypharr](https://github.com/sirrobot01/decypharr) uses.
AcerviNode is a from-scratch alternative: one static Go binary, an embedded SQLite
store, and no Docker or FUSE requirement anywhere — including Completed Download
Handling, which fetches finished downloads to local disk over plain HTTP rather
than mounting anything. That's the whole stack, not just the compat shims, which
is why it builds and runs on Windows as readily as on Linux — though the
packaged, supported deployment is Linux with systemd, and there is no Windows
build published.
