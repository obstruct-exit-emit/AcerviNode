import { useEffect, useRef, useState } from 'react'
import { getFileLink, getStoredApiKey } from '../api'
import { queryWritePermission, requestWritePermission, writeFileToDirectory } from '../fsAccess'
import type { AddBatchMessage, BatchCompleteMessage, BatchProgressMessage, FailedFile } from '../downloadWindowProtocol'
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

// The small popup window openDownloadWindow() (see fsAccess.ts) opens —
// rendered instead of the normal <App> when the URL has ?popup=downloads
// (see main.tsx). Its whole job: receive a download's files over
// postMessage from the main window, fetch and stream them to the
// already-picked folder itself, and keep doing that independent of whatever
// happens to the main tab afterward (closed, navigated away, whatever) —
// this window is what actually survives that, not the main tab.
export function DownloadWindow() {
  const [batches, setBatches] = useState<Record<string, BatchState>>({})
  // Batches already being processed — a StrictMode double-effect-run guard,
  // and general protection against handling the same add-batch message twice.
  const processing = useRef<Set<string>>(new Set())
  // Batches parked waiting on a "Grant folder access" click — see onMessage
  // below. Needs the original message (for its files/directoryHandle) plus
  // the size total already computed for it.
  const pending = useRef<Record<string, { batch: AddBatchMessage; totalBytes: number }>>({})

  useEffect(() => {
    // Tells the opener it's safe to postMessage batches now — sent once,
    // right after this listener is registered, so nothing posted before
    // this point is ever lost waiting for a page that hadn't loaded yet.
    window.opener?.postMessage({ type: 'popup-ready' }, window.location.origin)

    async function onMessage(e: MessageEvent<AddBatchMessage>) {
      if (e.origin !== window.location.origin) return
      if (e.data?.type !== 'add-batch') return
      const batch = e.data
      if (processing.current.has(batch.downloadId)) return
      processing.current.add(batch.downloadId)

      const totalBytes = batch.files.reduce((sum, f) => sum + f.sizeBytes, 0)

      // A directory handle's write grant is checked per top-level browsing
      // context, not just per origin — the main window already had it
      // granted, but that doesn't carry over to this popup automatically,
      // even though it's the very same handle (postMessage-cloned). Asking
      // for it here needs this window's own user gesture, so if it's not
      // already granted, processing pauses on a "Grant folder access"
      // button instead of failing every file silently.
      const state = await queryWritePermission(batch.directoryHandle)
      if (state === 'granted') {
        setBatches((prev) => ({
          ...prev,
          [batch.downloadId]: {
            downloadId: batch.downloadId,
            downloadName: batch.downloadName,
            loaded: 0,
            total: totalBytes,
            filesDone: 0,
            filesTotal: batch.files.length,
            failed: [],
            status: 'downloading',
          },
        }))
        processBatch(batch, totalBytes)
      } else {
        pending.current[batch.downloadId] = { batch, totalBytes }
        setBatches((prev) => ({
          ...prev,
          [batch.downloadId]: {
            downloadId: batch.downloadId,
            downloadName: batch.downloadName,
            loaded: 0,
            total: totalBytes,
            filesDone: 0,
            filesTotal: batch.files.length,
            failed: [],
            status: 'needs-permission',
          },
        }))
      }
    }

    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
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

  // Both report functions are best-effort: window.opener is null once the
  // main tab that opened this popup has closed, which is the whole point of
  // this window existing — there's simply nobody to relay progress to at
  // that point, and that's fine, this window doesn't need it either.
  function reportProgress(downloadId: string, loaded: number, total: number) {
    const msg: BatchProgressMessage = { type: 'batch-progress', downloadId, loaded, total }
    window.opener?.postMessage(msg, window.location.origin)
  }

  function reportComplete(downloadId: string, failed: FailedFile[]) {
    const msg: BatchCompleteMessage = { type: 'batch-complete', downloadId, failed }
    window.opener?.postMessage(msg, window.location.origin)
  }

  const active = Object.values(batches)

  return (
    <div className="download-window">
      <h1 className="download-window-title">📦 Downloads</h1>
      {active.length === 0 ? (
        <p className="empty">
          Nothing here yet — this window fills up automatically when you download files from the main AcerviNode
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
