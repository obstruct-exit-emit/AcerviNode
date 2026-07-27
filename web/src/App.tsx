import { useCallback, useEffect, useState } from 'react'
import {
  ApiError,
  clearStoredApiKey,
  deleteDownload,
  getStoredApiKey,
  getProviders,
  getVersion,
  listDownloads,
  storeApiKey,
  type Download,
  type ProviderStatus,
} from './api'
import { ApiKeyGate } from './components/ApiKeyGate'
import { DownloadDetail } from './components/DownloadDetail'
import { DownloadsTable } from './components/DownloadsTable'
import { ProviderBadges } from './components/ProviderBadges'
import { Settings } from './components/Settings'
import './App.css'

const POLL_INTERVAL_MS = 4000

type View = 'downloads' | 'settings'

export default function App() {
  const [apiKey, setApiKey] = useState<string | null>(() => getStoredApiKey())
  const [gateError, setGateError] = useState<string | undefined>(undefined)
  const [view, setView] = useState<View>('downloads')
  const [version, setVersion] = useState<string>('')
  const [providers, setProviders] = useState<ProviderStatus[]>([])
  const [downloads, setDownloads] = useState<Download[]>([])
  const [loadError, setLoadError] = useState<string | undefined>(undefined)
  const [selectedId, setSelectedId] = useState<string | null>(null)

  const handleUnauthorized = useCallback(() => {
    clearStoredApiKey()
    setApiKey(null)
    setGateError('That key was rejected by the server.')
  }, [])

  const refresh = useCallback(
    async (key: string) => {
      try {
        const [v, p, d] = await Promise.all([getVersion(key), getProviders(key), listDownloads(key)])
        setVersion(v.version)
        setProviders(p)
        setDownloads(d)
        setLoadError(undefined)
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          handleUnauthorized()
          return
        }
        setLoadError(err instanceof Error ? err.message : String(err))
      }
    },
    [handleUnauthorized],
  )

  useEffect(() => {
    if (!apiKey) return
    refresh(apiKey)
    const interval = setInterval(() => refresh(apiKey), POLL_INTERVAL_MS)
    return () => clearInterval(interval)
  }, [apiKey, refresh])

  function handleKeySubmit(key: string) {
    storeApiKey(key)
    setGateError(undefined)
    setApiKey(key)
  }

  // Called by Settings after a successful regenerate — the old key just
  // stopped working everywhere, so the UI's own session has to switch to
  // the new one immediately or its next poll would 401 and log it out.
  function handleApiKeyChanged(newKey: string) {
    storeApiKey(newKey)
    setApiKey(newKey)
  }

  async function handleDelete(d: Download) {
    if (!apiKey) return
    if (!confirm(`Delete "${d.name}"? This also removes it from the debrid provider.`)) return
    try {
      await deleteDownload(apiKey, d.id, true)
      refresh(apiKey)
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    }
  }

  if (!apiKey) {
    return <ApiKeyGate onSubmit={handleKeySubmit} error={gateError} />
  }

  return (
    <div className="app">
      <header className="app-header">
        <h1>📦 AcerviNode</h1>
        <div className="header-meta">
          <ProviderBadges providers={providers} />
          <span className="version">v{version}</span>
          <button
            className="logout-btn"
            onClick={() => {
              clearStoredApiKey()
              setApiKey(null)
            }}
          >
            Sign out
          </button>
        </div>
      </header>

      <nav className="tabs">
        <button className={view === 'downloads' ? 'tab tab-active' : 'tab'} onClick={() => setView('downloads')}>
          Downloads
        </button>
        <button className={view === 'settings' ? 'tab tab-active' : 'tab'} onClick={() => setView('settings')}>
          Settings
        </button>
      </nav>

      <main>
        {loadError && <p className="load-error">Couldn't reach AcerviNode: {loadError}</p>}
        {view === 'downloads' ? (
          <DownloadsTable downloads={downloads} onDelete={handleDelete} onSelect={(d) => setSelectedId(d.id)} />
        ) : (
          <Settings apiKey={apiKey} onApiKeyChanged={handleApiKeyChanged} />
        )}
      </main>

      {selectedId && <DownloadDetail apiKey={apiKey} id={selectedId} onClose={() => setSelectedId(null)} />}
    </div>
  )
}
