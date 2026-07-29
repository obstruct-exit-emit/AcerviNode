import { useEffect, useState } from 'react'
import type { Download } from '../api'
import { getDefaultDirectory, pickAndRememberDirectory } from '../fsAccess'

export interface DownloadOptions {
  folder: FileSystemDirectoryHandle
  useDownloadManager: boolean
}

interface Props {
  download: Download
  onClose: () => void
  onConfirm: (opts: DownloadOptions) => void
}

// Shown before a streamed-to-folder "Download all" actually starts — lets
// the user see (and change) which folder it's about to use instead of it
// silently reusing the remembered default, and choose whether this download
// should hand off to the shared Downloads popup window (survives closing
// this tab) or just stream in this tab like before. Only relevant to the
// File System Access path (see fsAccess.supportsDirectoryPicker) — App.tsx
// only ever renders this when that's true.
export function DownloadOptionsDialog({ download, onClose, onConfirm }: Props) {
  const [folder, setFolder] = useState<FileSystemDirectoryHandle | null>(null)
  const [loadingFolder, setLoadingFolder] = useState(true)
  const [useDownloadManager, setUseDownloadManager] = useState(true)
  const [status, setStatus] = useState<{ kind: 'idle' | 'error'; message?: string }>({ kind: 'idle' })

  useEffect(() => {
    // queryPermission alone needs no user gesture, so this is safe on mount.
    getDefaultDirectory().then((handle) => {
      setFolder(handle)
      setLoadingFolder(false)
    })
  }, [])

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  async function handleChangeFolder() {
    setStatus({ kind: 'idle' })
    try {
      const handle = await pickAndRememberDirectory()
      if (handle) setFolder(handle)
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof Error ? err.message : String(err) })
    }
  }

  // Deliberately synchronous up to here — onConfirm (App.tsx's
  // startStreamedDownload) opens the Downloads popup window as its own very
  // first statement, before any await, so it still runs inside this click's
  // user-activation. Nothing async happens in this function itself.
  function handleConfirm() {
    if (!folder) return
    onConfirm({ folder, useDownloadManager })
    onClose()
  }

  return (
    <div className="detail-overlay" onClick={onClose}>
      <div className="detail-panel download-options-panel" onClick={(e) => e.stopPropagation()}>
        <div className="detail-header">
          <h2>Download "{download.name}"</h2>
          <button className="detail-close" onClick={onClose} title="Close">
            ✕
          </button>
        </div>

        <div className="api-key-row">
          <code className="api-key-value">
            {loadingFolder ? 'Checking…' : (folder?.name ?? "No folder chosen yet")}
          </code>
          <button type="button" onClick={handleChangeFolder}>
            {folder ? 'Change folder' : 'Choose folder'}
          </button>
        </div>
        <p className="settings-help">
          Files stream straight into this folder. Remembered for next time — change it here, or later
          in Settings → Downloads.
        </p>

        <label className="download-manager-check">
          <input type="checkbox" checked={useDownloadManager} onChange={(e) => setUseDownloadManager(e.target.checked)} />
          Send to the Downloads window — keeps going even if you close this tab
        </label>

        {status.kind === 'error' && <p className="settings-error">Failed to change folder: {status.message}</p>}

        <div className="download-options-actions">
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="button" className="download-options-confirm" onClick={handleConfirm} disabled={!folder}>
            Download
          </button>
        </div>
      </div>
    </div>
  )
}
