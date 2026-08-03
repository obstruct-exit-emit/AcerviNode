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

export function addTorrent(
  apiKey: string,
  input: { magnet: string; category?: string } | { file: File; category?: string },
): Promise<Download> {
  const form = new FormData()
  if ('magnet' in input) form.set('magnet', input.magnet)
  else form.set('file', input.file)
  if (input.category) form.set('category', input.category)
  // No Content-Type header here on purpose — the browser sets
  // multipart/form-data with the correct boundary itself when the body is a
  // FormData; setting it manually would drop the boundary parameter.
  return request('/api/v1/downloads/torrent', apiKey, { method: 'POST', body: form })
}

export function addUsenet(
  apiKey: string,
  input: { url: string; category?: string } | { file: File; category?: string },
): Promise<Download> {
  const form = new FormData()
  if ('url' in input) form.set('url', input.url)
  else form.set('file', input.file)
  if (input.category) form.set('category', input.category)
  return request('/api/v1/downloads/usenet', apiKey, { method: 'POST', body: form })
}

// addWebDownload submits a direct hoster link (Mega, 1Fichier, Mediafire, and
// ~160 others TorBox's Web Downloads service supports) — link-only, unlike
// addTorrent/addUsenet: there's no file-upload variant for this endpoint
// (TorBox's own createwebdownload API has none either).
export function addWebDownload(apiKey: string, input: { link: string; category?: string }): Promise<Download> {
  const body = new URLSearchParams()
  body.set('link', input.link)
  if (input.category) body.set('category', input.category)
  return request('/api/v1/downloads/webdl', apiKey, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body,
  })
}

export interface ProviderSettings {
  [providerName: string]: { configured: boolean }
}

export function getProviderSettings(apiKey: string): Promise<ProviderSettings> {
  return request('/api/v1/settings/providers', apiKey)
}

export function setTorBoxApiKey(apiKey: string, torboxApiKey: string): Promise<void> {
  return request('/api/v1/settings/providers/torbox', apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ api_key: torboxApiKey }),
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
  // tls_cert_file/tls_key_file are config/env-only (no editable UI field —
  // same treatment data_dir already gets) but still reported here for
  // transparency, the same way data_dir is.
  tls_enabled: boolean
  tls_port: number
  tls_cert_file: string
  tls_key_file: string
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
  tls_enabled: boolean
  tls_port: number
  tls_cert_file: string
  tls_key_file: string
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

// testTorBoxConnection makes one real, live call to TorBox with the
// currently configured key. A failed connection test is still a successful
// API call (200) — the failure is reported in the body via ok:false, not an
// HTTP error status.
export function testTorBoxConnection(apiKey: string): Promise<{ ok: boolean; latency_ms?: number; error?: string }> {
  return request('/api/v1/settings/providers/torbox/test', apiKey, { method: 'POST' })
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

// TorBoxAccount is GET /api/v1/settings/account's response — a live call to
// the configured provider, not a cached snapshot. available is false (with a
// reason in error) whenever nothing's configured yet, or the configured
// provider doesn't support reporting account status at all — that's routine,
// not a failure, so the Settings page just hides the section rather than
// showing an error state.
export interface TorBoxAccount {
  available: boolean
  error?: string
  plan_name?: string
  is_subscribed?: boolean
  premium_expires_at?: string
  total_bytes_downloaded?: number
}

export function getTorBoxAccount(apiKey: string): Promise<TorBoxAccount> {
  return request('/api/v1/settings/account', apiKey)
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
