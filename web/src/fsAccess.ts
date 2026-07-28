// Thin wrapper around the File System Access API (Chromium-only as of this
// writing — not Firefox/Safari), used by the downloads table's "Download
// all" button to write files straight into a folder the user picks, instead
// of opening one browser tab per file. Callers should feature-detect via
// `supportsDirectoryPicker()` and fall back to opening links individually
// when it's false.

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
export async function writeFileToDirectory(root: FileSystemDirectoryHandle, path: string, response: Response): Promise<void> {
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
  await response.body.pipeTo(writable)
}
