// Thin client for AcerviNode's native /api/v1. No generated client, no
// fetch-wrapper library — this is small enough to just write directly.

export interface ProviderStatus {
  name: string
  torrent_capable: boolean
  usenet_capable: boolean
  webdl_capable: boolean
}

export interface Download {
  id: string
  provider: string
  protocol: 'torrent' | 'usenet' | 'webdl'
  hash?: string
  name: string
  category?: string
  save_path?: string
  size_bytes: number
  state: 'queued' | 'downloading' | 'provider_completed' | 'ready_for_import' | 'error'
  progress: number
  added_at: string
  updated_at: string
  completed_at?: string
  // cached_at is when the provider first reported this download done —
  // distinct from completed_at (files actually on local disk): a Manual
  // download that's never fetched has cached_at set but completed_at stays
  // unset forever.
  cached_at?: string
  error_message?: string
  retry_count?: number
  next_retry_at?: string
  // has_source reports whether reAddDownload could actually resubmit this
  // download — false for one added via an uploaded file, or discovered
  // (adopted from the provider's own account with no original link ever
  // known). Not scoped to a particular added_via; Re-add works for any kind
  // as long as a source link is stored.
  has_source: boolean
  // added_via is permanent from the moment a download is added — "arr" (via
  // the qBittorrent/SABnzbd compat shim, auto-fetched to local disk) or
  // "manual" (added directly, or discovered already sitting in the
  // provider's own account — never auto-fetched). What the Managed/Manual
  // tabs filter on.
  added_via: 'arr' | 'manual'
  // eta_seconds/seeders/leechers/download_speed_bytes/phase are fast-moving,
  // provider-reported fields that are never persisted server-side — read
  // fresh from the backend's own in-memory cache on every poll, so they can
  // lag slightly behind the very latest provider state (whatever the last
  // background poll happened to see), same as progress/state themselves
  // can. seeders/leechers/download_speed_bytes are torrent-only (always 0
  // for usenet/webdl, which have no BitTorrent-swarm concept); phase is
  // usenet-only ("verifying"/"repairing"/"extracting", or "" for plain
  // transfer/no concept). All default to 0/"" before anything's polled a
  // download yet.
  eta_seconds: number
  seeders: number
  leechers: number
  download_speed_bytes: number
  phase?: string
  // airlocked reports whether the provider is keeping this download in
  // permanent storage, exempt from the retention policy that would
  // otherwise eventually remove it (TorBox calls this AirLock). Same
  // never-persisted treatment as the fields above, so it reads false until
  // the download has actually been polled once. Set from the provider's
  // own side, never by AcerviNode.
  airlocked: boolean
}

export interface DownloadFile {
  path: string
  size_bytes: number
  provider_file_id?: string
}

export interface DownloadDetail extends Download {
  files: DownloadFile[]
  // Set only when files came back empty *because* the live provider query
  // failed — e.g. the provider genuinely no longer has this download at all
  // (deleted directly through its own site). Distinguishes that from the
  // ordinary "not processed yet" case, which also has an empty files array
  // but no files_error.
  files_error?: string
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

// request is used two ways: with a real apiKey (the master credential, sent
// as Authorization: Bearer — always works, restart-proof, what Sonarr/
// Radarr always use since they can't do cookie logins), or with an empty
// string for a signed-in login session instead — fetch() already sends the
// session cookie automatically for a same-origin request with no special
// config needed, and the backend falls through to checking it whenever the
// Authorization header doesn't match the real key (see internal/api's
// currentRole), so simply omitting the header here is enough.
async function request<T>(path: string, apiKey: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    ...init,
    headers: apiKey ? { ...init?.headers, Authorization: `Bearer ${apiKey}` } : init?.headers,
  })
  if (!resp.ok) {
    const text = await resp.text().catch(() => '')
    throw new ApiError(resp.status, text || `${resp.status} ${resp.statusText}`)
  }
  if (resp.status === 204) return undefined as T
  return (await resp.json()) as T
}

export function getVersion(apiKey: string): Promise<{ version: string }> {
  return request('/api/v1/version', apiKey)
}

export function getProviders(apiKey: string): Promise<ProviderStatus[]> {
  return request('/api/v1/providers', apiKey)
}

// listDownloads optionally filters to just the Managed ("arr") or Manual
// tab's downloads — omit addedVia for everything, unfiltered.
export function listDownloads(apiKey: string, addedVia?: 'arr' | 'manual'): Promise<Download[]> {
  const query = addedVia ? `?added_via=${addedVia}` : ''
  return request(`/api/v1/downloads${query}`, apiKey)
}

