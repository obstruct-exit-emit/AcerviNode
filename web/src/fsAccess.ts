// Thin wrapper around the File System Access API (Chromium-only as of this
// writing — not Firefox/Safari), used by the downloads table's "Download
// all" button to write files straight into a folder the user picks, instead
// of opening one browser tab per file. Callers should feature-detect via
// `supportsDirectoryPicker()` and fall back to opening links individually
// when it's false.

import { DOWNLOAD_CHANNEL_NAME } from './downloadWindowProtocol'
import type { AddBatchMessage, FromPopupMessage } from './downloadWindowProtocol'

export function supportsDirectoryPicker(): boolean {
  return typeof window !== 'undefined' && 'showDirectoryPicker' in window
}

// forceDownload fetches url and triggers a real browser download via a
// blob: URL + a synthetic <a download> click, instead of window.open/a
// plain navigation. Necessary because a resolved per-file link (unlike the
// zip link) carries no Content-Disposition: attachment header from the
// provider — a plain link click just navigates there, which the browser
// renders inline instead of downloading whenever it's a type it knows how
// to display (video, audio, images…), in every browser, not just
// Firefox/Safari. blob: URLs are always same-origin, so the download
// attribute reliably applies regardless of the resolved link's own
// (cross-origin, provider-hosted) origin — a plain <a download> pointed
// directly at a cross-origin URL is unreliable (several browsers ignore
// the attribute cross-origin and just navigate instead). Tradeoff: pulls
// the whole file through memory before the save begins — no progressive
// disk streaming the way a real browser download (or the Chromium
// folder-streaming path elsewhere in this file) gets, so this is
// meaningfully heavier for a very large file. Never used for the zip
// link, which already downloads reliably as a plain link — the provider's
// own response sets Content-Disposition there.
export async function forceDownload(url: string, filename: string): Promise<void> {
  const resp = await fetchWithIdleTimeout(url, DEFAULT_IDLE_TIMEOUT_MS)
  if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`)
  const blob = await readBlobWithIdleTimeout(resp, DEFAULT_IDLE_TIMEOUT_MS)
  const blobUrl = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = blobUrl
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  // Give the download a moment to actually start before revoking — some
  // browsers race the handoff if it's revoked immediately.
  setTimeout(() => URL.revokeObjectURL(blobUrl), 30000)
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

// DEFAULT_IDLE_TIMEOUT_MS matches internal/importer's own
// import_fetch_timeout_seconds default (600s) — see writeFileToDirectory's
// own doc comment for why this is idle/stall-based, not a total-transfer
// budget. Not read live from Settings: that endpoint is admin-only, but a
// Manual download (the only thing this streams) is reachable by a member
// too, so there's no session-independent way to fetch the configured value
// from here — a hardcoded default matching the backend's own is the
// simplest honest answer.
export const DEFAULT_IDLE_TIMEOUT_MS = 600_000

// fetchWithIdleTimeout wraps fetch, aborting if idleTimeoutMs pass with no
// response received at all — the connect-and-wait-for-headers half of the
// same idle/stall protection writeFileToDirectory applies to the body
// itself once headers do arrive (see its own doc comment); together they
// cover a stall at any point in a streamed download, not just mid-transfer,
// the same two phases internal/importer's own idleTimeoutReader covers on
// the backend. externalSignal, if given, aborts the request too (e.g. a
// user's manual Stop click in DownloadWindow.tsx) — combined via
// AbortSignal.any, safe here since this whole subsystem is already
// Chromium-only (see this file's own top doc comment) and AbortSignal.any
// has been in Chromium since 2023. A caller checking its own external
// controller's aborted flag afterward (see DownloadWindow.tsx's "deliberate
// Stop, not a real failure" check) still works unchanged either way: only a
// real externalSignal abort sets that controller's own aborted flag, not an
// idle-timeout one, so a stall is correctly reported as a real failure
// rather than silently swallowed as if the user had clicked Stop.
export async function fetchWithIdleTimeout(url: string, idleTimeoutMs: number, externalSignal?: AbortSignal): Promise<Response> {
  const idleController = new AbortController()
  const timer = setTimeout(() => idleController.abort(), idleTimeoutMs)
  const signal = externalSignal ? AbortSignal.any([externalSignal, idleController.signal]) : idleController.signal
  try {
    return await fetch(url, { signal })
  } finally {
    clearTimeout(timer)
  }
}

// readWithIdleTimeout races a single reader.read() call against an idle
// deadline, rejecting if it fires first — used by writeFileToDirectory
// below. Doesn't reset anything itself; the caller calling this again on
// every successful chunk is what makes the deadline "idle" rather than
// "total" — see writeFileToDirectory's own doc comment.
function readWithIdleTimeout<T>(reader: ReadableStreamDefaultReader<T>, idleTimeoutMs: number): Promise<ReadableStreamReadResult<T>> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`No data received for ${Math.round(idleTimeoutMs / 1000)}s — connection appears stalled`))
    }, idleTimeoutMs)
    reader.read().then(
      (result) => {
        clearTimeout(timer)
        resolve(result)
      },
      (err) => {
        clearTimeout(timer)
        reject(err)
      },
    )
  })
}

// readBlobWithIdleTimeout is forceDownload's counterpart to
// writeFileToDirectory's own idle-timeout wiring below — Response.blob()
// has no per-chunk hook to protect, so this reads the body manually via the
// same readWithIdleTimeout helper and reassembles it into a Blob
// afterward, rather than buffering with zero stall protection the way a
// plain await response.blob() does.
async function readBlobWithIdleTimeout(response: Response, idleTimeoutMs: number): Promise<Blob> {
  if (!response.body) {
    return response.blob() // no body at all (e.g. a 204) — nothing to protect
  }
  const reader = response.body.getReader()
  const chunks: BlobPart[] = []
  try {
    while (true) {
      const { done, value } = await readWithIdleTimeout(reader, idleTimeoutMs)
      if (done) break
      chunks.push(value)
    }
  } catch (err) {
    await reader.cancel().catch(() => {})
    throw err
  }
  return new Blob(chunks)
}

// writeFileToDirectory streams a fetch Response's body straight to disk
// (never buffering the whole file in memory — matters for multi-gigabyte
// video files), creating any subdirectories a "/"-containing path implies.
// onChunk, if given, is called with each chunk's byte length as it's
// written — the caller sums these across a whole batch of files to drive a
// progress bar.
//
// idleTimeoutMs is an idle/stall deadline, not a total-transfer one —
// mirrors internal/importer's own idleTimeoutReader on the backend
// (import_fetch_timeout_seconds): the clock resets on every chunk actually
// received, so a large file that's slow but genuinely still transferring
// is never affected by this however long the whole download takes; only a
// connection that's actually gone quiet (the provider CDN link hangs, the
// network drops mid-transfer) trips it. Before this, a stalled streamed
// download — the in-tab path in App.tsx and the Downloads popup window in
// DownloadWindow.tsx, the only two callers — just sat there forever with
// no error and no way to notice short of watching the progress bar stop
// moving; the popup's own manual Stop button was the only recourse, and
// the in-tab path didn't even have that.
export async function writeFileToDirectory(
  root: FileSystemDirectoryHandle,
  path: string,
  response: Response,
  onChunk?: (bytesWritten: number) => void,
  idleTimeoutMs = DEFAULT_IDLE_TIMEOUT_MS,
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

  const reader = response.body.getReader()
  try {
    while (true) {
      const { done, value } = await readWithIdleTimeout(reader, idleTimeoutMs)
      if (done) break
      await writable.write(value)
      onChunk?.(value.byteLength)
    }
    await writable.close()
  } catch (err) {
    await reader.cancel().catch(() => {})
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

// queryWritePermission/requestWritePermission are exported for the Downloads
// popup window (see components/DownloadWindow.tsx): a directory handle's
// write grant is checked per top-level browsing context, not just per
// origin, so a handle that already has permission in the main window still
// reads back as 'prompt' once it arrives in the popup via postMessage — the
// popup has to query (and, on a real click there, request) its own grant
// before it can actually write anything, even though it's the same handle.
export async function queryWritePermission(handle: FileSystemDirectoryHandle): Promise<PermissionState> {
  return (handle as unknown as PermissibleHandle).queryPermission({ mode: 'readwrite' })
}

export async function requestWritePermission(handle: FileSystemDirectoryHandle): Promise<PermissionState> {
  return (handle as unknown as PermissibleHandle).requestPermission({ mode: 'readwrite' })
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
// gathers every active batch (not one popup per download), coordinated over
// a BroadcastChannel rather than the window.open()/window.opener references
// directly — see downloadWindowProtocol.ts for why: two independently-opened
// tabs of AcerviNode can't always see each other's named popup, and
// BroadcastChannel is what makes reuse correct regardless.

const DOWNLOAD_WINDOW_NAME = 'acervinode-downloads'
const POPUP_READY_TIMEOUT_MS = 8000

let downloadWindowRef: Window | null = null
let channel: BroadcastChannel | null = null

function downloadChannel(): BroadcastChannel {
  if (!channel) channel = new BroadcastChannel(DOWNLOAD_CHANNEL_NAME)
  return channel
}

// openDownloadWindow opens the shared popup if none is already known to
// this tab, or asks whatever popup is actually alive (which may be a
// different physical window than the one this tab previously opened, or
// none at all yet) to bring itself to the front. Must be called
// synchronously in a click handler, same gesture requirement as
// showDirectoryPicker — window.open itself is synchronous (no await before
// it), so calling it first, with no other await in between, keeps the
// activation alive for it.
export function openDownloadWindow(): { popup: Window } | null {
  if (downloadWindowRef && !downloadWindowRef.closed) {
    // Not downloadWindowRef.focus() directly — this tab's own reference
    // might itself be a duplicate that lost the singleton lock (see
    // downloadWindowProtocol.ts), in which case the broadcast is what
    // actually reaches the real active popup instead.
    downloadChannel().postMessage({ type: 'focus-request' })
    return { popup: downloadWindowRef }
  }

  const popup = window.open(
    `${window.location.pathname}?popup=downloads`,
    DOWNLOAD_WINDOW_NAME,
    'width=380,height=520,menubar=no,toolbar=no,location=no,status=no',
  )
  if (!popup) return null // blocked by a popup blocker

  downloadWindowRef = popup
  // Best-effort: if window.open() actually reused an existing popup this
  // tab didn't know about (or, rarely, created a genuine duplicate — see
  // downloadWindowProtocol.ts), this brings whichever one actually holds
  // the singleton lock to the front, not necessarily the object above.
  downloadChannel().postMessage({ type: 'focus-request' })
  return { popup }
}

// sendBatchToDownloadWindow pings for a live popup (its singleton-lock
// holder specifically — see DownloadWindow.tsx), waits for the reply, then
// broadcasts one download's files for it to fetch and stream to disk on its
// own. Doesn't need a Window reference at all — the popup that actually
// answers ping might not be the object openDownloadWindow() returned.
export async function sendBatchToDownloadWindow(batch: Omit<AddBatchMessage, 'type'>): Promise<void> {
  const ch = downloadChannel()
  await new Promise<void>((resolve, reject) => {
    const timeout = setTimeout(() => {
      ch.removeEventListener('message', onMessage)
      reject(new Error('Timed out waiting for the Downloads window to be ready.'))
    }, POPUP_READY_TIMEOUT_MS)
    function onMessage(e: MessageEvent<FromPopupMessage>) {
      if (e.data?.type !== 'popup-ready') return
      clearTimeout(timeout)
      ch.removeEventListener('message', onMessage)
      resolve()
    }
    ch.addEventListener('message', onMessage)
    ch.postMessage({ type: 'ping' })
  })
  const message: AddBatchMessage = { type: 'add-batch', ...batch }
  ch.postMessage(message)
}

// listenForDownloadWindowMessages relays the popup's progress/completion
// reports back into the main app's own UI (e.g. the downloads table's
// per-row progress bar) — best-effort, since the popup works fine without
// anyone listening. Every open AcerviNode tab receives every report
// regardless of which one actually triggered that download, which is
// harmless (a batch-progress/-complete for an unknown id is just ignored by
// callers) and arguably a feature — every tab reflects the shared popup's
// real state. Returns an unsubscribe function.
export function listenForDownloadWindowMessages(onMessage: (msg: FromPopupMessage) => void): () => void {
  const ch = downloadChannel()
  function handler(e: MessageEvent<FromPopupMessage>) {
    if (e.data?.type === 'batch-progress' || e.data?.type === 'batch-complete') {
      onMessage(e.data)
    }
  }
  ch.addEventListener('message', handler)
  return () => ch.removeEventListener('message', handler)
}
