import { useEffect, useState, type FormEvent } from 'react'
import {
  addTorrent,
  addUsenet,
  addWebDownload,
  ApiError,
  checkCachedTorrent,
  checkCachedUsenet,
  checkCachedWebDownload,
  getTorrentInfo,
  type ProviderStatus,
  type TorrentInfoResponse,
} from '../api'
import { formatBytes } from '../format'

type Protocol = 'torrent' | 'usenet' | 'webdl'
type InputMode = 'link' | 'file'

interface Props {
  apiKey: string
  providers: ProviderStatus[]
  // isAdmin gates the Managed/Manual toggle entirely — a member has no
  // access to the Managed pipeline at all (see docs/providers.md#roles),
  // so they never see the choice; everything they add stays Manual, same
  // as before this existed. defaultManaged seeds the toggle's starting
  // position from whichever tab the button was opened from (see App.tsx) —
  // still just a default, not a restriction; an admin can flip it either
  // way regardless of which tab they started from.
  isAdmin: boolean
  defaultManaged: boolean
  onClose: () => void
  // onAdded reports which mode was actually used — not necessarily
  // defaultManaged, since an admin can flip the toggle before submitting —
  // so the caller can navigate to the tab the new download actually landed
  // in, not just the one the button happened to default to.
  onAdded: (addedManaged: boolean) => void
}

const PROTOCOL_LABELS: Record<Protocol, string> = {
  torrent: 'Torrent',
  usenet: 'Usenet',
  webdl: 'Web Link',
}

