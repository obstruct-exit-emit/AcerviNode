// Message shapes exchanged between the main app window(s) and the small
// "Downloads" popup window (see fsAccess.ts's openDownloadWindow and
// components/DownloadWindow.tsx) — the mechanism that lets a multi-file
// streamed download keep running after the main tab is closed or navigated
// away from. Both sides import this file, so the shapes can't drift apart.
//
// Carried over a BroadcastChannel (DOWNLOAD_CHANNEL_NAME below), not direct
// window.postMessage via window.opener/the window.open() return value —
// deliberately: two independently-opened tabs of the same origin (not one
// opened from the other) can end up in different browsing-context groups,
// where window.open(url, name) can't see the other tab's already-open named
// popup and spawns a second, genuinely separate one. A real, observed bug —
// downloads split silently across two popups depending on which tab
// happened to trigger which one. BroadcastChannel reaches every same-origin
// context regardless of that topology; the popup's own singleton Web Lock
// (see DOWNLOAD_LOCK_NAME in DownloadWindow.tsx) then guarantees only one
// popup instance ever actually processes a batch even when a second,
// physical popup window does briefly exist.
//
// The underlying transport still uses the structured clone algorithm (same
// as postMessage), so FileSystemDirectoryHandle travels the same way it
// always did — as an ordinary property, not anything special-cased.
export const DOWNLOAD_CHANNEL_NAME = 'acervinode-downloads'

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

// Sent main window -> popup: "is a Downloads popup alive?" — answered with
// PopupReadyMessage. A main window sends this before add-batch so a
// freshly-opened popup (still loading its JS bundle) doesn't miss it.
export interface PingMessage {
  type: 'ping'
}

// Sent main window -> popup: "bring yourself to the front" — used instead
// of calling .focus() on whatever window.open() returned, since that
// object isn't necessarily the popup actually holding the singleton lock
// (see the topology note above).
export interface FocusRequestMessage {
  type: 'focus-request'
}

// Sent popup -> any listening main window, best-effort (there may be none —
// that's fine, the popup doesn't depend on it for anything).
export interface BatchProgressMessage {
  type: 'batch-progress'
  downloadId: string
  loaded: number
  total: number
}

export interface FailedFile {
  path: string
  error: string
}

export interface BatchCompleteMessage {
  type: 'batch-complete'
  downloadId: string
  failed: FailedFile[]
}

// Sent popup -> every listening main window, once right after the popup
// (the one holding the singleton lock) registers its own listener, and
// again every time it receives a PingMessage.
export interface PopupReadyMessage {
  type: 'popup-ready'
}

export type ToPopupMessage = AddBatchMessage | PingMessage | FocusRequestMessage
export type FromPopupMessage = BatchProgressMessage | BatchCompleteMessage | PopupReadyMessage