export function getDownload(apiKey: string, id: string): Promise<DownloadDetail> {
  return request(`/api/v1/downloads/${encodeURIComponent(id)}`, apiKey)
}

// getFileLink resolves a direct, provider-hosted download URL for one file
// — resolved fresh on every call (not cached), matching what
// internal/importer itself does when fetching a file to local disk.
export function getFileLink(apiKey: string, downloadId: string, providerFileId: string): Promise<{ url: string }> {
  return request(
    `/api/v1/downloads/${encodeURIComponent(downloadId)}/files/${encodeURIComponent(providerFileId)}/link`,
    apiKey,
  )
}

// getZipLink resolves one URL for every file in a download at once, zipped
// provider-side — an explicit opt-in alternative to downloading files
// individually (see getFileLink and DownloadsTable's per-row "Download all",
// which downloads files individually by default).
export function getZipLink(apiKey: string, downloadId: string): Promise<{ url: string }> {
  return request(`/api/v1/downloads/${encodeURIComponent(downloadId)}/zip-link`, apiKey)
}

export function deleteDownload(apiKey: string, id: string, deleteFiles: boolean): Promise<void> {
  return request(`/api/v1/downloads/${encodeURIComponent(id)}?deleteFiles=${deleteFiles}`, apiKey, {
    method: 'DELETE',
  })
}

// retryDownload resets a download that gave up (state error) back to
// provider_completed with retry_count cleared, so internal/importer's very
// next tick attempts the fetch again — only valid while the download is
// actually in the error state.
export function retryDownload(apiKey: string, id: string): Promise<Download> {
  return request(`/api/v1/downloads/${encodeURIComponent(id)}/retry`, apiKey, { method: 'POST' })
}

// reAddDownload resubmits a download's original source (magnet/NZB URL) to
// the provider as a brand new add — for when retryDownload alone isn't
// enough because the original provider-side download is gone entirely, not
// just a transient fetch failure. Only valid in error state, and only if the
// download was originally added via a link rather than an uploaded file.
export function reAddDownload(apiKey: string, id: string): Promise<Download> {
  return request(`/api/v1/downloads/${encodeURIComponent(id)}/readd`, apiKey, { method: 'POST' })
}

// AddedVia mirrors the backend's database.AddedVia — "arr" only takes
// effect for an admin (see resolveAddedVia in internal/api/add.go); omitted
// or "manual" keeps the existing default (never auto-fetched, no *arr
// pipeline involvement).
export type AddedVia = 'arr' | 'manual'

export function addTorrent(
  apiKey: string,
  input: ({ magnet: string } | { file: File }) & { category?: string; addedVia?: AddedVia; provider?: string },
): Promise<Download> {
  const form = new FormData()
  if ('magnet' in input) form.set('magnet', input.magnet)
  else form.set('file', input.file)
  if (input.category) form.set('category', input.category)
  if (input.addedVia) form.set('added_via', input.addedVia)
  // Omitted rather than sent empty when unset: the server treats an absent
  // provider as "use the default", and an empty string as a name to look up.
  if (input.provider) form.set('provider', input.provider)
  // No Content-Type header here on purpose — the browser sets
  // multipart/form-data with the correct boundary itself when the body is a
  // FormData; setting it manually would drop the boundary parameter.
  return request('/api/v1/downloads/torrent', apiKey, { method: 'POST', body: form })
}

export function addUsenet(
  apiKey: string,
  input: ({ url: string } | { file: File }) & { category?: string; addedVia?: AddedVia; provider?: string },
): Promise<Download> {
  const form = new FormData()
  if ('url' in input) form.set('url', input.url)
  else form.set('file', input.file)
  if (input.category) form.set('category', input.category)
  if (input.addedVia) form.set('added_via', input.addedVia)
  // Omitted rather than sent empty when unset: the server treats an absent
  // provider as "use the default", and an empty string as a name to look up.
  if (input.provider) form.set('provider', input.provider)
  return request('/api/v1/downloads/usenet', apiKey, { method: 'POST', body: form })
}

