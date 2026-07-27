# Configuration

AcerviNode reads `config.yaml` from its working directory (override the path with
`ACERVINODE_CONFIG`). Every key can also be set via an `ACERVINODE_*` environment
variable, which takes precedence over the file — useful for containers and systemd
units without a mounted config file.

| Key | Env var | Default | Purpose |
|---|---|---|---|
| `port` | `ACERVINODE_PORT` | `7846` | HTTP port for both compat shims and the native API |
| `data_dir` | `ACERVINODE_DATA_DIR` | `./data` | Where the SQLite database file lives |
| `api_key` | `ACERVINODE_API_KEY` | *(generated on first run)* | Key required by the native `/api/v1` endpoints, and by default the key the SABnzbd shim's `apikey` param is checked against |
| `log_level` | `ACERVINODE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `download_dir` | `ACERVINODE_DOWNLOAD_DIR` | `./downloads` | Fallback destination for Completed Download Handling when the *arr app didn't supply its own `save_path` |
| `import_interval_seconds` | `ACERVINODE_IMPORT_INTERVAL_SECONDS` | `10` | How often `internal/importer` checks for provider-completed downloads to fetch to local disk |
| `providers.torbox.api_key` | `ACERVINODE_PROVIDERS_TORBOX_API_KEY` | *(none — required to enable TorBox)* | Bearer token used for every TorBox API call. Can also be set (or changed) without a restart via the web UI's Settings tab, or `PUT /api/v1/settings/providers/torbox` — see [API](api.md) and [Providers](providers.md#live-settings) |

## Example

```yaml
port: 7846
data_dir: /var/lib/acervinode
download_dir: /var/lib/acervinode/downloads
log_level: info
providers:
  torbox:
    api_key: "your-torbox-api-key"
```

## Provider config shape

`providers` is a map keyed by provider name (`torbox` today; `realdebrid` and
others land here as they're implemented — see [Providers](providers.md) and the
[roadmap](../ROADMAP.md)). Both compat shims are always mounted, whether or not a
provider is configured yet — see [Providers](providers.md#live-settings) for why
that's what makes setting a key through the web UI (rather than only at startup)
possible at all.

## Categories and save paths

*arr apps set a category on every add (`tv-sonarr`, `radarr`, ...) and, for the
qBittorrent shim, generally rely on the category's own configured path rather than
sending an explicit `save_path`. AcerviNode stores whatever category and save path
the calling app does send, and Completed Download Handling
([Providers](providers.md#completed-download-handling)) writes fetched files there
— falling back to `download_dir` (organized as `<download_dir>/<category>/<name>/`)
when no save path was supplied. There's no category-to-path mapping to configure
separately on AcerviNode's side; whatever path arrives with the add request (or the
fallback) is where files land.
