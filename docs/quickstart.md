# Quickstart

This walks through running AcerviNode with a real [TorBox](https://torbox.app) API
key and pointing Sonarr at it — once as a qBittorrent client, once as a SABnzbd
client. Either path exercises the full add → track → resolve flow through TorBox.
This is also the manual verification procedure for this vertical slice: there's no
CI account, so this is how it gets proven against a live provider.

## 1. Get a TorBox API key

Sign in at [torbox.app](https://torbox.app), copy your API key from account
settings.

## 2. Configure and run AcerviNode

Create `config.yaml` next to the binary (see [Configuration](configuration.md) for
every key) — the TorBox key can go here, or be added after startup through the web
UI instead (see [step 4](#4-the-web-ui) below):

```yaml
port: 7846
api_key: "<pick any string — this is what Sonarr will authenticate with>"
providers:
  torbox:
    api_key: "<your TorBox API key, optional here — see below>"
```

Setting `api_key` explicitly (rather than leaving it to be auto-generated) means
it survives restarts, so you don't have to re-enter it in Sonarr every time.

```sh
./acervinode
```

Confirm it logs that it bound `:7846` — the startup log also prints the effective
`api_key` if you didn't set one, so it's never a mystery value, and logs whether a
TorBox provider is already configured.

## 3a. Point Sonarr at it as a qBittorrent client

Settings → Download Clients → Add → qBittorrent:

- Host: `localhost` (or the AcerviNode host), Port: `7846`
- Username: anything (not checked) — Password: your `api_key`
- Category: whatever you'd normally use (e.g. `tv-sonarr`)
- Click **Test** — this exercises AcerviNode's `/api/v2/auth/login` and
  `/api/v2/app/webapiVersion` handlers and should succeed

Grab a release. In AcerviNode's SQLite `downloads` table you should see a new row
with `kind = 'torrent'` and a `provider_download_id` set once TorBox accepts the
add. Progress should advance as TorBox's own torrent list reports it, and once
TorBox reports it done, `internal/importer` fetches the actual files to
`save_path` (or `download_dir`) within one `import_interval_seconds` tick — check
there directly, or wait for Sonarr's own import step to find them.

## 3b. Point Sonarr at it as a SABnzbd client

Settings → Download Clients → Add → SABnzbd:

- Host: `localhost` (or the AcerviNode host), Port: `7846`
- API Key: any value you configured AcerviNode to accept (see
  [Configuration](configuration.md))
- Click **Test** — this exercises AcerviNode's `mode=version` and API-key check
  and should succeed

Grab an NZB-eligible release. You should see a `downloads` row with
`kind = 'usenet'`, tracked through TorBox's usenet service the same way, with files
fetched to disk by `internal/importer` the same way once TorBox reports it done.

## 4. The web UI

Open `http://localhost:7846` in a browser. On a fresh instance you get the
**first-run setup wizard**: pick a username and password, and that becomes the
Default admin account. Logging in is the only way into the web UI — there's no
API-key-only path, and pasting `api_key` won't get you in. (The API key is for
Sonarr/Radarr and scripts, which can't do cookie logins; it keeps working
exactly as before and is unaffected by any of this.)

A provider key already in `config.yaml` does **not** skip the wizard — an
instance with a key but no account is still a fresh install.

Once in, watch the same downloads update live — same data as `curl`-ing
[`/api/v1/downloads`](api.md), just rendered. The **Managed** tab is what you
just added through Sonarr; **Manual** is for anything added directly (a
"+ Add" button, or added straight through the provider's own site/app — it
shows up here automatically too, see
[Providers](providers.md#managed-vs-manual)).

If you didn't add a TorBox key to `config.yaml`, do it here instead: the
**Settings** tab has a field for it. Saving takes effect immediately — no
restart, and both compat shims (already mounted, whether or not a provider was
configured yet) start working against real TorBox calls right away. See
[Providers](providers.md#live-settings) for how that works.

## Adding a second provider

Multi-provider support has shipped, so TorBox isn't the only option. AllDebrid
works the same way: **Settings → Providers** has a card for every provider this
build supports, whether or not it's configured yet — paste a key and hit Test.

Two things worth knowing:

- **AllDebrid has no usenet service.** It handles torrents and hoster links
  only, and simply never appears as an option for a usenet add. Keep TorBox
  configured if you want usenet.
- **One service can hold two accounts.** Each provider card has an "Add another
  account" action; the entries are fully independent, right down to separate
  rate-limit backoff, so one account being throttled doesn't slow the other.

`default_provider` picks where a new download goes when nothing says
otherwise, and it resolves per kind — naming a torrent-only provider as the
default won't break usenet adds, which fall through to a provider that
supports them. See [Configuration](configuration.md#provider-config-shape) and
[Providers](providers.md#multiple-providers-debridregistry).

Real-Debrid and others remain unimplemented — see the
[roadmap](../ROADMAP.md).
