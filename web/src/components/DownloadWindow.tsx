import { useEffect, useRef, useState } from 'react'
import { getFileLink } from '../api'
import { DEFAULT_IDLE_TIMEOUT_MS, fetchWithIdleTimeout, queryWritePermission, requestWritePermission, writeFileToDirectory } from '../fsAccess'
import { DOWNLOAD_CHANNEL_NAME } from '../downloadWindowProtocol'
import type { AddBatchMessage, BatchCompleteMessage, BatchProgressMessage, FailedFile, ToPopupMessage } from '../downloadWindowProtocol'
import { formatBytes } from '../format'
import '../App.css'

type BatchState = {
  downloadId: string
  downloadName: string
  loaded: number
  total: number
  filesDone: number
  filesTotal: number
  failed: FailedFile[]
  status: 'needs-permission' | 'downloading' | 'done' | 'error' | 'stopped'
}

// Guards against two Downloads popups both actually processing batches —
// see downloadWindowProtocol.ts: two independently-opened AcerviNode tabs
// can end up in different browsing-context groups, where window.open()'s
// "reuse by name" trick can't see an already-open popup and spawns a
// genuine second one. Only the instance holding this lock (acquired for its
// whole lifetime, released automatically when the tab closes) ever
// registers a channel listener or touches a batch; any other instance just
// shows a "already open elsewhere" message.
const DOWNLOAD_LOCK_NAME = 'acervinode-downloads-singleton'

function initialBatchState(batch: AddBatchMessage, totalBytes: number, status: BatchState['status']): BatchState {
  return {
    downloadId: batch.downloadId,
    downloadName: batch.downloadName,
    loaded: 0,
    total: totalBytes,
    filesDone: 0,
    filesTotal: batch.files.length,
    failed: [],
    status,
  }
}

