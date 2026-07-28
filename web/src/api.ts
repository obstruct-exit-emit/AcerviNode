// Thin client for AcerviNode's native /api/v1. No generated client, no
// fetch-wrapper library — this is small enough to just write directly.

export interface ProviderStatus {
  name: string
  torrent_capable: boolean
  usenet_capable: boolean
}

export interface Download {
  id: string
  provider: string
  protocol: 'torrent' | 'usenet'
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
  error_message?: string
  retry_count?: number
  next_retry_at?: string
}

export interface DownloadFile {
  path: string
  size_bytes: number
}

export interface DownloadDetail extends Download {
  files: DownloadFile[]
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

const STORAGE_KEY = 'acervinode_api_key'

export function getStoredApiKey(): string | null {
  return localStorage.getItem(STORAGE_KEY)
}

export function storeApiKey(key: string): void {
  localStorage.setItem(STORAGE_KEY, key)
}

export function clearStoredApiKey(): void {
  localStorage.removeItem(STORAGE_KEY)
}

async function request<T>(path: string, apiKey: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    ...init,
    headers: { ...init?.headers, Authorization: `Bearer ${apiKey}` },
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

export function listDownloads(apiKey: string): Promise<Download[]> {
  return request('/api/v1/downloads', apiKey)
}

export function getDownload(apiKey: string, id: string): Promise<DownloadDetail> {
  return request(`/api/v1/downloads/${encodeURIComponent(id)}`, apiKey)
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
}

// updateGeneralSettings applies download_dir/log_level/import_interval_seconds/
// import_max_retries immediately, no restart needed. port/data_dir are saved
// too, but only take effect after a restart — restart_required in the
// response reflects whether either of those changed.
export function updateGeneralSettings(apiKey: string, update: GeneralUpdateInput): Promise<{ restart_required: boolean }> {
  return request('/api/v1/settings/general', apiKey, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(update),
  })
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
}

export function getCategories(apiKey: string): Promise<Categories> {
  return request('/api/v1/settings/categories', apiKey)
}

export function addCategory(apiKey: string, protocol: 'torrent' | 'usenet', name: string): Promise<void> {
  return request('/api/v1/settings/categories', apiKey, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ protocol, name }),
  })
}
