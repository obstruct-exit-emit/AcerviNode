import { useEffect, useState, type FormEvent } from 'react'
import {
  addTorrent,
  addUsenet,
  addWebDownload,
  ApiError,
  checkCachedTorrent,
  checkCachedUsenet,
  checkCachedWebDownload,
  getGeneralSettings,
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
  // Only offered when there is genuinely a choice — with one provider the
  // picker would be a control with a single option.
  const [provider, setProvider] = useState('')
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
  // Per-add overrides of the configured Managed defaults. undefined until the
  // defaults arrive, so an add before then sends nothing and the server
  // applies its own defaults rather than a guessed false.
  const [deleteAfterFetch, setDeleteAfterFetch] = useState<boolean | undefined>(undefined)
  const [keepFiles, setKeepFiles] = useState<boolean | undefined>(undefined)
  // defaultsLoaded distinguishes "not fetched yet" from "fetched, both
  // false". Without it a failed fetch is indistinguishable from real
  // defaults of false — and an earlier version of this gated rendering on
  // the values being set, so a failure hid the controls entirely with no
  // way to tell why.
  const [defaultsLoaded, setDefaultsLoaded] = useState(false)

  // Seed the two Managed options from the configured defaults. Admin-only,
  // because the settings endpoint is and only an admin can add a Managed
  // download at all. On failure the controls still render, unchecked and
  // usable — an untouched box sends nothing, so the server applies its own
  // default and a failed fetch costs accuracy of the initial tick rather
  // than the whole feature.
  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    getGeneralSettings(apiKey)
      .then((g) => {
        if (cancelled) return
        setDeleteAfterFetch(g.managed_add_delete_after_fetch)
        setKeepFiles(g.managed_add_keep_files)
        setDefaultsLoaded(true)
      })
      .catch(() => {
        if (!cancelled) setDefaultsLoaded(true)
      })
    return () => {
      cancelled = true
    }
  }, [apiKey, isAdmin])
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
      // Managed-only: the server ignores them for a Manual add, and sending
      // them anyway would imply they meant something there.
      const dafToSend = managed ? deleteAfterFetch : undefined
      const kfToSend = managed ? keepFiles : undefined
      const addedVia = managed ? 'arr' : undefined
      // Empty means "didn't choose", which the server reads as the default.
      const providerToSend = provider || undefined
      if (protocol === 'torrent') {
        if (mode === 'file') await addTorrent(apiKey, { file: file as File, category: categoryToSend, addedVia, provider: providerToSend, deleteAfterFetch: dafToSend, keepFiles: kfToSend })
        else await addTorrent(apiKey, { magnet: link.trim(), category: categoryToSend, addedVia, provider: providerToSend, deleteAfterFetch: dafToSend, keepFiles: kfToSend })
      } else if (protocol === 'usenet') {
        if (mode === 'file') await addUsenet(apiKey, { file: file as File, category: categoryToSend, addedVia, provider: providerToSend, deleteAfterFetch: dafToSend, keepFiles: kfToSend })
        else await addUsenet(apiKey, { url: link.trim(), category: categoryToSend, addedVia, provider: providerToSend, deleteAfterFetch: dafToSend, keepFiles: kfToSend })
      } else {
        await addWebDownload(apiKey, { link: link.trim(), category: categoryToSend, addedVia, provider: providerToSend, deleteAfterFetch: dafToSend, keepFiles: kfToSend })
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

  // Which providers can actually take the protocol currently selected — a
  // torrent-only provider shouldn't be offered for a usenet add.
  const capableProviders = providers.filter((p) =>
    protocol === 'torrent' ? p.torrent_capable : protocol === 'usenet' ? p.usenet_capable : p.webdl_capable,
  )

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

        {/* Only for a Managed add, and only once the defaults have loaded —
            these override them for this one download. Nothing here applies to
            a download an *arr adds for itself. Defaults live in
            Settings → Managed adds. */}
        {isAdmin && managed && (
          <div className="managed-options">
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={deleteAfterFetch ?? false}
                disabled={!defaultsLoaded}
                onChange={(e) => setDeleteAfterFetch(e.target.checked)}
              />
              Delete from provider once fetched
            </label>
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={keepFiles ?? false}
                disabled={!defaultsLoaded}
                onChange={(e) => setKeepFiles(e.target.checked)}
              />
              Keep local files (exempt from cleanup)
            </label>
          </div>
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

        {/* Only shown when more than one provider can handle the selected
            protocol — otherwise there is nothing to choose between, and a
            picker with one option is just noise. */}
        {capableProviders.length > 1 && (
          <label className="add-provider">
            Provider
            <select value={provider} onChange={(e) => setProvider(e.target.value)}>
              <option value="">Default</option>
              {capableProviders.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
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
