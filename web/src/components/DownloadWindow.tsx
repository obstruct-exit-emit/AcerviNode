import { useEffect, useRef, useState } from 'react'
import { getFileLink, getStoredApiKey } from '../api'
import { queryWritePermission, requestWritePermission, writeFileToDirectory } from '../fsAccess'
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
  status: 'needs-permission' | 'downloading' | 'done' | 'error'
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
  // Batches already being processed — general protection against handling
  // the same add-batch message twice (e.g. a duplicate broadcast).
  const processing = useRef<Set<string>>(new Set())
  // Batches parked waiting on a "Grant folder access" click — see
  // handleBatch below. Needs the original message (for its files/
  // directoryHandle) plus the size total already computed for it.
  const pending = useRef<Record<string, { batch: AddBatchMessage; totalBytes: number }>>({})
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

  useEffect(() => {
    let cancelled = false

    async function handleBatch(batch: AddBatchMessage) {
      if (processing.current.has(batch.downloadId)) return
      processing.current.add(batch.downloadId)
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

  async function grantAccess(downloadId: string) {
    const entry = pending.current[downloadId]
    if (!entry) return
    const state = await requestWritePermission(entry.batch.directoryHandle)
    if (state !== 'granted') {
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
    const apiKey = getStoredApiKey()
    if (!apiKey) {
      setBatches((prev) => ({
        ...prev,
        [batch.downloadId]: { ...prev[batch.downloadId], status: 'error', failed: [{ path: '(all files)', error: 'Not signed in to AcerviNode in this window.' }] },
      }))
      return
    }

    let loaded = 0
    let filesDone = 0
    const failed: FailedFile[] = []

    for (const f of batch.files) {
      try {
        const { url } = await getFileLink(apiKey, batch.downloadId, f.providerFileId)
        const resp = await fetch(url)
        if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
        await writeFileToDirectory(batch.directoryHandle, f.path, resp, (chunkBytes) => {
          loaded += chunkBytes
          setBatches((prev) => (prev[batch.downloadId] ? { ...prev, [batch.downloadId]: { ...prev[batch.downloadId], loaded } } : prev))
          reportProgress(batch.downloadId, loaded, totalBytes)
        })
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err)
        console.error(`Failed to download ${f.path}`, err)
        failed.push({ path: f.path, error: message })
      }
      filesDone++
      setBatches((prev) => (prev[batch.downloadId] ? { ...prev, [batch.downloadId]: { ...prev[batch.downloadId], filesDone } } : prev))
    }

    setBatches((prev) =>
      prev[batch.downloadId] ? { ...prev, [batch.downloadId]: { ...prev[batch.downloadId], status: failed.length > 0 ? 'error' : 'done', failed } } : prev,
    )
    reportComplete(batch.downloadId, failed)
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
            return (
              <li key={b.downloadId} className="download-window-item">
                <div className="download-window-item-name" title={b.downloadName}>
                  {b.downloadName}
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
                      {b.status === 'error' && (
                        <span className="settings-error">
                          {b.failed.length > 0 ? `${b.failed.length} of ${b.filesTotal} file(s) failed` : 'Failed'}
                        </span>
                      )}
                    </div>
                    {b.status === 'error' && b.failed.length > 0 && (
                      <ul className="download-window-item-errors">
                        {b.failed.map((f) => (
                          <li key={f.path}>
                            <span title={f.path}>{f.path}</span>: {f.error}
                          </li>
                        ))}
                      </ul>
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
