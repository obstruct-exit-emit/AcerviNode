import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ApiError,
  deleteDownload,
  getAuthStatus,
  getDownload,
  getFileLink,
  getProviders,
  getSetupStatus,
  getStatus,
  getVersion,
  getZipLink,
  listDownloads,
  logout as apiLogout,
  retryDownload,
  type AuthStatus,
  type Download,
  type ProviderStatus,
  type StatusInfo,
} from './api'
import { AddDownload } from './components/AddDownload'
import { BulkActionBar } from './components/BulkActionBar'
import { DownloadDetail } from './components/DownloadDetail'
import { DownloadOptionsDialog, type DownloadOptions } from './components/DownloadOptionsDialog'
import { DownloadsTable } from './components/DownloadsTable'
import { LoginForm } from './components/LoginForm'
import { ProviderBadges } from './components/ProviderBadges'
import { Settings } from './components/Settings'
import SetupWizard from './components/SetupWizard'
import {
  DEFAULT_IDLE_TIMEOUT_MS,
  fetchWithIdleTimeout,
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


// rateLimitPausedUntil returns the furthest-out still-active provider
// rate-limit cooldown across every kind, or undefined when none is active.
//
// While a kind is in cooldown the importer skips its provider listing
// entirely (see internal/importer's refreshKind), so every download of that
// kind stops advancing — progress, state and speed all freeze at whatever
// was last seen. Without something saying so, that is indistinguishable
// from AcerviNode being broken, which is exactly how it gets reported. The
// limit is enforced per API key across the provider's servers, so in
// practice all three kinds trip together and one banner covers it.
function rateLimitPausedUntil(status: StatusInfo | undefined): Date | undefined {
  if (!status) return undefined
  let latest: Date | undefined
  for (const kind of Object.values(status.kinds ?? {})) {
    if (!kind.rate_limited_until) continue
    const until = new Date(kind.rate_limited_until)
    if (until.getTime() <= Date.now()) continue
    if (!latest || until > latest) latest = until
  }
  return latest
}

export default function App() {
  const [auth, setAuth] = useState<AuthStatus | null>(null)
  const [setupNeeded, setSetupNeeded] = useState<boolean | null>(null)
  const [view, setView] = useState<View>('manual')
  const [version, setVersion] = useState<string>('')
  // Set once, on the first successful poll — the baseline this tab loaded
  // with. Compared against every later poll's version so a deploy while the
  // tab is already open (it's a long-lived SPA; nothing else ever re-fetches
  // its own JS) surfaces an "Update available" prompt instead of silently
  // running stale indefinitely. A prompt, not a forced reload — this tab
  // could be sitting on a filled-out Add-download form, or mid-edit in
  // Settings, when a deploy happens elsewhere; the user decides when to
  // reload, same reasoning as (and deliberately not applied to) the
  // Downloads popup window, where a forced reload mid-transfer would
  // actually interrupt an in-progress download.
  const initialVersionRef = useRef<string | null>(null)
  const [updateAvailable, setUpdateAvailable] = useState(false)
  const [providers, setProviders] = useState<ProviderStatus[]>([])
  // Both tabs' downloads are kept loaded regardless of which is active, so
  // switching tabs is instant and doesn't need its own loading state.
  const [managedDownloads, setManagedDownloads] = useState<Download[]>([])
  const [manualDownloads, setManualDownloads] = useState<Download[]>([])
  const [loadError, setLoadError] = useState<string | undefined>(undefined)
  // Polled alongside the downloads themselves so the tables can explain a
  // stall rather than just showing frozen rows — see the rate-limit banner
  // below. Cheap: /api/v1/status is a purely local read, no provider call.
  const [status, setStatus] = useState<StatusInfo | undefined>(undefined)
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
  // Bulk selection for the currently-active tab's table — a single Set
  // rather than one per tab, cleared on every tab switch (see the effect
  // below) so it's always scoped to whichever table is actually visible.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())

  function toggleSelect(id: string) {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  function toggleSelectAll(ids: string[]) {
    setSelectedIds((prev) => {
      const allSelected = ids.length > 0 && ids.every((id) => prev.has(id))
      return allSelected ? new Set() : new Set(ids)
    })
  }

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

  const refreshAuth = useCallback(() => {
    getAuthStatus()
      .then(setAuth)
      .catch(() => setAuth({ auth_enabled: true, authenticated: false }))
  }, [])

  const handleUnauthorized = useCallback(() => {
    // A session that expired or was revoked server-side — refreshing auth
    // status brings back the login form on its own, since authenticated
    // will now read false.
    refreshAuth()
  }, [refreshAuth])

  useEffect(() => {
    refreshAuth()
    getSetupStatus()
      .then((s) => setSetupNeeded(s.needed))
      .catch(() => setSetupNeeded(false))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Login is mandatory — there is no API-key-only way into the web UI (the
  // API key remains the master credential for Sonarr/Radarr/scripts hitting
  // the native API and compat shims directly, unaffected by any of this).
  // ready means signed in via session; activeKey is always '' since every
  // authenticated browser call relies on the session cookie instead (see
  // api.ts's request()).
  const ready = auth?.authenticated ?? false
  const activeKey = ''
  // A member's access is scoped to Manual downloads only (see
  // docs/providers.md#roles).
  const isAdmin = auth?.role === 'admin'

  const refresh = useCallback(
    async (key: string) => {
      try {
        const [v, p, managed, manual, st] = await Promise.all([
          getVersion(key),
          getProviders(key),
          listDownloads(key, 'arr'),
          listDownloads(key, 'manual'),
          getStatus(key),
        ])
        if (initialVersionRef.current === null) {
          initialVersionRef.current = v.version
        } else if (v.version !== initialVersionRef.current) {
          setUpdateAvailable(true)
        }
        setVersion(v.version)
        setProviders(p)
        setManagedDownloads(managed)
        setManualDownloads(manual)
        setStatus(st)
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
    if (!ready) return
    // A self-rescheduling timeout, not setInterval — see DownloadDetail's
    // identical fix for why: firing on a fixed cadence regardless of
    // whether the previous poll finished lets overlapping requests pile up
    // the moment anything in the chain is briefly slow, instead of the poll
    // just running late. This one's own calls are normally fast (DB reads
    // only), but there's no reason to leave the same footgun in place here
    // too — waiting for refresh() to finish before scheduling the next one
    // costs nothing in the common case and closes it either way.
    let cancelled = false
    let timer: ReturnType<typeof setTimeout>
    async function loop() {
      await refresh(activeKey)
      if (!cancelled) timer = setTimeout(loop, POLL_INTERVAL_MS)
    }
    loop()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [ready, activeKey, refresh])

  // Selection is scoped to whichever table is currently visible — switching
  // tabs starts fresh rather than carrying a Manual-tab selection into the
  // Managed tab's bulk action bar (or vice versa).
  useEffect(() => {
    setSelectedIds(new Set())
  }, [view])

  // Drops any selected id that's no longer present in either list — e.g. a
  // row deleted through a different path (its own row's ✕, or discovered
  // vanished by the importer) while still checked here. Only updates state
  // when something actually needs dropping, to avoid a pointless re-render
  // every single poll.
  useEffect(() => {
    const known = new Set([...managedDownloads, ...manualDownloads].map((d) => d.id))
    setSelectedIds((prev) => {
      let changed = false
      const next = new Set<string>()
      for (const id of prev) {
        if (known.has(id)) next.add(id)
        else changed = true
      }
      return changed ? next : prev
    })
  }, [managedDownloads, manualDownloads])

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

  async function handleDelete(d: Download) {
    if (!ready) return
    if (!confirm(`Delete "${d.name}"? This also removes it from the debrid provider.`)) return
    try {
      await deleteDownload(activeKey, d.id, true)
      refresh(activeKey)
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    }
  }

  async function handleRetry(d: Download) {
    if (!ready) return
    try {
      await retryDownload(activeKey, d.id)
      refresh(activeKey)
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    }
  }

  // Bulk delete/retry both loop the existing single-item endpoint rather
  // than needing a new batch endpoint of their own — same precedent as
  // "Download all" looping the per-file link call instead of a dedicated
  // batch API. Run BULK_CONCURRENCY at a time rather than strictly one at a
  // time — a plain sequential loop made a large selection visibly crawl
  // (each delete/retry is its own round trip, including a real provider-side
  // call), but firing every request at once risked tripping TorBox's own
  // rate limit the same way an unthrottled burst of adds already has
  // elsewhere in this project — a small concurrency cap is the middle
  // ground. One failure doesn't stop the rest; every id is attempted and
  // failures are reported together at the end.
  const BULK_CONCURRENCY = 4

  async function runWithLimit<T>(items: T[], limit: number, task: (item: T) => Promise<unknown>): Promise<T[]> {
    const failed: T[] = []
    let next = 0
    async function worker() {
      while (next < items.length) {
        const item = items[next++]
        try {
          await task(item)
        } catch {
          failed.push(item)
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker))
    return failed
  }

  async function handleBulkDelete(ids: string[]) {
    if (!ready || ids.length === 0) return
    if (!confirm(`Delete ${ids.length} download${ids.length === 1 ? '' : 's'}? This also removes them from the debrid provider.`)) return
    const failed = await runWithLimit(ids, BULK_CONCURRENCY, (id) => deleteDownload(activeKey, id, true))
    setSelectedIds(new Set())
    refresh(activeKey)
    if (failed.length > 0) {
      alert(`${failed.length} of ${ids.length} download(s) failed to delete.`)
    }
  }

  async function handleBulkRetry(ids: string[]) {
    if (!ready || ids.length === 0) return
    const failed = await runWithLimit(ids, BULK_CONCURRENCY, (id) => retryDownload(activeKey, id))
    setSelectedIds(new Set())
    refresh(activeKey)
    if (failed.length > 0) {
      alert(`${failed.length} of ${ids.length} download(s) failed to retry.`)
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
    if (!ready) return
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
    if (!ready) return
    markBusy(d.id)
    try {
      const { url } = await getZipLink(activeKey, d.id)
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
    if (!ready) return
    markBusy(d.id)
    try {
      const detail = await getDownload(activeKey, d.id)
      const files = detail.files.filter((f) => f.provider_file_id)
      if (files.length === 0) {
        alert(detail.files_error ? `Couldn't get this download's files: ${detail.files_error}` : 'No files available to download yet.')
        return
      }
      const failed: string[] = []
      for (const f of files) {
        try {
          const { url } = await getFileLink(activeKey, d.id, f.provider_file_id as string)
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
    if (!ready) return

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
        const detail = await getDownload(activeKey, d.id)
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
      const detail = await getDownload(activeKey, d.id)
      const files = detail.files.filter((f) => f.provider_file_id)
      if (files.length === 0) {
        alert(detail.files_error ? `Couldn't get this download's files: ${detail.files_error}` : 'No files available to download yet.')
        return
      }

      const totalBytes = files.reduce((sum, f) => sum + f.size_bytes, 0)
      let loadedBytes = 0
      setProgressFor(d.id, { loaded: 0, total: totalBytes })

      // A fast connection can deliver a chunk many times a second — updating
      // React state on every single one flooded the whole app with re-
      // renders (this component plus the downloads table) for the entire
      // length of a large download, the actual cause behind reports of the
      // UI stuttering/lagging while a download was active. Throttled to at
      // most 5 updates/second; the byte count itself (loadedBytes) still
      // accumulates every chunk regardless, so nothing is ever lost, and
      // each file's own loop below still ends with one final, unthrottled
      // update so the displayed progress is never stale at a file boundary.
      const PROGRESS_THROTTLE_MS = 200
      let lastProgressUpdate = 0

      const failed: string[] = []
      for (const f of files) {
        try {
          const { url } = await getFileLink(activeKey, d.id, f.provider_file_id as string)
          const resp = await fetchWithIdleTimeout(url, DEFAULT_IDLE_TIMEOUT_MS)
          if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
          await writeFileToDirectory(folder, f.path, resp, (chunkBytes) => {
            loadedBytes += chunkBytes
            const now = Date.now()
            if (now - lastProgressUpdate >= PROGRESS_THROTTLE_MS) {
              lastProgressUpdate = now
              setProgressFor(d.id, { loaded: loadedBytes, total: totalBytes })
            }
          })
          setProgressFor(d.id, { loaded: loadedBytes, total: totalBytes })
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

  // Brief loading state — both auth/status and setup/status are fast, local
  // requests, but rendering nothing meaningful before they answer avoids a
  // flash of the login form for a moment on a genuinely fresh instance.
  if (auth === null || setupNeeded === null) {
    return null
  }

  if (setupNeeded) {
    return (
      <SetupWizard
        onDone={() => {
          setSetupNeeded(false)
          refreshAuth()
        }}
        // A stale setupNeeded=true (e.g. this tab had the wizard open from
        // before the instance was actually claimed elsewhere, or a second
        // tab raced the first) means step 0's own create-account call gets
        // rejected with "already set up" — a dead end otherwise, since the
        // wizard has no way back to the login form on its own. Re-checking
        // here instead of just erroring routes straight to it.
        onAlreadySetUp={() => {
          setSetupNeeded(false)
          refreshAuth()
        }}
      />
    )
  }

  if (!auth.authenticated) {
    return <LoginForm onLoggedIn={refreshAuth} />
  }

  return (
    <div className="app">
      {updateAvailable && (
        <div className="update-banner">
          <span>A new version of AcerviNode is available.</span>
          <button className="update-banner-reload" onClick={() => window.location.reload()}>
            Reload
          </button>
          <button className="update-banner-dismiss" onClick={() => setUpdateAvailable(false)} title="Dismiss">
            ✕
          </button>
        </div>
      )}
      <header className="app-header">
        <h1>📦 AcerviNode</h1>
        <div className="header-meta">
          <ProviderBadges providers={providers} />
          <span className="version">v{version}</span>
          {auth.username && <span className="current-user">{auth.username}</span>}
          <button className="logout-btn" onClick={() => apiLogout().finally(refreshAuth)}>
            Log out
          </button>
        </div>
      </header>

      <nav className="tabs">
        <button className={view === 'manual' ? 'tab tab-active' : 'tab'} onClick={() => setView('manual')}>
          Manual
        </button>
        {isAdmin && (
          <button className={view === 'managed' ? 'tab tab-active' : 'tab'} onClick={() => setView('managed')}>
            Managed
          </button>
        )}
        {isAdmin && (
          <button className={view === 'settings' ? 'tab tab-active' : 'tab'} onClick={() => setView('settings')}>
            Settings
          </button>
        )}
        {view !== 'settings' && (
          <button className="add-download-btn" onClick={() => setAddOpen(true)}>
            {isAdmin && view === 'managed' ? '+ Add to Managed' : '+ Add to Manual'}
          </button>
        )}
      </nav>

      <main>
        {loadError && <p className="load-error">Couldn't reach AcerviNode: {loadError}</p>}
        {view !== 'settings' &&
          (() => {
            const until = rateLimitPausedUntil(status)
            return until ? (
              <p className="poll-paused">
                ⏸ Provider polling is paused until {until.toLocaleTimeString()} — the provider rate-limited this
                account, so progress and state below will look frozen until it clears. Nothing is broken and no
                download is lost; polling resumes on its own.
              </p>
            ) : null
          })()}
        {isAdmin && view === 'managed' && (
          <>
            <BulkActionBar
              count={managedDownloads.filter((d) => selectedIds.has(d.id)).length}
              onDelete={() => handleBulkDelete(managedDownloads.filter((d) => selectedIds.has(d.id)).map((d) => d.id))}
              onRetry={() =>
                handleBulkRetry(
                  managedDownloads.filter((d) => selectedIds.has(d.id) && d.state === 'error').map((d) => d.id),
                )
              }
              retryCount={managedDownloads.filter((d) => selectedIds.has(d.id) && d.state === 'error').length}
              onClear={() => setSelectedIds(new Set())}
            />
            <DownloadsTable
              downloads={managedDownloads}
              onDelete={handleDelete}
              onRetry={handleRetry}
              onDownloadAll={handleDownloadAll}
              busyIds={busyIds}
              downloadProgress={downloadProgress}
              onSelect={(d) => setSelectedId(d.id)}
              selectedIds={selectedIds}
              onToggleSelect={toggleSelect}
              onToggleSelectAll={() => toggleSelectAll(managedDownloads.map((d) => d.id))}
              allowRetry
              showCategory
              emptyMessage="No managed downloads yet. Add one through Sonarr/Radarr, or via the qBittorrent/SABnzbd compat APIs directly."
            />
          </>
        )}
        {view === 'manual' && (
          <>
            <BulkActionBar
              count={manualDownloads.filter((d) => selectedIds.has(d.id)).length}
              onDelete={() => handleBulkDelete(manualDownloads.filter((d) => selectedIds.has(d.id)).map((d) => d.id))}
              onClear={() => setSelectedIds(new Set())}
            />
            <DownloadsTable
              downloads={manualDownloads}
              onDelete={handleDelete}
              onRetry={handleRetry}
              onDownloadAll={handleDownloadAll}
              busyIds={busyIds}
              downloadProgress={downloadProgress}
              onSelect={(d) => setSelectedId(d.id)}
              selectedIds={selectedIds}
              onToggleSelect={toggleSelect}
              onToggleSelectAll={() => toggleSelectAll(manualDownloads.map((d) => d.id))}
              allowRetry={false}
              showCategory={false}
              emptyMessage="No manual downloads yet. Add one with the button above, or add it directly through TorBox — it'll show up here automatically."
            />
          </>
        )}
        {isAdmin && view === 'settings' && <Settings apiKey={activeKey} />}
      </main>

      {selectedId && (
        <DownloadDetail
          apiKey={activeKey}
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
          apiKey={activeKey}
          providers={providers}
          isAdmin={isAdmin}
          defaultManaged={isAdmin && view === 'managed'}
          onClose={() => setAddOpen(false)}
          onAdded={(addedManaged) => {
            setAddOpen(false)
            setView(addedManaged ? 'managed' : 'manual')
            refresh(activeKey)
          }}
        />
      )}
    </div>
  )
}