// addWebDownload submits a direct hoster link (Mega, 1Fichier, Mediafire, and
// ~160 others TorBox's Web Downloads service supports) — link-only, unlike
// addTorrent/addUsenet: there's no file-upload variant for this endpoint
// (TorBox's own createwebdownload API has none either).
export function addWebDownload(apiKey: string, input: { link: string; category?: string; addedVia?: AddedVia; provider?: string }): Promise<Download> {
  const body = new URLSearchParams()
  body.set('link', input.link)
  if (input.category) body.set('category', input.category)
  if (input.addedVia) body.set('added_via', input.addedVia)
  if (input.provider) body.set('provider', input.provider)
  return request('/api/v1/downloads/webdl', apiKey, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body,
  })
}

// CheckCachedResponse is the shared shape of all three check-cached
// endpoints below — reports whether the given magnet/URL/link is already
// cached on the provider's side, without adding it.
export interface CheckCachedResponse {
  cached: boolean
}

export function checkCachedTorrent(apiKey: string, magnet: string): Promise<CheckCachedResponse> {
  return request(`/api/v1/downloads/torrent/check-cached?magnet=${encodeURIComponent(magnet)}`, apiKey)
}

export function checkCachedUsenet(apiKey: string, url: string): Promise<CheckCachedResponse> {
  return request(`/api/v1/downloads/usenet/check-cached?url=${encodeURIComponent(url)}`, apiKey)
}

export function checkCachedWebDownload(apiKey: string, link: string): Promise<CheckCachedResponse> {
  return request(`/api/v1/downloads/webdl/check-cached?link=${encodeURIComponent(link)}`, apiKey)
}

export interface TorrentInfoFile {
  path: string
  size_bytes: number
}

// TorrentInfoResponse is GET /api/v1/downloads/torrent/info's response —
// available is false (with no other fields, just error) whenever the
// provider doesn't support previews, or TorBox couldn't find the torrent on
// the network at all — routine, not something to show as an error.
export interface TorrentInfoResponse {
  available: boolean
  error?: string
  name?: string
  hash?: string
  size_bytes?: number
  seeds?: number
  peers?: number
  files?: TorrentInfoFile[]
}

export function getTorrentInfo(apiKey: string, magnet: string): Promise<TorrentInfoResponse> {
  return request(`/api/v1/downloads/torrent/info?magnet=${encodeURIComponent(magnet)}`, apiKey)
}

// ProviderSetting is one configurable provider. Every registered provider
// appears here, including ones holding no credentials yet — this is the
// surface you configure them from, so one has to be visible before it can
// be set up. That's the opposite of getProviders(), which answers "what can
// I use right now" and omits them.
export interface ProviderSetting {
  name: string
  // type is which service this entry actually is — equal to name for a
  // first account, different when one service holds several.
  type: string
  configured: boolean
  // *_capable is what the provider's service can do at all; *_enabled is
  // whether it's switched on here. Enabled is always false when capable is,
  // and they're separate so the UI can distinguish "this service has no
  // usenet" from "usenet is turned off".
  torrent_capable: boolean
  usenet_capable: boolean
  webdl_capable: boolean
  torrent_enabled: boolean
  usenet_enabled: boolean
  webdl_enabled: boolean
  // default marks which provider a new download goes to when the add
  // doesn't name one.
  default: boolean
}

export function getProviderSettings(apiKey: string): Promise<ProviderSetting[]> {
  return request('/api/v1/settings/providers', apiKey)
}

// setProviderApiKey applies a key to one provider. An empty key clears its
// credentials, which is how a provider is switched off without editing
// config.yaml — it stays listed and can be configured again later.
export function setProviderApiKey(apiKey: string, provider: string, providerKey: string): Promise<void> {
  return request(`/api/v1/settings/providers/${encodeURIComponent(provider)}`, apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ api_key: providerKey }),
  })
}

// setProviderKinds switches which kinds one provider handles. Only the
// kinds passed are changed, so a caller can flip one without restating the
// rest. A kind the provider's service doesn't have is refused rather than
// silently ignored.
export function setProviderKinds(
  apiKey: string,
  provider: string,
  kinds: Partial<Record<'torrent' | 'usenet' | 'webdl', boolean>>,
): Promise<void> {
  return request(`/api/v1/settings/providers/${encodeURIComponent(provider)}/kinds`, apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(kinds),
  })
}

// resetProvider returns a provider to its unconfigured state: credentials
// cleared, every supported kind switched back on, the entry itself kept so
// it stays listed and can be set up again.
export function resetProvider(apiKey: string, provider: string): Promise<void> {
  return request(`/api/v1/settings/providers/${encodeURIComponent(provider)}/reset`, apiKey, { method: 'POST' })
}