export function AddDownload({ apiKey, providers, isAdmin, defaultManaged, onClose, onAdded }: Props) {
  const torrentAvailable = providers.some((p) => p.torrent_capable)
  const usenetAvailable = providers.some((p) => p.usenet_capable)
  const webdlAvailable = providers.some((p) => p.webdl_capable)
  const availableProtocols: Protocol[] = [
    ...(torrentAvailable ? (['torrent'] as const) : []),
    ...(usenetAvailable ? (['usenet'] as const) : []),
    ...(webdlAvailable ? (['webdl'] as const) : []),
  ]

  const [protocol, setProtocol] = useState<Protocol>(availableProtocols[0] ?? 'torrent')
  // Web Downloads is genuinely link-only — TorBox's own createwebdownload API
  // has no file-upload variant, unlike torrent/usenet — so there's no mode
  // toggle to show for it (see handleSubmit's protocol==='webdl' branch).
  const [mode, setMode] = useState<InputMode>('link')
  const [link, setLink] = useState('')
  const [file, setFile] = useState<File | null>(null)
  // managed is always false for a member — isAdmin gates whether the toggle
  // even renders, not just whether it's editable, so there's no path for a
  // non-admin to end up with this true.
  const [managed, setManaged] = useState(isAdmin && defaultManaged)
  const [category, setCategory] = useState('')
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  // Cached/metadata preview — cached is null until a check-cached response
  // comes back (or the input isn't recognizable yet); torrentInfo is only
  // ever populated for the torrent protocol, which is the only one TorBox
  // has a by-hash metadata-preview endpoint for at all (see
  // docs/providers.md#cached--metadata-previews-before-adding). Debounced
  // rather than firing on every keystroke — the torrent-info lookup in
  // particular searches the live BitTorrent network, not just TorBox's own
  // account, so it's real work worth not spamming.
  const [cached, setCached] = useState<boolean | null>(null)
  const [torrentInfo, setTorrentInfo] = useState<TorrentInfoResponse | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  useEffect(() => {
    setCached(null)
    setTorrentInfo(null)
    if (mode !== 'link') return
    const value = link.trim()
    // Cheap client-side sanity checks before ever spending a round trip —
    // real validation still happens server-side either way, this just
    // avoids firing on a clearly-incomplete paste-in-progress.
    if (protocol === 'torrent' && !/xt=urn:btih:/i.test(value)) return
    if (protocol !== 'torrent' && !/^https?:\/\//i.test(value)) return

    let cancelled = false
    setPreviewLoading(true)

    async function loadPreview() {
      // Independent, best-effort — neither ever blocks submission, so one
      // failing (e.g. a transient network error) shouldn't hide the other
      // succeeding.
      const cachedPromise =
        protocol === 'torrent'
          ? checkCachedTorrent(apiKey, value)
          : protocol === 'usenet'
            ? checkCachedUsenet(apiKey, value)
            : checkCachedWebDownload(apiKey, value)
      const [cachedResult, infoResult] = await Promise.allSettled([
        cachedPromise,
        protocol === 'torrent' ? getTorrentInfo(apiKey, value) : Promise.resolve(null),
      ])
      if (cancelled) return
      setCached(cachedResult.status === 'fulfilled' ? cachedResult.value.cached : null)
      setTorrentInfo(infoResult.status === 'fulfilled' ? infoResult.value : null)
      setPreviewLoading(false)
    }

    const timer = setTimeout(loadPreview, 500)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiKey, protocol, mode, link])

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (protocol !== 'webdl' && mode === 'link' && !link.trim()) return
    if (protocol !== 'webdl' && mode === 'file' && !file) return
    if (protocol === 'webdl' && !link.trim()) return

    setStatus({ kind: 'saving' })
    try {
      // Category only means anything for a Managed add — it drives
      // internal/importer's save-path resolution, the same as a category
      // Sonarr/Radarr declared through the compat shims (see
      // docs/configuration.md#categories-and-save-paths). A Manual add never
      // sends it: Manual downloads mirror TorBox's own web UI, which has no
      // category concept at all (see ROADMAP.md's "Manual categories" entry).
      const categoryToSend = managed ? category.trim() : undefined
      const addedVia = managed ? 'arr' : undefined
      if (protocol === 'torrent') {
        if (mode === 'file') await addTorrent(apiKey, { file: file as File, category: categoryToSend, addedVia })
        else await addTorrent(apiKey, { magnet: link.trim(), category: categoryToSend, addedVia })
      } else if (protocol === 'usenet') {
        if (mode === 'file') await addUsenet(apiKey, { file: file as File, category: categoryToSend, addedVia })
        else await addUsenet(apiKey, { url: link.trim(), category: categoryToSend, addedVia })
      } else {
        await addWebDownload(apiKey, { link: link.trim(), category: categoryToSend, addedVia })
      }
      onAdded(managed)
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  function selectProtocol(p: Protocol) {
    setProtocol(p)
    setMode('link')
    setStatus({ kind: 'idle' })
  }

  const protocolAvailable =
    protocol === 'torrent' ? torrentAvailable : protocol === 'usenet' ? usenetAvailable : webdlAvailable

  return (
    <div className="detail-overlay" onClick={onClose}>
      <div className="detail-panel add-download-panel" onClick={(e) => e.stopPropagation()}>
        <div className="detail-header">
          <h2>Add to {managed ? 'Managed' : 'Manual'}</h2>
          <button className="detail-close" onClick={onClose} title="Close">
            ✕
          </button>
        </div>

        {isAdmin && (
          <div className="mode-toggle">
            <label>
              <input type="radio" checked={!managed} onChange={() => setManaged(false)} />
              Manual
            </label>
            <label>
              <input type="radio" checked={managed} onChange={() => setManaged(true)} />
              Managed
            </label>
          </div>
        )}
        {isAdmin && (
          <p className="settings-help">
            {managed
              ? 'Fetched to disk automatically, same as an *arr-added download — shows up in the Managed tab.'
              : "Grabbed on demand, mirroring TorBox's own web UI — shows up in the Manual tab, never auto-fetched."}
          </p>
        )}

        {availableProtocols.length > 1 && (
          <div className="protocol-tabs">
            {availableProtocols.map((p) => (
              <button
                key={p}
                type="button"
                className={protocol === p ? 'tab tab-active' : 'tab'}
                onClick={() => selectProtocol(p)}
              >
                {PROTOCOL_LABELS[p]}
              </button>
            ))}
          </div>
        )}

        {availableProtocols.length === 0 && (
          <p className="settings-help">No provider is configured yet — add one under Settings first.</p>
        )}

        {protocolAvailable && (
          <form onSubmit={handleSubmit}>
            {protocol !== 'webdl' && (
              <div className="mode-toggle">
                <label>
                  <input type="radio" checked={mode === 'link'} onChange={() => setMode('link')} />
                  {protocol === 'torrent' ? 'Magnet link' : 'NZB URL'}
                </label>
                <label>
                  <input type="radio" checked={mode === 'file'} onChange={() => setMode('file')} />
                  Upload file
                </label>
              </div>
            )}

            {protocol === 'webdl' || mode === 'link' ? (
              <input
                type="text"
                placeholder={
                  protocol === 'torrent'
                    ? 'magnet:?xt=urn:btih:...'
                    : protocol === 'usenet'
                      ? 'https://example.com/release.nzb'
                      : 'https://mega.nz/folder/...'
                }
                value={link}
                onChange={(e) => setLink(e.target.value)}
                autoFocus
              />
            ) : (
              <input
                type="file"
                accept={protocol === 'torrent' ? '.torrent' : '.nzb'}
                onChange={(e) => setFile(e.target.files?.[0] ?? null)}
              />
            )}

            {mode === 'link' && (previewLoading || cached !== null || torrentInfo) && (
              <div className="add-preview">
                {previewLoading && <p className="settings-help">Checking…</p>}
                {!previewLoading && cached !== null && (
                  <p className={cached ? 'settings-success' : 'settings-help'}>
                    {cached ? '✓ Cached — instantly available' : 'Not cached — will need a real download'}
                  </p>
                )}
                {!previewLoading && torrentInfo && (
                  <>
                    {torrentInfo.available ? (
                      <p className="settings-help" title={torrentInfo.name}>
                        {torrentInfo.name}
                        {typeof torrentInfo.size_bytes === 'number' && ` · ${formatBytes(torrentInfo.size_bytes)}`}
                        {torrentInfo.files && ` · ${torrentInfo.files.length} file${torrentInfo.files.length === 1 ? '' : 's'}`}
                        {typeof torrentInfo.seeds === 'number' &&
                          ` · ${torrentInfo.seeds} seed${torrentInfo.seeds === 1 ? '' : 's'}, ${torrentInfo.peers} peer${torrentInfo.peers === 1 ? '' : 's'}`}
                      </p>
                    ) : (
                      // Routine, not an error — TorBox couldn't find this
                      // torrent on the network yet (or the provider doesn't
                      // support previews at all). The check-cached badge
                      // above still shows either way.
                      <p className="settings-help">No preview available yet.</p>
                    )}
                  </>
                )}
              </div>
            )}

            {managed && (
              <input
                type="text"
                placeholder="Category (optional, e.g. tv-sonarr)"
                value={category}
                onChange={(e) => setCategory(e.target.value)}
              />
            )}

            <button
              type="submit"
              disabled={
                status.kind === 'saving' ||
                (protocol === 'webdl' ? !link.trim() : mode === 'link' ? !link.trim() : !file)
              }
            >
              {status.kind === 'saving' ? 'Adding…' : 'Add'}
            </button>
            {status.kind === 'error' && <p className="settings-error">Failed to add: {status.message}</p>}
          </form>
        )}
      </div>
    </div>
  )
}
