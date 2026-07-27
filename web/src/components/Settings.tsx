import { useEffect, useState, type FormEvent } from 'react'
import {
  getGeneralSettings,
  getProviderSettings,
  regenerateApiKey,
  setTorBoxApiKey,
  ApiError,
  type GeneralSettings,
  type ProviderSettings,
} from '../api'

interface Props {
  apiKey: string
  // Called after a successful regenerate with the new key — App.tsx uses
  // this to keep the UI's own session (and localStorage) in sync, since the
  // key it's currently authenticated with just stopped working everywhere.
  onApiKeyChanged: (newKey: string) => void
}

export function Settings({ apiKey, onApiKeyChanged }: Props) {
  const [settings, setSettings] = useState<ProviderSettings | null>(null)
  const [torboxKey, setTorboxKey] = useState('')
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'error'; message?: string }>({ kind: 'idle' })
  const [general, setGeneral] = useState<GeneralSettings | null>(null)
  const [keyRevealed, setKeyRevealed] = useState(false)
  const [copyStatus, setCopyStatus] = useState<'idle' | 'copied'>('idle')
  const [regenStatus, setRegenStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  async function load() {
    try {
      const [providerSettings, generalSettings] = await Promise.all([getProviderSettings(apiKey), getGeneralSettings(apiKey)])
      setSettings(providerSettings)
      setGeneral(generalSettings)
    } catch {
      // The dashboard's own polling will surface auth/connectivity errors;
      // this view just leaves the form usable either way.
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function handleCopyKey() {
    if (!general) return
    try {
      await navigator.clipboard.writeText(general.api_key)
      setCopyStatus('copied')
      setTimeout(() => setCopyStatus('idle'), 1500)
    } catch {
      // Clipboard access can be denied by the browser; the key is still
      // visible once revealed, so this is a soft failure.
    }
  }

  async function handleRegenerate() {
    if (!confirm('Regenerate the AcerviNode API key? The current key stops working immediately everywhere — this browser, and any Sonarr/Radarr client using it — until updated with the new one.')) {
      return
    }
    setRegenStatus({ kind: 'saving' })
    try {
      const { api_key } = await regenerateApiKey(apiKey)
      setRegenStatus({ kind: 'idle' })
      // Update locally rather than re-fetching: the key this component was
      // just called with is now invalid everywhere, so a re-fetch using it
      // would only 401.
      setGeneral((g) => (g ? { ...g, api_key } : g))
      onApiKeyChanged(api_key)
    } catch (err) {
      setRegenStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

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
        <h2>General</h2>
        {general && (
          <>
            <div className="api-key-row">
              <code className="api-key-value">{keyRevealed ? general.api_key : '•'.repeat(24)}</code>
              <button type="button" onClick={() => setKeyRevealed((v) => !v)} title={keyRevealed ? 'Hide' : 'Reveal'}>
                {keyRevealed ? 'Hide' : 'Reveal'}
              </button>
              <button type="button" onClick={handleCopyKey} title="Copy to clipboard">
                {copyStatus === 'copied' ? 'Copied!' : 'Copy'}
              </button>
            </div>
            <p className="settings-help">
              This is the key both compat shims and the native API check — the same one Sonarr/Radarr need when
              adding AcerviNode as a download client.
            </p>
            <button
              type="button"
              className="regenerate-btn"
              onClick={handleRegenerate}
              disabled={regenStatus.kind === 'saving'}
            >
              {regenStatus.kind === 'saving' ? 'Regenerating…' : 'Regenerate API key'}
            </button>
            {regenStatus.kind === 'error' && <p className="settings-error">Failed to regenerate: {regenStatus.message}</p>}

            <dl className="general-info">
              <div>
                <dt>Port</dt>
                <dd>{general.port}</dd>
              </div>
              <div>
                <dt>Data dir</dt>
                <dd className="mono">{general.data_dir}</dd>
              </div>
              <div>
                <dt>Download dir</dt>
                <dd className="mono">{general.download_dir}</dd>
              </div>
              <div>
                <dt>Log level</dt>
                <dd>{general.log_level}</dd>
              </div>
              <div>
                <dt>Import interval</dt>
                <dd>{general.import_interval_seconds}s</dd>
              </div>
              <div>
                <dt>Import max retries</dt>
                <dd>{general.import_max_retries}</dd>
              </div>
            </dl>
            <p className="settings-help">
              These are set in <code>config.yaml</code> (or matching <code>ACERVINODE_*</code> env vars) and need a
              restart to change.
            </p>
          </>
        )}
      </section>

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
