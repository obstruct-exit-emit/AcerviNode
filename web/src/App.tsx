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
  listenForDownloadWindowMessages,
  openDownloadWindow,
  sendBatchToDownloadWindow,
  supportsDirectoryPicker,
  writeFileToDirectory,
} from './fsAccess'
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
  // Set while the DownloadOptionsDialog is open for a given row (streamed-
  // to-folder path only) — lets the user see/change the destination folder
  // and choose whether to hand off to the Downloads popup window before
  // anything actually starts. See handleDownloadAll/startStreamedDownload.
  const [downloadDialogFor, setDownloadDialogFor] = useState<Download | null>(null)
  const [downloadingAllId, setDownloadingAllId] = useState<string | null>(null)
  // Cumulative bytes written across every file in the batch currently
  // streaming to disk (the File System Access path only — see
  // handleDownloadAllIndividual). null while nothing's downloading, or once
  // a download starts if total size wasn't knowable up front.
  const [downloadProgress, setDownloadProgress] = useState<{ loaded: number; total: number } | null>(null)

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

  // Relays the Downloads popup window's progress back into this row's own
  // progress bar — best-effort and, for now, single-row: if two downloads
  // are both running in the popup at once, this only ever reflects whichever
  // sent the most recent message. The popup itself always shows every batch
  // at once regardless, so nothing is actually lost, just not mirrored here.
  useEffect(() => {
    return listenForDownloadWindowMessages((msg) => {
      if (msg.type === 'batch-progress') {
        setDownloadingAllId(msg.downloadId)
        setDownloadProgress({ loaded: msg.loaded, total: msg.total })
      } else if (msg.type === 'batch-complete') {
        setDownloadingAllId((current) => (current === msg.downloadId ? null : current))
        setDownloadProgress(null)
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

  // The per-row "Download all" button's entry point — dispatches on the
  // user's Settings > Downloads preference (see preferences.getDownloadMode).
  // 'zip' resolves the whole download as one provider-zipped archive. The
  // default 'individual' either opens DownloadOptionsDialog (browsers that
  // support streaming to a folder — see startStreamedDownload, which the
  // dialog's confirm triggers) or, if this browser can't do that at all,
  // falls straight to handleDownloadAllIndividual's plain tab-per-file
  // behavior, since there's no folder/Downloads-window choice to make there
  // anyway. Either way, the detail view's own explicit "Download all
  // (zip)"/per-file buttons remain available regardless of this preference —
  // it only controls the row button's default action.
  async function handleDownloadAll(d: Download) {
    if (!apiKey) return
    if (getDownloadMode() === 'zip') {
      await handleDownloadAllZip(d)
    } else if (supportsDirectoryPicker()) {
      setDownloadDialogFor(d)
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

  // No-File-System-Access fallback (Firefox/Safari, or any browser without
  // showDirectoryPicker): opens every file's link in its own tab. Nothing
  // to choose here — no folder, no Downloads window — so this never goes
  // through DownloadOptionsDialog.
  async function handleDownloadAllIndividual(d: Download) {
    if (!apiKey) return
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
          window.open(url, '_blank', 'noopener,noreferrer')
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

  // Triggered by DownloadOptionsDialog's confirm — folder and the
  // Downloads-window choice are already decided by then, so this only ever
  // streams (never a tab-per-file fallback; that path never opens the
  // dialog in the first place — see handleDownloadAll).
  async function startStreamedDownload(d: Download, opts: DownloadOptions) {
    if (!apiKey) return

    // Deliberately the first statement, before any await — window.open()
    // is synchronous, and this needs the dialog confirm button's own click
    // gesture, same requirement as showDirectoryPicker(). Only actually
    // opens/focuses the popup when the user left "Send to the Downloads
    // window" checked.
    const opened = opts.useDownloadManager ? openDownloadWindow() : null

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
        setDownloadingAllId(d.id)
        setDownloadProgress({ loaded: 0, total: files.reduce((sum, f) => sum + f.size_bytes, 0) })
        await sendBatchToDownloadWindow(opened, {
          downloadId: d.id,
          downloadName: d.name,
          directoryHandle: opts.folder,
          files: files.map((f) => ({ path: f.path, providerFileId: f.provider_file_id as string, sizeBytes: f.size_bytes })),
        })
      } catch (err) {
        alert(err instanceof Error ? err.message : String(err))
        setDownloadingAllId(null)
        setDownloadProgress(null)
      }
      return
    }

    // In-tab streaming: either the checkbox was unchecked, or the popup was
    // blocked — either way, stream straight to the chosen folder here.
    setDownloadingAllId(d.id)
    try {
      const detail = await getDownload(apiKey, d.id)
      const files = detail.files.filter((f) => f.provider_file_id)
      if (files.length === 0) {
        alert(detail.files_error ? `Couldn't get this download's files: ${detail.files_error}` : 'No files available to download yet.')
        return
      }

      const totalBytes = files.reduce((sum, f) => sum + f.size_bytes, 0)
      let loadedBytes = 0
      setDownloadProgress({ loaded: 0, total: totalBytes })

      const failed: string[] = []
      for (const f of files) {
        try {
          const { url } = await getFileLink(apiKey, d.id, f.provider_file_id as string)
          const resp = await fetch(url)
          if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
          await writeFileToDirectory(opts.folder, f.path, resp, (chunkBytes) => {
            loadedBytes += chunkBytes
            setDownloadProgress({ loaded: loadedBytes, total: totalBytes })
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
      setDownloadingAllId(null)
      setDownloadProgress(null)
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
            downloadingAllId={downloadingAllId}
            downloadProgress={downloadProgress}
            onSelect={(d) => setSelectedId(d.id)}
            allowRetry={false}
            showCategory={false}
            emptyMessage="No manual downloads yet. Add one with the button above, or add it directly through TorBox — it'll show up here automatically."
          />
        )}
        {view === 'settings' && <Settings apiKey={apiKey} onApiKeyChanged={handleApiKeyChanged} />}
      </main>

      {selectedId && <DownloadDetail apiKey={apiKey} id={selectedId} onClose={() => setSelectedId(null)} />}
      {downloadDialogFor && (
        <DownloadOptionsDialog
          download={downloadDialogFor}
          onClose={() => setDownloadDialogFor(null)}
          onConfirm={(opts) => startStreamedDownload(downloadDialogFor, opts)}
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
