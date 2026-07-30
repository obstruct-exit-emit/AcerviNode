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
import { DownloadOptionsDialog, type DownloadOptions } from './components/DownloadOptionsDialog'
import { DownloadsTable } from './components/DownloadsTable'
import { ProviderBadges } from './components/ProviderBadges'
import { Settings } from './components/Settings'
import {
  forceDownload,
  listenForDownloadWindowMessages,
  openDownloadWindow,
  sendBatchToDownloadWindow,
  writeFileToDirectory,
} from './fsAccess'
import './App.css'

// basename extracts the last path segment for a saved file's name — a
// browser's own download mechanism (forceDownload, or the tab-per-file
// path this replaced) always saves flat into its downloads location, not
// into a nested directory structure the way the folder-streaming path
// (writeFileToDirectory) does.
function basename(path: string): string {
  const parts = path.split('/').filter(Boolean)
  return parts[parts.length - 1] ?? path
}

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
  // Set while DownloadOptionsDialog is open for a given row — every
  // "download everything" action goes through it now, regardless of which
  // mode ends up chosen. See handleDownloadAll/startDownloadAll.
  const [downloadDialogFor, setDownloadDialogFor] = useState<Download | null>(null)
  // Every row with a "Download all" currently in flight, keyed by download
  // id rather than a single shared id — more than one row can genuinely be
  // downloading at once (the Downloads popup can run several batches
  // concurrently), and a shared single value made every row's progress
  // indicator flicker between whichever download's messages arrived most
  // recently, a real bug found live once two downloads actually overlapped.
  const [busyIds, setBusyIds] = useState<Set<string>>(new Set())
  // Cumulative bytes written so far, per row id (the File System Access
  // path only — see startStreamedDownload). A row in busyIds but absent
  // here shows the plain "…" indicator instead (every other mode has
  // nothing granular to report).
  const [downloadProgress, setDownloadProgress] = useState<Record<string, { loaded: number; total: number }>>({})

  function markBusy(id: string) {
    setBusyIds((prev) => (prev.has(id) ? prev : new Set(prev).add(id)))
  }
  function clearBusy(id: string) {
    setBusyIds((prev) => {
      if (!prev.has(id)) return prev
      const next = new Set(prev)
      next.delete(id)
      return next
    })
  }
  function setProgressFor(id: string, progress: { loaded: number; total: number }) {
    setDownloadProgress((prev) => ({ ...prev, [id]: progress }))
  }
  function clearProgressFor(id: string) {
    setDownloadProgress((prev) => {
      if (!(id in prev)) return prev
      const next = { ...prev }
      delete next[id]
      return next
    })
  }

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

  // Relays the Downloads popup window's progress back into each row's own
  // progress bar, keyed by download id — best-effort, since the popup works
  // fine without anyone listening.
  useEffect(() => {
    return listenForDownloadWindowMessages((msg) => {
      if (msg.type === 'batch-progress') {
        markBusy(msg.downloadId)
        setProgressFor(msg.downloadId, { loaded: msg.loaded, total: msg.total })
      } else if (msg.type === 'batch-complete') {
        clearBusy(msg.downloadId)
        clearProgressFor(msg.downloadId)
        if (msg.failed.length > 0) {
          alert(`${msg.failed.length} file(s) failed to download:\n${msg.failed.map((f) => `${f.path}: ${f.error}`).join('\n')}`)
        }
      }
    })
  }, [])

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

  // The single entry point for every "download everything" action — both
  // the downloads table's per-row button and the detail view's "Download
  // all" button call this now, and it always opens DownloadOptionsDialog
  // rather than silently picking a mode/fallback on the caller's behalf.
  // The dialog itself shows every mode this browser can actually do and
  // remembers the last one chosen — see startDownloadAll, its confirm
  // handler, for the actual per-mode dispatch.
  function handleDownloadAll(d: Download) {
    if (!apiKey) return
    setDownloadDialogFor(d)
  }

  // DownloadOptionsDialog's confirm handler — dispatches on whichever mode
  // was chosen there.
  async function startDownloadAll(d: Download, opts: DownloadOptions) {
    if (opts.mode === 'zip') {
      await handleDownloadAllZip(d)
    } else if (opts.mode === 'individual') {
      await handleDownloadAllIndividual(d)
    } else if (opts.folder) {
      await startStreamedDownload(d, opts.folder, opts.useDownloadManager ?? true)
    }
  }

  async function handleDownloadAllZip(d: Download) {
    if (!apiKey) return
    markBusy(d.id)
    try {
      const { url } = await getZipLink(apiKey, d.id)
      window.open(url, '_blank', 'noopener,noreferrer')
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    } finally {
      clearBusy(d.id)
    }
  }

  // The 'individual' mode chosen in DownloadOptionsDialog: every file
  // downloads on its own via forceDownload — a real browser download (blob
  // + <a download>), not a tab that might just render the file inline (see
  // fsAccess.forceDownload's own doc comment for why that distinction
  // matters). The only mode available at all on a browser without folder
  // access, but can be chosen deliberately on any browser now too.
  async function handleDownloadAllIndividual(d: Download) {
    if (!apiKey) return
    markBusy(d.id)
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
          await forceDownload(url, basename(f.path))
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
      clearBusy(d.id)
    }
  }

  // The 'folder' mode chosen in DownloadOptionsDialog — folder and the
  // Downloads-window choice are already decided by then.
  async function startStreamedDownload(d: Download, folder: FileSystemDirectoryHandle, useDownloadManager: boolean) {
    if (!apiKey) return

    // Deliberately the first statement, before any await — window.open()
    // is synchronous, and this needs the dialog confirm button's own click
    // gesture, same requirement as showDirectoryPicker(). Only actually
    // opens/focuses the popup when the user left "Send to the Downloads
    // window" checked.
    const opened = useDownloadManager ? openDownloadWindow() : null

    // Popup path: hand the whole batch off and return immediately — the
    // popup owns fetching/writing every file from here on, independent of
    // this tab. Its progress/completion arrive via the
    // listenForDownloadWindowMessages relay set up above.
    if (opened) {
      try {
        const detail = await getDownload(apiKey, d.id)
        const files = detail.files.filter((f) => f.provider_file_id)
        if (files.length === 0) {
          alert(detail.files_error ? `Couldn't get this download's files: ${detail.files_error}` : 'No files available to download yet.')
          return
        }
        markBusy(d.id)
        setProgressFor(d.id, { loaded: 0, total: files.reduce((sum, f) => sum + f.size_bytes, 0) })
        await sendBatchToDownloadWindow({
          downloadId: d.id,
          downloadName: d.name,
          directoryHandle: folder,
          files: files.map((f) => ({ path: f.path, providerFileId: f.provider_file_id as string, sizeBytes: f.size_bytes })),
        })
      } catch (err) {
        alert(err instanceof Error ? err.message : String(err))
        clearBusy(d.id)
        clearProgressFor(d.id)
      }
      return
    }

    // In-tab streaming: either the checkbox was unchecked, or the popup was
    // blocked — either way, stream straight to the chosen folder here.
    markBusy(d.id)
    try {
      const detail = await getDownload(apiKey, d.id)
      const files = detail.files.filter((f) => f.provider_file_id)
      if (files.length === 0) {
        alert(detail.files_error ? `Couldn't get this download's files: ${detail.files_error}` : 'No files available to download yet.')
        return
      }

      const totalBytes = files.reduce((sum, f) => sum + f.size_bytes, 0)
      let loadedBytes = 0
      setProgressFor(d.id, { loaded: 0, total: totalBytes })

      const failed: string[] = []
      for (const f of files) {
        try {
          const { url } = await getFileLink(apiKey, d.id, f.provider_file_id as string)
          const resp = await fetch(url)
          if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
          await writeFileToDirectory(folder, f.path, resp, (chunkBytes) => {
            loadedBytes += chunkBytes
            setProgressFor(d.id, { loaded: loadedBytes, total: totalBytes })
          })
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
      clearBusy(d.id)
      clearProgressFor(d.id)
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
            busyIds={busyIds}
            downloadProgress={downloadProgress}
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
            busyIds={busyIds}
            downloadProgress={downloadProgress}
            onSelect={(d) => setSelectedId(d.id)}
            allowRetry={false}
            showCategory={false}
            emptyMessage="No manual downloads yet. Add one with the button above, or add it directly through TorBox — it'll show up here automatically."
          />
        )}
        {view === 'settings' && <Settings apiKey={apiKey} onApiKeyChanged={handleApiKeyChanged} />}
      </main>

      {selectedId && (
        <DownloadDetail
          apiKey={apiKey}
          id={selectedId}
          onClose={() => setSelectedId(null)}
          onDownloadAll={handleDownloadAll}
          busy={busyIds.has(selectedId)}
          progress={downloadProgress[selectedId]}
        />
      )}
      {downloadDialogFor && (
        <DownloadOptionsDialog
          download={downloadDialogFor}
          onClose={() => setDownloadDialogFor(null)}
          onConfirm={(opts) => startDownloadAll(downloadDialogFor, opts)}
        />
      )}
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
