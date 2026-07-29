// Thin wrapper around the File System Access API (Chromium-only as of this
// writing — not Firefox/Safari), used by the downloads table's "Download
// all" button to write files straight into a folder the user picks, instead
// of opening one browser tab per file. Callers should feature-detect via
// `supportsDirectoryPicker()` and fall back to opening links individually
// when it's false.

import type { AddBatchMessage, FromPopupMessage } from './downloadWindowProtocol'

export function supportsDirectoryPicker(): boolean {
  return typeof window !== 'undefined' && 'showDirectoryPicker' in window
}

// pickDirectory must be called directly inside a click handler, before any
// other await — Chromium requires "transient user activation" for the
// picker, which an earlier await (e.g. an API call) can consume before this
// ever runs. Returns null if the user cancels the picker, not an error.
export async function pickDirectory(): Promise<FileSystemDirectoryHandle | null> {
  try {
    // TypeScript's bundled DOM lib doesn't declare showDirectoryPicker yet.
    return await (window as unknown as { showDirectoryPicker(opts?: { mode?: 'read' | 'readwrite' }): Promise<FileSystemDirectoryHandle> }).showDirectoryPicker({
      mode: 'readwrite',
    })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') return null
    throw err
  }
}

// writeFileToDirectory streams a fetch Response's body straight to disk
// (never buffering the whole file in memory — matters for multi-gigabyte
// video files), creating any subdirectories a "/"-containing path implies.
// onChunk, if given, is called with each chunk's byte length as it's
// written — the caller sums these across a whole batch of files to drive a
// progress bar; omitting it uses pipeTo directly (marginally cheaper, no
// per-chunk callback overhead) for callers that don't need progress.
export async function writeFileToDirectory(
  root: FileSystemDirectoryHandle,
  path: string,
  response: Response,
  onChunk?: (bytesWritten: number) => void,
): Promise<void> {
  if (!response.body) {
    throw new Error(`${path}: response has no body to stream`)
  }
  const parts = path.split('/').filter(Boolean)
  if (parts.length === 0) {
    throw new Error(`invalid file path: ${path}`)
  }

  let dir = root
  for (const segment of parts.slice(0, -1)) {
    dir = await dir.getDirectoryHandle(segment, { create: true })
  }
  const fileHandle = await dir.getFileHandle(parts[parts.length - 1], { create: true })
  const writable = await fileHandle.createWritable()

  if (!onChunk) {
    await response.body.pipeTo(writable)
    return
  }

  const reader = response.body.getReader()
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      await writable.write(value)
      onChunk(value.byteLength)
    }
    await writable.close()
  } catch (err) {
    await writable.abort().catch(() => {})
    throw err
  }
}

// --- remembered default folder ---------------------------------------------
//
// Picking a folder every single time you download a batch of files is
// friction the File System Access API doesn't strictly require: a directory
// handle can be persisted (IndexedDB — localStorage can't hold a handle
// object) and reused later without re-prompting, as long as its permission
// is still granted. queryPermission itself never needs a user gesture, so
// checking "do we already have a usable default?" is safe to do before the
// click handler's first await; only actually showing the picker (when there
// is no usable default yet) still needs to happen first in the gesture.

const DB_NAME = 'acervinode-fs-access'
const STORE_NAME = 'handles'
const DEFAULT_DIR_KEY = 'default-directory'

function openHandleStore(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1)
    req.onupgradeneeded = () => {
      req.result.createObjectStore(STORE_NAME)
    }
    req.onsuccess = () => resolve(req.result)
    req.onerror = () => reject(req.error)
  })
}

async function storeHandle(key: string, handle: FileSystemDirectoryHandle): Promise<void> {
  const db = await openHandleStore()
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readwrite')
      tx.objectStore(STORE_NAME).put(handle, key)
      tx.oncomplete = () => resolve()
      tx.onerror = () => reject(tx.error)
    })
  } finally {
    db.close()
  }
}

async function loadHandle(key: string): Promise<FileSystemDirectoryHandle | null> {
  const db = await openHandleStore()
  try {
    return await new Promise<FileSystemDirectoryHandle | null>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readonly')
      const req = tx.objectStore(STORE_NAME).get(key)
      req.onsuccess = () => resolve(req.result ?? null)
      req.onerror = () => reject(req.error)
    })
  } finally {
    db.close()
  }
}

type PermissionMode = { mode: 'read' | 'readwrite' }
interface PermissibleHandle {
  queryPermission(desc: PermissionMode): Promise<PermissionState>
  requestPermission(desc: PermissionMode): Promise<PermissionState>
}

// hasReadWritePermission checks (and, if requestIfNeeded, asks for) write
// access to a previously-picked handle. Asking requires a user gesture, same
// as showDirectoryPicker; checking alone doesn't.
async function hasReadWritePermission(handle: FileSystemDirectoryHandle, requestIfNeeded: boolean): Promise<boolean> {
  const permissible = handle as unknown as PermissibleHandle
  if ((await permissible.queryPermission({ mode: 'readwrite' })) === 'granted') return true
  if (!requestIfNeeded) return false
  return (await permissible.requestPermission({ mode: 'readwrite' })) === 'granted'
}

