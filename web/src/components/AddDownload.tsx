import { useEffect, useState, type FormEvent } from 'react'
import { addTorrent, addUsenet, addWebDownload, ApiError, type ProviderStatus } from '../api'

type Protocol = 'torrent' | 'usenet' | 'webdl'
type InputMode = 'link' | 'file'

interface Props {
  apiKey: string
  providers: ProviderStatus[]
  onClose: () => void
  onAdded: () => void
}

const PROTOCOL_LABELS: Record<Protocol, string> = {
  torrent: 'Torrent',
  usenet: 'Usenet',
  webdl: 'Web Link',
}

export function AddDownload({ apiKey, providers, onClose, onAdded }: Props) {
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
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (protocol !== 'webdl' && mode === 'link' && !link.trim()) return
    if (protocol !== 'webdl' && mode === 'file' && !file) return
    if (protocol === 'webdl' && !link.trim()) return

    setStatus({ kind: 'saving' })
    try {
      // No category — everything added through this form is a Manual
      // download (see docs/providers.md#managed-vs-manual), and category has
      // no effect there (it only drives save-path resolution for Managed
      // downloads, which are the only ones internal/importer auto-fetches).
      // Deliberately left out for now rather than wired up as a cosmetic-only
      // label; revisit if the Manual tab ever needs its own organization
      // scheme — see ROADMAP.md.
      if (protocol === 'torrent') {
        if (mode === 'file') await addTorrent(apiKey, { file: file as File })
        else await addTorrent(apiKey, { magnet: link.trim() })
      } else if (protocol === 'usenet') {
        if (mode === 'file') await addUsenet(apiKey, { file: file as File })
        else await addUsenet(apiKey, { url: link.trim() })
      } else {
        await addWebDownload(apiKey, { link: link.trim() })
      }
      onAdded()
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
          <h2>Add to Manual</h2>
          <button className="detail-close" onClick={onClose} title="Close">
            ✕
          </button>
        </div>

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
