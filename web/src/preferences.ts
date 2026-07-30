// Purely client-side (localStorage, per-browser) UI preferences — unlike
// internal/config's settings, these never touch the server, since they're
// about how *this* browser behaves rather than anything AcerviNode itself
// does. See DownloadOptionsDialog, the single entry point for every
// "download everything" action (table row and detail view alike), which
// both reads and writes this.

const DOWNLOAD_MODE_KEY = 'acervinode_download_mode'

// folder: stream every file straight into a folder the user picks
// (Chromium only — see fsAccess.supportsDirectoryPicker; DownloadOptionsDialog
// never offers this as a choice on a browser without it, rather than
// offering it and having it fail).
// zip: resolve the whole download as one provider-zipped archive instead —
// a single file, so no folder picker involved, and reliable in every
// browser (the provider's own response forces a real save here).
// individual: fetch and save every file as its own real download, no
// folder chosen up front — the universal option, forced via
// fsAccess.forceDownload rather than a plain link click (see its own doc
// comment for why that distinction matters).
export type DownloadMode = 'folder' | 'zip' | 'individual'

// Not validated against the current browser's actual capabilities here —
// a stored 'folder' preference from a previous, more-capable browser is
// harmless to read back; DownloadOptionsDialog is the one place that
// decides whether 'folder' is actually usable right now and substitutes a
// fallback if not.
export function getDownloadMode(): DownloadMode {
  const stored = localStorage.getItem(DOWNLOAD_MODE_KEY)
  if (stored === 'folder' || stored === 'zip' || stored === 'individual') return stored
  return 'folder'
}

export function setDownloadMode(mode: DownloadMode): void {
  localStorage.setItem(DOWNLOAD_MODE_KEY, mode)
}
