import { useEffect, useState, type FormEvent } from 'react'
import { getProviderSettings, setTorBoxApiKey, ApiError, type ProviderSettings } from '../api'

interface Props {
  apiKey: string
}

export function Settings({ apiKey }: Props) {
  const [settings, setSettings] = useState<ProviderSettings | null>(null)
  const [torboxKey, setTorboxKey] = useState('')
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'error'; message?: string }>({ kind: 'idle' })

  async function load() {
    try {
      setSettings(await getProviderSettings(apiKey))
    } catch {
      // The dashboard's own polling will surface auth/connectivity errors;
      // this view just leaves the form usable either way.
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!torboxKey.trim()) return
    setStatus({ kind: 'saving' })
    try {
      await setTorBoxApiKey(apiKey, torboxKey.trim())
      setTorboxKey('')
      setStatus({ kind: 'saved' })
      await load()
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  const configured = settings?.torbox?.configured ?? false

  return (
    <div className="settings">
      <section className="settings-card">
        <h2>TorBox</h2>
        <p className="settings-status">
          {configured ? (
            <span className="badge badge-ready_for_import">Configured</span>
          ) : (
            <span className="badge badge-queued">Not configured</span>
          )}
        </p>
        <p className="settings-help">
          {configured
            ? 'Enter a new key below to replace the current one — takes effect immediately, no restart needed.'
            : 'Add your TorBox API key to enable the qBittorrent and SABnzbd compat shims.'}
        </p>
        <form onSubmit={handleSubmit}>
          <input
            type="password"
            placeholder="TorBox API key"
            value={torboxKey}
            onChange={(e) => setTorboxKey(e.target.value)}
          />
          <button type="submit" disabled={status.kind === 'saving' || !torboxKey.trim()}>
            {status.kind === 'saving' ? 'Saving…' : 'Save'}
          </button>
        </form>
        {status.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
        {status.kind === 'error' && <p className="settings-error">Failed to save: {status.message}</p>}
      </section>
    </div>
  )
}
