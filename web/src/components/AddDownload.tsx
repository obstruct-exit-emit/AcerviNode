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
  const [category, setCategory] = useState('')
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
      const cat = category.trim() || undefined
      if (protocol === 'torrent') {
        if (mode === 'file') await addTorrent(apiKey, { file: file as File, category: cat })
        else await addTorrent(apiKey, { magnet: link.trim(), category: cat })
      } else {
        if (mode === 'file') await addUsenet(apiKey, { file: file as File, category: cat })
        else await addUsenet(apiKey, { url: link.trim(), category: cat })
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

            <input type="text" placeholder="Category (optional)" value={category} onChange={(e) => setCategory(e.target.value)} />

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