// The small popup window openDownloadWindow() (see fsAccess.ts) opens —
// rendered instead of the normal <App> when the URL has ?popup=downloads
// (see main.tsx). Its whole job: receive a download's files over a shared
// BroadcastChannel from any AcerviNode tab, fetch and stream them to the
// already-picked folder itself, and keep doing that independent of whatever
// happens to any main tab afterward (closed, navigated away, whatever) —
// this window is what actually survives that, not the main tab.
export function DownloadWindow() {
  const [batches, setBatches] = useState<Record<string, BatchState>>({})
  // null while the singleton lock is still being requested; true once this
  // instance holds it (the only state where it's actually the active
  // popup); false if another instance already holds it.
  const [isPrimary, setIsPrimary] = useState<boolean | null>(null)
  // Batches currently being processed — general protection against
  // handling the same add-batch message twice (e.g. a duplicate
  // broadcast), and against a stale re-click while one's still running.
  // Cleared the moment a batch finishes, however it finishes (done, error,
  // or stopped), so re-downloading afterward always works.
  const processing = useRef<Set<string>>(new Set())
  // Batches parked waiting on a "Grant folder access" click — see
  // handleBatch below. Needs the original message (for its files/
  // directoryHandle) plus the size total already computed for it.
  const pending = useRef<Record<string, { batch: AddBatchMessage; totalBytes: number }>>({})
  // The most recent add-batch message per download id, kept around after
  // completion so "Retry failed" can rebuild a batch containing just the
  // files that didn't make it, without the main window having to resend
  // anything.
  const lastBatch = useRef<Record<string, AddBatchMessage>>({})
  // One AbortController per in-flight batch — Stop calls .abort() on it;
  // processBatch checks it between (and cancels mid-) files.
  const controllers = useRef<Record<string, AbortController>>({})
  const channelRef = useRef<BroadcastChannel | null>(null)
  const baseTitleRef = useRef(document.title)

  // window.focus() called on ourselves from a BroadcastChannel handler (no
  // direct user gesture in this context) is exactly the "popup keeps
  // stealing focus" pattern Chromium deliberately blocks, so it's
  // best-effort only — it might do nothing. Flashing the title is the
  // fallback that actually always works, no permission needed: it changes
  // what's visible in the taskbar/alt-tab switcher even while genuinely
  // backgrounded, and clears itself the moment the user does bring this
  // window to the front.
  useEffect(() => {
    function onFocus() {
      document.title = baseTitleRef.current
    }
    window.addEventListener('focus', onFocus)
    return () => window.removeEventListener('focus', onFocus)
  }, [])

  function bumpToForeground() {
    window.focus()
    if (!document.hasFocus()) {
      document.title = `🔴 New download — ${baseTitleRef.current}`
    }
  }

  async function handleBatch(batch: AddBatchMessage) {
    if (processing.current.has(batch.downloadId)) return
    processing.current.add(batch.downloadId)
    lastBatch.current[batch.downloadId] = batch
    bumpToForeground()

    const totalBytes = batch.files.reduce((sum, f) => sum + f.sizeBytes, 0)

    // A directory handle's write grant is checked per top-level browsing
    // context, not just per origin — the main window already had it
    // granted, but that doesn't carry over to this popup automatically,
    // even though it's the very same handle (structured-clone-transferred).
    // Asking for it here needs this window's own user gesture, so if it's
    // not already granted, processing pauses on a "Grant folder access"
    // button instead of failing every file silently.
    const state = await queryWritePermission(batch.directoryHandle)
    if (state === 'granted') {
      setBatches((prev) => ({ ...prev, [batch.downloadId]: initialBatchState(batch, totalBytes, 'downloading') }))
      processBatch(batch, totalBytes)
    } else {
      pending.current[batch.downloadId] = { batch, totalBytes }
      setBatches((prev) => ({ ...prev, [batch.downloadId]: initialBatchState(batch, totalBytes, 'needs-permission') }))
    }
  }

  async function grantAccess(downloadId: string) {
    const entry = pending.current[downloadId]
    if (!entry) return
    const state = await requestWritePermission(entry.batch.directoryHandle)
    if (state !== 'granted') {
      processing.current.delete(downloadId)
      setBatches((prev) => ({
        ...prev,
        [downloadId]: { ...prev[downloadId], status: 'error', failed: [{ path: '(all files)', error: 'Folder access was denied.' }] },
      }))
      return
    }
    delete pending.current[downloadId]
    setBatches((prev) => ({ ...prev, [downloadId]: { ...prev[downloadId], status: 'downloading' } }))
    processBatch(entry.batch, entry.totalBytes)
  }

  async function processBatch(batch: AddBatchMessage, totalBytes: number) {
    // '' — this popup is always same-origin with the main AcerviNode tab
    // that opened it, so it shares the same session cookie automatically;
    // no need to hold or pass the API key around separately (see api.ts's
    // request()).
    const controller = new AbortController()
    controllers.current[batch.downloadId] = controller

    let loaded = 0
    let filesDone = 0
    const failed: FailedFile[] = []

    for (const f of batch.files) {
      if (controller.signal.aborted) break
      try {
        const { url } = await getFileLink('', batch.downloadId, f.providerFileId)
        if (controller.signal.aborted) break
        const resp = await fetchWithIdleTimeout(url, DEFAULT_IDLE_TIMEOUT_MS, controller.signal)
        if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
        await writeFileToDirectory(batch.directoryHandle, f.path, resp, (chunkBytes) => {
          loaded += chunkBytes
          setBatches((prev) => (prev[batch.downloadId] ? { ...prev, [batch.downloadId]: { ...prev[batch.downloadId], loaded } } : prev))
          reportProgress(batch.downloadId, loaded, totalBytes)
        })
      } catch (err) {
        if (controller.signal.aborted) break // deliberate Stop, not a real failure
        const message = err instanceof Error ? err.message : String(err)
        console.error(`Failed to download ${f.path}`, err)
        failed.push({ path: f.path, error: message })
      }
      filesDone++
      setBatches((prev) => (prev[batch.downloadId] ? { ...prev, [batch.downloadId]: { ...prev[batch.downloadId], filesDone } } : prev))
    }

    const stopped = controller.signal.aborted
    delete controllers.current[batch.downloadId]
    processing.current.delete(batch.downloadId)

    setBatches((prev) =>
      prev[batch.downloadId]
        ? { ...prev, [batch.downloadId]: { ...prev[batch.downloadId], status: stopped ? 'stopped' : failed.length > 0 ? 'error' : 'done', failed } }
        : prev,
    )
    // Always the real list, even when stopped — a file that genuinely
    // failed before the Stop click is a real failure regardless of what
    // happened afterward, not something to hide just because the batch
    // didn't run to completion. (A stop with nothing having failed yet
    // naturally has an empty failed here already, so this isn't a
    // regression for the "just stopped, nothing wrong" case.)
    reportComplete(batch.downloadId, failed)
  }

  function stopBatch(downloadId: string) {
    controllers.current[downloadId]?.abort()
  }

  // Rebuilds a batch containing just the files that failed last time and
  // reprocesses it under the same download id — reuses handleBatch's own
  // permission check rather than assuming access is still granted.
  function retryFailed(downloadId: string) {
    const original = lastBatch.current[downloadId]
    const current = batches[downloadId]
    if (!original || !current || current.failed.length === 0) return
    const failedPaths = new Set(current.failed.map((f) => f.path))
    const retryBatch: AddBatchMessage = { ...original, files: original.files.filter((f) => failedPaths.has(f.path)) }
    if (retryBatch.files.length === 0) return
    handleBatch(retryBatch)
  }

  // Rebuilds a batch from whatever a Stop left outstanding: everything
  // after filesDone was never attempted at all (processBatch always
  // processes files in order and breaks the instant it's aborted, so
  // filesDone is exactly how many files at the start of the array were
  // attempted), PLUS anything at or before that point that genuinely
  // failed rather than succeeded — filesDone counts attempts, not
  // successes, so a real failure earlier in the batch would otherwise look
  // "done" and never get retried. The one interrupted mid-write is redone
  // from scratch either way; there's no byte-offset resume yet (see the
  // pause/resume roadmap item).
  function retryStopped(downloadId: string) {
    const original = lastBatch.current[downloadId]
    const current = batches[downloadId]
    if (!original || !current) return
    const failedPaths = new Set(current.failed.map((f) => f.path))
    const remaining = original.files.filter((f, i) => i >= current.filesDone || failedPaths.has(f.path))
    if (remaining.length === 0) return
    handleBatch({ ...original, files: remaining })
  }

  function dismissBatch(downloadId: string) {
    setBatches((prev) => {
      if (!(downloadId in prev)) return prev
      const next = { ...prev }
      delete next[downloadId]
      return next
    })
    delete lastBatch.current[downloadId]
    delete pending.current[downloadId]
  }

  // Both report functions are best-effort: broadcasting works fine with
  // zero listeners (every main tab may be closed), which is the whole point
  // of this window existing.
  function reportProgress(downloadId: string, loaded: number, total: number) {
    const msg: BatchProgressMessage = { type: 'batch-progress', downloadId, loaded, total }
    channelRef.current?.postMessage(msg)
  }

  function reportComplete(downloadId: string, failed: FailedFile[]) {
    const msg: BatchCompleteMessage = { type: 'batch-complete', downloadId, failed }
    channelRef.current?.postMessage(msg)
  }

  useEffect(() => {
    let cancelled = false

    // ifAvailable: true resolves the callback with null immediately instead
    // of queueing if another instance already holds the lock — exactly the
    // "is anyone else already the active Downloads window?" check needed
    // here. The lock is held for as long as the callback's promise stays
    // pending, i.e. for this window's whole lifetime (never resolved on
    // purpose — the browser releases it automatically when the tab closes).
    navigator.locks.request(DOWNLOAD_LOCK_NAME, { ifAvailable: true }, (lock) => {
      if (cancelled) return Promise.resolve()
      if (!lock) {
        setIsPrimary(false)
        return Promise.resolve()
      }
      setIsPrimary(true)

      const ch = new BroadcastChannel(DOWNLOAD_CHANNEL_NAME)
      channelRef.current = ch

      function onMessage(e: MessageEvent<ToPopupMessage>) {
        switch (e.data?.type) {
          case 'ping':
            ch.postMessage({ type: 'popup-ready' })
            break
          case 'focus-request':
            bumpToForeground()
            break
          case 'add-batch':
            handleBatch(e.data)
            break
        }
      }
      ch.addEventListener('message', onMessage)
      // Announce readiness immediately too, in case a main window is
      // already waiting on a ping sent just before this listener attached.
      ch.postMessage({ type: 'popup-ready' })

      return new Promise<void>(() => {
        // Deliberately never resolves — see the comment above.
      })
    })

    return () => {
      cancelled = true
      channelRef.current?.close()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // A duplicate (lost the singleton lock race) tries to close itself right
  // away — window.close() only works on a window script actually opened
  // (true here, via openDownloadWindow()'s window.open() call), but some
  // browsers/settings still refuse it silently with no error, so the
  // message and manual button below stay as the fallback either way.
  useEffect(() => {
    if (isPrimary === false) window.close()
  }, [isPrimary])

  if (isPrimary === false) {
    return (
      <div className="download-window">
        <h1 className="download-window-title">📦 Downloads</h1>
        <p className="empty">
          Another Downloads window is already open and is the one actually tracking your downloads — this extra
          copy isn't doing anything. Closing automatically…
        </p>
        <button onClick={() => window.close()}>Close this window</button>
      </div>
    )
  }

  const active = Object.values(batches)

  return (
    <div className="download-window">
      <h1 className="download-window-title">📦 Downloads</h1>
      {active.length === 0 ? (
        <p className="empty">
          Nothing here yet — this window fills up automatically when you download files from any AcerviNode
          window. You can leave it open (even minimized) and close the main tab; downloads already running here
          keep going.
        </p>
      ) : (
        <ul className="download-window-list">
          {active.map((b) => {
            const pct = b.total > 0 ? Math.min(100, Math.round((b.loaded / b.total) * 100)) : 0
            const canDismiss = b.status === 'done' || b.status === 'error' || b.status === 'stopped' || b.status === 'needs-permission'
            return (
              <li key={b.downloadId} className="download-window-item">
                <div className="download-window-item-header">
                  <div className="download-window-item-name" title={b.downloadName}>
                    {b.downloadName}
                  </div>
                  {b.status === 'downloading' && (
                    <button className="download-window-icon-btn stop" onClick={() => stopBatch(b.downloadId)} title="Stop this download">
                      ⏹
                    </button>
                  )}
                  {b.status === 'stopped' && (
                    <button className="download-window-icon-btn retry" onClick={() => retryStopped(b.downloadId)} title="Retry remaining files">
                      ↻
                    </button>
                  )}
                  {canDismiss && (
                    <button className="download-window-icon-btn dismiss" onClick={() => dismissBatch(b.downloadId)} title="Dismiss">
                      ✕
                    </button>
                  )}
                </div>
                {b.status === 'needs-permission' ? (
                  <div className="download-window-item-meta">
                    <p>This window needs its own permission to write to your download folder.</p>
                    <button onClick={() => grantAccess(b.downloadId)}>Grant folder access</button>
                  </div>
                ) : (
                  <>
                    <div className="progress-track">
                      <div className="progress-fill" style={{ width: `${pct}%` }} />
                    </div>
                    <div className="download-window-item-meta">
                      {b.status === 'downloading' && (
                        <span>
                          {formatBytes(b.loaded)} / {formatBytes(b.total)} · {b.filesDone}/{b.filesTotal} files
                        </span>
                      )}
                      {b.status === 'done' && <span className="settings-success">Done — {b.filesTotal} files</span>}
                      {b.status === 'stopped' && (
                        <span className="text-muted">
                          Stopped — {b.filesDone}/{b.filesTotal} files
                          {b.failed.length > 0 && ` (${b.failed.length} failed before stopping)`}
                        </span>
                      )}
                      {b.status === 'error' && (
                        <span className="settings-error">
                          {b.failed.length > 0 ? `${b.failed.length} of ${b.filesTotal} file(s) failed` : 'Failed'}
                        </span>
                      )}
                    </div>
                    {(b.status === 'error' || b.status === 'stopped') && b.failed.length > 0 && (
                      <ul className="download-window-item-errors">
                        {b.failed.map((f) => (
                          <li key={f.path}>
                            <span title={f.path}>{f.path}</span>: {f.error}
                          </li>
                        ))}
                      </ul>
                    )}
                    {b.status === 'error' && b.failed.length > 0 && (
                      <button className="download-window-retry-btn" onClick={() => retryFailed(b.downloadId)}>
                        ↻ Retry failed ({b.failed.length})
                      </button>
                    )}
                  </>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
