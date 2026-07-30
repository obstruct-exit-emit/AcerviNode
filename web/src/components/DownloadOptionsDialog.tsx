import { useEffect, useState } from 'react'
import type { Download } from '../api'
import { getDefaultDirectory, pickAndRememberDirectory, supportsDirectoryPicker } from '../fsAccess'
import { getDownloadMode, setDownloadMode, type DownloadMode } from '../preferences'

export interface DownloadOptions {
  mode: DownloadMode
  // Only ever set (and only ever read by a caller) when mode === 'folder'.
  folder?: FileSystemDirectoryHandle
  useDownloadManager?: boolean
}

interface Props {
  download: Download
  onClose: () => void
  onConfirm: (opts: DownloadOptions) => void
}

// The single entry point for every "download everything" action — the
// downloads table's per-row button and the detail view's "Download all"
// button both open this now, rather than each encoding its own default/
// fallback logic and the app ending up with three inconsistent-looking
// download paths scattered across two components (see ROADMAP.md's
// "Streamline the download UX" for the history). Shows every mode this
// browser can actually do — folder-streaming is simply absent from the
// choices, not present-but-disabled, on a browser without File System
// Access (see fsAccess.supportsDirectoryPicker) — remembers the last mode
// chosen as next time's default (preferences.ts), and for folder mode
// specifically lets the user see/change the destination folder and choose
// whether to hand off to the shared Downloads popup window before
// anything actually starts.
export function DownloadOptionsDialog({ download, onClose, onConfirm }: Props) {
  const canUseFolder = supportsDirectoryPicker()
  const [mode, setMode] = useState<DownloadMode>(() => {
    const stored = getDownloadMode()
    return stored === 'folder' && !canUseFolder ? 'individual' : stored
  })
  const [folder, setFolder] = useState<FileSystemDirectoryHandle | null>(null)
  const [loadingFolder, setLoadingFolder] = useState(canUseFolder)
  const [useDownloadManager, setUseDownloadManager] = useState(true)
  const [status, setStatus] = useState<{ kind: 'idle' | 'error'; message?: string }>({ kind: 'idle' })

  useEffect(() => {
    if (!canUseFolder) return
    // queryPermission alone needs no user gesture, so this is safe on mount.
    getDefaultDirectory().then((handle) => {
      setFolder(handle)
      setLoadingFolder(false)
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
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

  function selectMode(next: DownloadMode) {
    setMode(next)
    setDownloadMode(next)
  }

  // Deliberately synchronous up to here for folder mode — onConfirm
  // (App.tsx's startDownloadAll/startStreamedDownload) opens the Downloads
  // popup window as its own very first statement, before any await, so it
  // still runs inside this click's user activation — the same requirement
  // showDirectoryPicker itself has. Nothing async happens in this function.
  function handleConfirm() {
    if (mode === 'folder') {
      if (!folder) return
      onConfirm({ mode, folder, useDownloadManager })
    } else {
      onConfirm({ mode })
    }
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

        <div className="download-mode-options">
          {canUseFolder && (
            <label className="download-mode-option">
              <input type="radio" name="download-mode" checked={mode === 'folder'} onChange={() => selectMode('folder')} />
              <span>
                <strong>All files → a folder you pick</strong>
                <br />
                Streamed straight to disk, no browser download prompts.
              </span>
            </label>
          )}
          <label className="download-mode-option">
            <input type="radio" name="download-mode" checked={mode === 'zip'} onChange={() => selectMode('zip')} />
            <span>
              <strong>One zip archive</strong>
              <br />
              Everything bundled provider-side into a single file.
            </span>
          </label>
          <label className="download-mode-option">
            <input type="radio" name="download-mode" checked={mode === 'individual'} onChange={() => selectMode('individual')} />
            <span>
              <strong>Individual files</strong>
              <br />
              Each file downloads on its own, the normal browser way{!canUseFolder ? ' — no folder picker on this browser' : ''}.
            </span>
          </label>
        </div>

        {mode === 'folder' && (
          <>
            <div className="api-key-row">
              <code className="api-key-value">{loadingFolder ? 'Checking…' : (folder?.name ?? "No folder chosen yet")}</code>
              <button type="button" onClick={handleChangeFolder}>
                {folder ? 'Change folder' : 'Choose folder'}
              </button>
            </div>
            <p className="settings-help">
              Remembered for next time — change it here, or later in Settings → Downloads. Note: the
              browser won't let you pick Desktop/Documents/Downloads itself (a deliberate Chrome
              restriction) — choose a subfolder inside it instead.
            </p>
            <label className="download-manager-check">
              <input type="checkbox" checked={useDownloadManager} onChange={(e) => setUseDownloadManager(e.target.checked)} />
              Send to the Downloads window — keeps going even if you close this tab
            </label>
          </>
        )}

        {status.kind === 'error' && <p className="settings-error">Failed to change folder: {status.message}</p>}

        <div className="download-options-actions">
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="button" className="download-options-confirm" onClick={handleConfirm} disabled={mode === 'folder' && !folder}>
            Download
          </button>
        </div>
      </div>
    </div>
  )
}
