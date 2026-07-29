// Message shapes exchanged between the main app window and the small
// "Downloads" popup window (see fsAccess.ts's openDownloadWindow and
// components/DownloadWindow.tsx) — the mechanism that lets a multi-file
// streamed download keep running after the main tab is closed or navigated
// away from. Both sides import this file, so the shapes can't drift apart.
//
// window.postMessage carries these as plain structured-clone data;
// FileSystemDirectoryHandle is itself structured-clone-safe in Chromium
// (the only browser this whole download-window feature applies to — see
// fsAccess.supportsDirectoryPicker), so it travels as an ordinary property,
// not anything special-cased.

export interface DownloadWindowFile {
  path: string
  providerFileId: string
  sizeBytes: number
}

// Sent main window -> popup: hand off one download's files for the popup to
// fetch and stream to disk on its own, independent of whatever happens to
// the main tab afterward.
export interface AddBatchMessage {
  type: 'add-batch'
  downloadId: string
  downloadName: string
  files: DownloadWindowFile[]
  directoryHandle: FileSystemDirectoryHandle
}

// Sent popup -> main window, best-effort (the main window may not be open
// or listening — that's fine, the popup doesn't depend on it for anything).
export interface BatchProgressMessage {
  type: 'batch-progress'
  downloadId: string
  loaded: number
  total: number
}

export interface BatchCompleteMessage {
  type: 'batch-complete'
  downloadId: string
  failed: string[]
}

// Sent popup -> main window once, right after the popup's own listener is
// registered — what the main window waits for before posting the first
// add-batch, so a freshly-opened popup can't miss it (see
// fsAccess.openDownloadWindow's ready handshake).
export interface PopupReadyMessage {
  type: 'popup-ready'
}

export type ToPopupMessage = AddBatchMessage
export type FromPopupMessage = BatchProgressMessage | BatchCompleteMessage | PopupReadyMessage