// getProviderTypes lists the implementations this build can construct. A
// provider's name is free text; its type is not — the two differ when one
// service holds more than one account.
export function getProviderTypes(apiKey: string): Promise<string[]> {
  return request('/api/v1/settings/provider-types', apiKey)
}

// addProvider registers a new provider entry. Pass a type when it differs
// from the name, which is how a second account on the same service is
// added: name "torbox-work", type "torbox".
export function addProvider(apiKey: string, name: string, type: string, providerKey: string): Promise<void> {
  return request('/api/v1/settings/providers', apiKey, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, type, api_key: providerKey }),
  })
}

// removeProvider deletes a provider entry. Downloads already tracked
// against it are left alone — they're records of real things, and removing
// a provider is a configuration change, not an instruction to discard them.
export function removeProvider(apiKey: string, provider: string): Promise<void> {
  return request(`/api/v1/settings/providers/${encodeURIComponent(provider)}`, apiKey, { method: 'DELETE' })
}

export function setDefaultProvider(apiKey: string, provider: string): Promise<void> {
  return request('/api/v1/settings/providers/default', apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ provider }),
  })
}

export interface GeneralSettings {
  api_key: string
  port: number
  data_dir: string
  download_dir: string
  log_level: string
  import_interval_seconds: number
  import_max_retries: number
  max_concurrent_downloads: number
  import_fetch_timeout_seconds: number
  cleanup_after_days: number
  // download_dir_mode is the octal permission string (e.g. "0777") every
  // download directory gets — see docs/providers.md#directory-permissions
  // for why the default is world-writable.
  download_dir_mode: string
  // fast_poll_interval_seconds is how often an actively in-flight Managed
  // download is checked individually, independent of
  // import_interval_seconds's own full-account listing — see
  // docs/providers.md.
  fast_poll_interval_seconds: number
  // provider_request_timeout_seconds bounds a single call to the debrid
  // provider's own API (list, status, add, delete, account) — a plain
  // total-request deadline, unlike fetch_timeout above (an idle one).
  provider_request_timeout_seconds: number
  // tls_cert_file/tls_key_file are config/env-only (no editable UI field —
  // same treatment data_dir already gets) but still reported here for
  // transparency, the same way data_dir is.
  tls_enabled: boolean
  tls_port: number
  tls_cert_file: string
  tls_key_file: string
  // min_fetch_file_size_bytes/max_fetch_file_size_bytes/include_file_regex/
  // exclude_file_regex skip a file entirely when fetching a download's
  // files to local disk — 0/0/empty (the defaults) fetch everything, same
  // as before these existed.
  min_fetch_file_size_bytes: number
  max_fetch_file_size_bytes: number
  include_file_regex: string
  exclude_file_regex: string
  // stuck_download_timeout_minutes auto-errors a download that's sat
  // queued/downloading with no genuine change reported for this long — 0
  // (the default) disables the watchdog entirely.
  stuck_download_timeout_minutes: number
  // cleanup_error_after_days automatically removes a download that's sat
  // in error for this many days — 0 (the default) disables it entirely.
  cleanup_error_after_days: number
  backup_interval_hours: number
  backup_keep: number
}

export function getGeneralSettings(apiKey: string): Promise<GeneralSettings> {
  return request('/api/v1/settings/general', apiKey)
}

// regenerateApiKey invalidates apiKey immediately (server-side, across the
// native API and both compat shims) — the caller must switch to the
// returned key right away, including for this same session.
export function regenerateApiKey(apiKey: string): Promise<{ api_key: string }> {
  return request('/api/v1/settings/api-key/regenerate', apiKey, { method: 'POST' })
}

export interface GeneralUpdateInput {
  port: number
  data_dir: string
  download_dir: string
  log_level: string
  import_interval_seconds: number
  import_max_retries: number
  max_concurrent_downloads: number
  import_fetch_timeout_seconds: number
  cleanup_after_days: number
  download_dir_mode: string
  fast_poll_interval_seconds: number
  provider_request_timeout_seconds: number
  tls_enabled: boolean
  tls_port: number
  tls_cert_file: string
  tls_key_file: string
  min_fetch_file_size_bytes: number
  max_fetch_file_size_bytes: number
  include_file_regex: string
  exclude_file_regex: string
  stuck_download_timeout_minutes: number
  cleanup_error_after_days: number
  backup_interval_hours: number
  backup_keep: number
}

