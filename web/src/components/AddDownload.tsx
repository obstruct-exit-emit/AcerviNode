import { useEffect, useState, type FormEvent } from 'react'
import { addTorrent, addUsenet, ApiError, type ProviderStatus } from '../api'

type Protocol = 'torrent' | 'usenet'
type InputMode = 'link' | 'file'

interface Props {
  apiKey: string
  providers: ProviderStatus[]
  onClose: () => void
  onAdded: () => void
}

export function AddDownload({ apiKey, providers, onClose, onAdded }: Props) {
  const torrentAvailable = providers.some((p) => p.torrent_capable)
  const usenetAvailable = providers.some((p) => p.usenet_capable)

  const [protocol, setProtocol] = useState<Protocol>(torrentAvailable ? 'torrent' : 'usenet')
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
    if (mode === 'link' && !link.trim()) return
    if (mode === 'file' && !file) return

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
      } else {
        if (mode === 'file') await addUsenet(apiKey, { file: file as File })
        else await addUsenet(apiKey, { url: link.trim() })
      }
      onAdded()
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  return (
    <div className="detail-overlay" onClick={onClose}>
      <div className="detail-panel add-download-panel" onClick={(e) => e.stopPropagation()}>
        <div className="detail-header">
          <h2>Add download</h2>
          <button className="detail-close" onClick={onClose} title="Close">
            ✕
          </button>
        </div>

        {torrentAvailable && usenetAvailable && (
          <div className="protocol-tabs">
            <button
              type="button"
              className={protocol === 'torrent' ? 'tab tab-active' : 'tab'}
              onClick={() => {
                setProtocol('torrent')
                setStatus({ kind: 'idle' })
              }}
            >
              Torrent
            </button>
            <button
              type="button"
              className={protocol === 'usenet' ? 'tab tab-active' : 'tab'}
              onClick={() => {
                setProtocol('usenet')
                setStatus({ kind: 'idle' })
              }}
            >
              Usenet
            </button>
          </div>
        )}

        {!torrentAvailable && !usenetAvailable && (
          <p className="settings-help">No provider is configured yet — add one under Settings first.</p>
        )}

        {(protocol === 'torrent' ? torrentAvailable : usenetAvailable) && (
          <form onSubmit={handleSubmit}>
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

            {mode === 'link' ? (
              <input
                type="text"
                placeholder={protocol === 'torrent' ? 'magnet:?xt=urn:btih:...' : 'https://example.com/release.nzb'}
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
              disabled={status.kind === 'saving' || (mode === 'link' ? !link.trim() : !file)}
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
