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
  kind: 'torrent' | 'usenet'
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

export function deleteDownload(apiKey: string, id: string, deleteFiles: boolean): Promise<void> {
  return request(`/api/v1/downloads/${encodeURIComponent(id)}?deleteFiles=${deleteFiles}`, apiKey, {
    method: 'DELETE',
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
