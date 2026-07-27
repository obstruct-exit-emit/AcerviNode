# Configuration

AcerviNode reads `config.yaml` from its working directory (override the path with
`ACERVINODE_CONFIG`). Every key can also be set via an `ACERVINODE_*` environment
variable, which takes precedence over the file — useful for containers and systemd
units without a mounted config file.

| Key | Env var | Default | Purpose |
|---|---|---|---|
| `port` | `ACERVINODE_PORT` | `7846` | HTTP port for both compat shims and the native API |
| `data_dir` | `ACERVINODE_DATA_DIR` | `./data` | Where the SQLite database file lives |
| `api_key` | `ACERVINODE_API_KEY` | *(generated on first run)* | Key required by the native `/api/v1` endpoints, the qBittorrent shim's login password, and the SABnzbd shim's `apikey` param. Viewable and regeneratable live (no restart) via the web UI's Settings tab, or `GET`/`POST /api/v1/settings/general` and `/api/v1/settings/api-key/regenerate` — see [API](api.md) |
| `log_level` | `ACERVINODE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `download_dir` | `ACERVINODE_DOWNLOAD_DIR` | `./downloads` | Fallback destination for Completed Download Handling when the *arr app didn't supply its own `save_path` |
| `import_interval_seconds` | `ACERVINODE_IMPORT_INTERVAL_SECONDS` | `10` | How often `internal/importer` checks for provider-completed downloads to fetch to local disk; also the base of its retry backoff (attempt *N* waits ~`import_interval_seconds`×2^*N*, capped at 1 hour) |
| `import_max_retries` | `ACERVINODE_IMPORT_MAX_RETRIES` | `5` | How many failed fetch attempts a download gets before `internal/importer` gives up and moves it to `error` instead of retrying again |
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

## Regenerating the API key

`POST /api/v1/settings/api-key/regenerate` (the Settings tab's "Regenerate API
key" button) replaces `api_key` with a fresh random one and applies it
immediately — every authenticated route, both compat shims included, stops
accepting the old key right away. This means every *arr app configured against
AcerviNode's qBittorrent/SABnzbd shims needs its download client password/API
key updated to match, and any other browser tab/session using the old key gets
logged out. The web UI's own session updates itself automatically since it's
the one that triggered the change; nothing else does.

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