// updateGeneralSettings applies everything except port/data_dir/tls_*
// immediately, no restart needed. Those are saved too, but only take effect
// after a restart — restart_required in the response reflects whether any
// of them changed.
export function updateGeneralSettings(apiKey: string, update: GeneralUpdateInput): Promise<{ restart_required: boolean }> {
  return request('/api/v1/settings/general', apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(update),
  })
}

// restartServer gracefully restarts AcerviNode so a restart-required
// setting just saved (port, tls_enabled/tls_port, ...) takes effect —
// already persisted to config.yaml by the time this is called, so the next
// start picks it up automatically. supervised reflects whether a process
// supervisor (systemd) is actually watching this process — false means the
// restart will stop AcerviNode with nothing bringing it back.
export function restartServer(apiKey: string): Promise<{ restarting: boolean; supervised: boolean }> {
  return request('/api/v1/settings/system/restart', apiKey, { method: 'POST' })
}

// regenerateCertificate forces a fresh self-signed TLS certificate — the
// fix when the current one's baked-in SANs no longer match how the instance
// is reached (e.g. its LAN IP changed). Requires a restart afterward to
// actually load the new cert.
export function regenerateCertificate(apiKey: string): Promise<{ restart_required: boolean }> {
  return request('/api/v1/settings/tls/regenerate', apiKey, { method: 'POST' })
}

// testProviderConnection makes one real, live call to a provider with its
// currently configured key. A failed connection test is still a successful
// API call (200) — the failure is reported in the body via ok:false, not an
// HTTP error status.
export function testProviderConnection(apiKey: string, provider: string): Promise<{ ok: boolean; latency_ms?: number; error?: string }> {
  return request(`/api/v1/settings/providers/${encodeURIComponent(provider)}/test`, apiKey, { method: 'POST' })
}

export interface Categories {
  torrent: string[]
  usenet: string[]
  // paths maps a category name to its override destination directory (see
  // setCategoryPath) — only categories with an override set have an entry.
  paths: Record<string, string>
}

export function getCategories(apiKey: string): Promise<Categories> {
  return request('/api/v1/settings/categories', apiKey)
}

// setCategoryPath sets category's override destination directory — internal/
// importer uses it instead of download_dir/<category> for downloads in that
// category. Pass an empty path to clear a previously set override. Applies
// immediately, no restart needed.
export function setCategoryPath(apiKey: string, category: string, path: string): Promise<void> {
  return request('/api/v1/settings/categories/path', apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ category, path }),
  })
}

// removeCategory forgets a category entirely (its path override and its
// registration with both compat shims) — unlike setCategoryPath with an
// empty path, which only clears the override. If Sonarr/Radarr is still
// actively configured with this category, it can simply come back the next
// time it's declared again, same as it would against a real qBittorrent/
// SABnzbd install.
export function removeCategory(apiKey: string, category: string): Promise<void> {
  return request(`/api/v1/settings/categories/${encodeURIComponent(category)}`, apiKey, { method: 'DELETE' })
}

// TorBoxAccount is GET /api/v1/settings/account's response — a live call to
// the configured provider, not a cached snapshot. available is false (with a
// reason in error) whenever nothing's configured yet, or the configured
// provider doesn't support reporting account status at all — that's routine,
// not a failure, so the Settings page just hides the section rather than
// showing an error state.
export interface ProviderAccount {
  available: boolean
  error?: string
  plan_name?: string
  is_subscribed?: boolean
  premium_expires_at?: string
  total_bytes_downloaded?: number
  // cooldown_until, if present and in the future, means the provider is
  // currently restricting this account — every download's progress/state
  // can look frozen with no other visible explanation while this is set.
  // Not documented anywhere by the provider; surfaced as-is, not acted on.
  cooldown_until?: string
}

// getProviderAccount reports one provider's own account state. Asked per
// provider because each account has its own plan, expiry and restrictions —
// a single instance-wide panel could only show one of them, under a heading
// that might mean either.
export function getProviderAccount(apiKey: string, provider: string): Promise<ProviderAccount> {
  return request(`/api/v1/settings/providers/${encodeURIComponent(provider)}/account`, apiKey)
}

// StatusInfo is GET /api/v1/status's response — internal/importer's own
// health signals (tick liveness, per-kind rate-limit cooldowns, per-kind
// error counts), distinct from ProviderAccount above (the provider's own
// account state, e.g. cooldown_until) — this answers "is polling itself
// working," not "is the provider account restricted." A fast, purely local
// call (no live network call to the provider), unlike getProviderAccount.
// ProviderKindStatus is one provider's handling of one kind. kinds below
// aggregates across providers, which answers "is this kind working" but not
// "which provider is struggling" — with two configured, one failing every
// list while the other succeeds leaves the kind looking healthy.
export interface ProviderKindStatus {
  provider: string
  kind: string
  last_successful_list_at?: string
  rate_limited_until?: string
  // Set while this provider/kind's listing is being disbelieved by the
  // mass-vanish guard: succeeding, but empty enough that acting on it would
  // flag every tracked download as gone. The one state where everything
  // else reads healthy while nothing is actually reconciling.
  listing_anomalous_since?: string
}

