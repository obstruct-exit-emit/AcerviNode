import { useCallback, useEffect, useState } from 'react'
import {
  ApiError,
  clearStoredApiKey,
  deleteDownload,
  getDownload,
  getFileLink,
  getStoredApiKey,
  getProviders,
  getVersion,
  getZipLink,
  listDownloads,
  retryDownload,
  storeApiKey,
  type Download,
  type ProviderStatus,
} from './api'
import { AddDownload } from './components/AddDownload'
import { ApiKeyGate } from './components/ApiKeyGate'
import { DownloadDetail } from './components/DownloadDetail'
import { DownloadsTable } from './components/DownloadsTable'
import { ProviderBadges } from './components/ProviderBadges'
import { Settings } from './components/Settings'
import { resolveDownloadDirectory, supportsDirectoryPicker, writeFileToDirectory } from './fsAccess'
import { getDownloadMode } from './preferences'
import './App.css'

const POLL_INTERVAL_MS = 4000

type View = 'managed' | 'manual' | 'settings'

export default function App() {
  const [apiKey, setApiKey] = useState<string | null>(() => getStoredApiKey())
  const [gateError, setGateError] = useState<string | undefined>(undefined)
  const [view, setView] = useState<View>('managed')
  const [version, setVersion] = useState<string>('')
  const [providers, setProviders] = useState<ProviderStatus[]>([])
  // Both tabs' downloads are kept loaded regardless of which is active, so
  // switching tabs is instant and doesn't need its own loading state.
  const [managedDownloads, setManagedDownloads] = useState<Download[]>([])
  const [manualDownloads, setManualDownloads] = useState<Download[]>([])
  const [loadError, setLoadError] = useState<string | undefined>(undefined)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [addOpen, setAddOpen] = useState(false)
  const [downloadingAllId, setDownloadingAllId] = useState<string | null>(null)

  const handleUnauthorized = useCallback(() => {
    clearStoredApiKey()
    setApiKey(null)
    setGateError('That key was rejected by the server.')
  }, [])

  const refresh = useCallback(
    async (key: string) => {
      try {
        const [v, p, managed, manual] = await Promise.all([
          getVersion(key),
          getProviders(key),
          listDownloads(key, 'arr'),
          listDownloads(key, 'manual'),
        ])
        setVersion(v.version)
        setProviders(p)
        setManagedDownloads(managed)
        setManualDownloads(manual)
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

  async function handleRetry(d: Download) {
    if (!apiKey) return
    try {
      await retryDownload(apiKey, d.id)
      refresh(apiKey)
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    }
  }

  // The per-row "Download all" button's entry point — dispatches on the
  // user's Settings > Downloads preference (see preferences.getDownloadMode).
  // 'zip' resolves the whole download as one provider-zipped archive; the
  // default 'individual' fetches every file separately (see
  // handleDownloadAllIndividual). Either way, the detail view's own explicit
  // "Download all (zip)"/per-file buttons remain available regardless of
  // this preference — it only controls the row button's default action.
  async function handleDownloadAll(d: Download) {
    if (!apiKey) return
    if (getDownloadMode() === 'zip') {
      await handleDownloadAllZip(d)
    } else {
      await handleDownloadAllIndividual(d)
    }
  }

  async function handleDownloadAllZip(d: Download) {
    if (!apiKey) return
    setDownloadingAllId(d.id)
    try {
      const { url } = await getZipLink(apiKey, d.id)
      window.open(url, '_blank', 'noopener,noreferrer')
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    } finally {
      setDownloadingAllId(null)
    }
  }

  // Downloads every file individually. In a browser that supports it
  // (Chromium-based; not Firefox/Safari), files are streamed straight into a
  // folder — the remembered default if one's already usable (no prompt at
  // all), otherwise the picker, same as before — with no per-file tab/
  // download popup at all. Elsewhere, it falls back to opening each file's
  // link in its own tab.
  async function handleDownloadAllIndividual(d: Download) {
    if (!apiKey) return

    // Must happen first, before any other await — resolving (and
    // potentially prompting for) a directory needs the click's own
    // user-activation, which an intervening API call consumes.
    let dir: FileSystemDirectoryHandle | null = null
    if (supportsDirectoryPicker()) {
      try {
        dir = await resolveDownloadDirectory()
        if (!dir) return // user cancelled the picker
      } catch (err) {
        alert(err instanceof Error ? err.message : String(err))
        return
      }
    }

    setDownloadingAllId(d.id)
    try {
      const detail = await getDownload(apiKey, d.id)
      const files = detail.files.filter((f) => f.provider_file_id)
      if (files.length === 0) {
        alert(detail.files_error ? `Couldn't get this download's files: ${detail.files_error}` : 'No files available to download yet.')
        return
      }
      const failed: string[] = []
      for (const f of files) {
        try {
          const { url } = await getFileLink(apiKey, d.id, f.provider_file_id as string)
          if (dir) {
            const resp = await fetch(url)
            if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
            await writeFileToDirectory(dir, f.path, resp)
          } else {
            window.open(url, '_blank', 'noopener,noreferrer')
          }
        } catch (err) {
          console.error(`Failed to download ${f.path}`, err)
          failed.push(f.path)
        }
      }
      if (failed.length > 0) {
        alert(`${failed.length} of ${files.length} file(s) failed to download:\n${failed.join('\n')}`)
      }
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    } finally {
      setDownloadingAllId(null)
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
        <button className={view === 'managed' ? 'tab tab-active' : 'tab'} onClick={() => setView('managed')}>
          Managed
        </button>
        <button className={view === 'manual' ? 'tab tab-active' : 'tab'} onClick={() => setView('manual')}>
          Manual
        </button>
        <button className={view === 'settings' ? 'tab tab-active' : 'tab'} onClick={() => setView('settings')}>
          Settings
        </button>
        <button className="add-download-btn" onClick={() => setAddOpen(true)}>
          + Add
        </button>
      </nav>

      <main>
        {loadError && <p className="load-error">Couldn't reach AcerviNode: {loadError}</p>}
        {view === 'managed' && (
          <DownloadsTable
            downloads={managedDownloads}
            onDelete={handleDelete}
            onRetry={handleRetry}
            onDownloadAll={handleDownloadAll}
            downloadingAllId={downloadingAllId}
            onSelect={(d) => setSelectedId(d.id)}
            allowRetry
            showCategory
            emptyMessage="No managed downloads yet. Add one through Sonarr/Radarr, or via the qBittorrent/SABnzbd compat APIs directly."
          />
        )}
        {view === 'manual' && (
          <DownloadsTable
            downloads={manualDownloads}
            onDelete={handleDelete}
            onRetry={handleRetry}
            onDownloadAll={handleDownloadAll}
            downloadingAllId={downloadingAllId}
            onSelect={(d) => setSelectedId(d.id)}
            allowRetry={false}
            showCategory={false}
            emptyMessage="No manual downloads yet. Add one with the button above, or add it directly through TorBox — it'll show up here automatically."
          />
        )}
        {view === 'settings' && <Settings apiKey={apiKey} onApiKeyChanged={handleApiKeyChanged} />}
      </main>

      {selectedId && <DownloadDetail apiKey={apiKey} id={selectedId} onClose={() => setSelectedId(null)} />}
      {addOpen && (
        <AddDownload
          apiKey={apiKey}
          providers={providers}
          onClose={() => setAddOpen(false)}
          onAdded={() => {
            setAddOpen(false)
            setView('manual')
            refresh(apiKey)
          }}
        />
      )}
    </div>
  )
}
