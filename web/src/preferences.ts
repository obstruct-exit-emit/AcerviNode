// Purely client-side (localStorage, per-browser) UI preferences — unlike
// internal/config's settings, these never touch the server, since they're
// about how *this* browser behaves rather than anything AcerviNode itself
// does. See DownloadsTable's "Download all" button and Settings' Downloads
// section.

const DOWNLOAD_MODE_KEY = 'acervinode_download_mode'

// individual: resolve and fetch every file individually (streamed into a
// picked folder in Chromium, one tab per file elsewhere — see fsAccess.ts).
// zip: resolve the whole download as one provider-zipped archive instead
// (see api.getZipLink) — a single file, so no folder picker involved.
export type DownloadMode = 'individual' | 'zip'

export function getDownloadMode(): DownloadMode {
  return localStorage.getItem(DOWNLOAD_MODE_KEY) === 'zip' ? 'zip' : 'individual'
}

export function setDownloadMode(mode: DownloadMode): void {
  localStorage.setItem(DOWNLOAD_MODE_KEY, mode)
}