export interface KindStatus {
  last_successful_list_at?: string
  rate_limited_until?: string
  error_count: number
}

// BackupInfo is one database snapshot. Names and sizes only — a snapshot
// contains every login account and session in the instance, so the API
// never serves its contents.
export interface BackupInfo {
  name: string
  size_bytes: number
  taken_at: string
}

export function getBackups(apiKey: string): Promise<BackupInfo[]> {
  return request('/api/v1/settings/backups', apiKey)
}

export function runBackup(apiKey: string): Promise<{ name: string }> {
  return request('/api/v1/settings/backups', apiKey, { method: 'POST' })
}

export interface StatusInfo {
  last_tick_at?: string
  kinds: Record<string, KindStatus>
  providers?: ProviderKindStatus[]
}

export function getStatus(apiKey: string): Promise<StatusInfo> {
  return request('/api/v1/status', apiKey)
}

// --- Auth: optional login accounts on top of the API key, which keeps
// working unaffected by any of this (see internal/api/auth.go). Every
// function here uses an empty apiKey ('') — none of them need the master
// credential, and a signed-in browser has no reason to know or hold it.

export type Role = 'admin' | 'member'

export interface AuthStatus {
  auth_enabled: boolean
  authenticated: boolean
  username?: string
  role?: Role
}

// getAuthStatus is unauthenticated — the UI needs it to decide between the
// login form, the API-key prompt, and going straight in, before it knows
// anything else.
export function getAuthStatus(): Promise<AuthStatus> {
  return request('/api/v1/auth/status', '')
}

// getSetupStatus is unauthenticated — it must answer before any credentials
// exist, to decide whether to open the first-run wizard.
export function getSetupStatus(): Promise<{ needed: boolean }> {
  return request('/api/v1/setup/status', '')
}

// setupInstance claims a fresh instance in one step: creates the first
// (always admin) login account and signs this browser in — no API key
// involved. Rejected (403) once the instance is no longer fresh (see
// docs/providers.md#roles).
export function setupInstance(username: string, password: string): Promise<{ ok: boolean }> {
  return request('/api/v1/setup', '', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
}

export function login(username: string, password: string): Promise<{ ok: boolean }> {
  return request('/api/v1/auth/login', '', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
}

export function logout(): Promise<void> {
  return request('/api/v1/auth/logout', '', { method: 'POST' })
}

// UserAccount is one login account, as reported to Settings → Security —
// never includes a password.
export interface UserAccount {
  username: string
  role: Role
  default: boolean
}

// The user-management endpoints below are admin-only (except
// setUserPassword's self-service case) — always called with the real
// apiKey/admin session, never ''.

export function listUsers(apiKey: string): Promise<{ users: UserAccount[] }> {
  return request('/api/v1/settings/users', apiKey)
}

export function addUser(apiKey: string, username: string, password: string, role: Role): Promise<{ users: UserAccount[] }> {
  return request('/api/v1/settings/users', apiKey, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password, role }),
  })
}

export function removeUser(apiKey: string, username: string): Promise<{ users: UserAccount[] }> {
  return request(`/api/v1/settings/users/${encodeURIComponent(username)}`, apiKey, { method: 'DELETE' })
}

// setUserPassword also works for a member changing their own password —
// call it with '' (the caller is relying on the session cookie, not an
// admin API key) when that's the case.
export function setUserPassword(apiKey: string, username: string, password: string): Promise<{ ok: boolean }> {
  return request(`/api/v1/settings/users/${encodeURIComponent(username)}/password`, apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  })
}

export function setUserRole(apiKey: string, username: string, role: Role): Promise<{ users: UserAccount[] }> {
  return request(`/api/v1/settings/users/${encodeURIComponent(username)}/role`, apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ role }),
  })
}

export function makeDefaultUser(apiKey: string, username: string): Promise<{ users: UserAccount[] }> {
  return request(`/api/v1/settings/users/${encodeURIComponent(username)}/default`, apiKey, { method: 'POST' })
}