// getDefaultDirectory returns the remembered folder if one is stored and
// still has live permission, without prompting — safe to call any time, not
// just inside a click handler.
export async function getDefaultDirectory(): Promise<FileSystemDirectoryHandle | null> {
  if (!supportsDirectoryPicker()) return null
  const handle = await loadHandle(DEFAULT_DIR_KEY)
  if (!handle || !(await hasReadWritePermission(handle, false))) return null
  return handle
}

// pickAndRememberDirectory shows the picker (must be the first await in
// whatever click handler calls it — see pickDirectory) and saves the result
// as the new default for next time, unless the user cancels.
export async function pickAndRememberDirectory(): Promise<FileSystemDirectoryHandle | null> {
  const handle = await pickDirectory()
  if (handle) await storeHandle(DEFAULT_DIR_KEY, handle)
  return handle
}

// forgetDefaultDirectory clears the remembered folder — the next download
// (or an explicit "change folder") shows the picker again.
export async function forgetDefaultDirectory(): Promise<void> {
  await storeHandle(DEFAULT_DIR_KEY, null as unknown as FileSystemDirectoryHandle)
}

// resolveDownloadDirectory is the one entry point download handlers should
// use: reuses the remembered default if it's still usable (no prompt at
// all), otherwise falls back to the picker. Only genuinely risky for the
// user-gesture requirement on that fallback path — the remembered-default
// check ahead of it is a couple of fast IndexedDB round trips, comfortably
// inside Chromium's several-second activation window in practice, but this
// hasn't been exercised by a real click in a real browser — see the
// caveat this was shipped with.
export async function resolveDownloadDirectory(): Promise<FileSystemDirectoryHandle | null> {
  const remembered = await getDefaultDirectory()
  if (remembered) return remembered
  return pickAndRememberDirectory()
}

// --- the shared "Downloads" popup window -----------------------------------
//
// A multi-file streamed download otherwise dies the moment the tab that
// started it closes or navigates away — there's no browser-level background
// process carrying it, unlike a real browser download. openDownloadWindow
// hands the whole batch off to a small, separate popup window instead: once
// it's running there, the main tab is free to close without losing anything,
// since the popup is its own independent browsing context. One shared window
// gathers every active batch (not one popup per download) — window.open with
// a fixed name reuses an already-open window rather than spawning another.

const DOWNLOAD_WINDOW_NAME = 'acervinode-downloads'

let downloadWindowRef: Window | null = null
let downloadWindowReady: Promise<void> | null = null

// openDownloadWindow opens (or focuses/reuses, if already open) the shared
// popup, and returns both the window reference and a promise that resolves
// once the popup's own message listener is confirmed registered — postMessage
// sent before that would just be lost. Must be called synchronously in a
// click handler, same gesture requirement as showDirectoryPicker — window.open
// itself is synchronous (no await before it), so calling it first and
// resolveDownloadDirectory()/the picker right after, with no other await in
// between, keeps both inside the same activation.
export function openDownloadWindow(): { popup: Window; ready: Promise<void> } | null {
  if (downloadWindowRef && !downloadWindowRef.closed) {
    downloadWindowRef.focus()
    return { popup: downloadWindowRef, ready: downloadWindowReady ?? Promise.resolve() }
  }

  const popup = window.open(
    `${window.location.pathname}?popup=downloads`,
    DOWNLOAD_WINDOW_NAME,
    'width=380,height=520,menubar=no,toolbar=no,location=no,status=no',
  )
  if (!popup) return null // blocked by a popup blocker

  downloadWindowRef = popup
  downloadWindowReady = new Promise<void>((resolve) => {
    function onMessage(e: MessageEvent<FromPopupMessage>) {
      if (e.source !== popup || e.origin !== window.location.origin) return
      if (e.data?.type === 'popup-ready') {
        window.removeEventListener('message', onMessage)
        resolve()
      }
    }
    window.addEventListener('message', onMessage)
  })
  return { popup, ready: downloadWindowReady }
}

// sendBatchToDownloadWindow waits for the popup to confirm it's ready, then
// posts one download's files for it to fetch and stream to disk on its own.
export async function sendBatchToDownloadWindow(
  opened: { popup: Window; ready: Promise<void> },
  batch: Omit<AddBatchMessage, 'type'>,
): Promise<void> {
  await opened.ready
  const message: AddBatchMessage = { type: 'add-batch', ...batch }
  opened.popup.postMessage(message, window.location.origin)
}

// listenForDownloadWindowMessages relays the popup's progress/completion
// reports back into the main app's own UI (e.g. the downloads table's
// per-row progress bar) — best-effort, since the popup works fine without
// anyone listening. Returns an unsubscribe function.
export function listenForDownloadWindowMessages(onMessage: (msg: FromPopupMessage) => void): () => void {
  function handler(e: MessageEvent<FromPopupMessage>) {
    if (e.origin !== window.location.origin) return
    if (e.data?.type === 'batch-progress' || e.data?.type === 'batch-complete') {
      onMessage(e.data)
    }
  }
  window.addEventListener('message', handler)
  return () => window.removeEventListener('message', handler)
}
