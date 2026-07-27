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
| `providers.torbox.api_key` | `ACERVINODE_PROVIDERS_TORBOX_API_KEY` | *(none — required to enable TorBox)* | Bearer token used for every TorBox API call |

## Example

```yaml
port: 7846
data_dir: /var/lib/acervinode
log_level: info
providers:
  torbox:
    api_key: "your-torbox-api-key"
```

## Provider config shape

`providers` is a map keyed by provider name (`torbox` today; `realdebrid` and
others land here as they're implemented — see [Providers](providers.md) and the
[roadmap](../ROADMAP.md)). AcerviNode only mounts the SABnzbd compat shim if the
configured provider implements the usenet interface — TorBox does, so both shims
come up; a future torrent-only provider would leave the SABnzbd shim unmounted
rather than erroring.

## Categories and save paths

*arr apps set a category on every add (`tv-sonarr`, `radarr`, ...) purely so they
know which import path to watch. AcerviNode stores whatever category and save path
the calling app sends and echoes it straight back — there's no category-to-path
mapping to configure on AcerviNode's side.
