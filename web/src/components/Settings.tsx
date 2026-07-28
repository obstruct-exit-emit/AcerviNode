import { useEffect, useState, type FormEvent } from 'react'
import {
  addCategory,
  getCategories,
  getGeneralSettings,
  getProviderSettings,
  regenerateApiKey,
  setTorBoxApiKey,
  testTorBoxConnection,
  updateGeneralSettings,
  ApiError,
  type Categories,
  type GeneralSettings,
  type GeneralUpdateInput,
  type ProviderSettings,
} from '../api'

interface Props {
  apiKey: string
  // Called after a successful regenerate with the new key — App.tsx uses
  // this to keep the UI's own session (and localStorage) in sync, since the
  // key it's currently authenticated with just stopped working everywhere.
  onApiKeyChanged: (newKey: string) => void
}

const LOG_LEVELS = ['debug', 'info', 'warn', 'error']

export function Settings({ apiKey, onApiKeyChanged }: Props) {
  const [settings, setSettings] = useState<ProviderSettings | null>(null)
  const [torboxKey, setTorboxKey] = useState('')
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'error'; message?: string }>({ kind: 'idle' })
  const [general, setGeneral] = useState<GeneralSettings | null>(null)
  const [form, setForm] = useState<GeneralUpdateInput | null>(null)
  const [keyRevealed, setKeyRevealed] = useState(false)
  const [copyStatus, setCopyStatus] = useState<'idle' | 'copied'>('idle')
  const [regenStatus, setRegenStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })
  const [generalStatus, setGeneralStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'restart' | 'error'; message?: string }>({
    kind: 'idle',
  })
  const [testStatus, setTestStatus] = useState<{ kind: 'idle' | 'testing' | 'ok' | 'error'; message?: string; latencyMs?: number }>({
    kind: 'idle',
  })
  const [categories, setCategories] = useState<Categories | null>(null)
  const [newCategory, setNewCategory] = useState('')
  const [newCategoryProtocol, setNewCategoryProtocol] = useState<'torrent' | 'usenet'>('torrent')
  const [categoryStatus, setCategoryStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  async function load() {
    try {
      const [providerSettings, generalSettings, cats] = await Promise.all([
        getProviderSettings(apiKey),
        getGeneralSettings(apiKey),
        getCategories(apiKey),
      ])
      setSettings(providerSettings)
      setGeneral(generalSettings)
      setForm({
        port: generalSettings.port,
        data_dir: generalSettings.data_dir,
        download_dir: generalSettings.download_dir,
        log_level: generalSettings.log_level,
        import_interval_seconds: generalSettings.import_interval_seconds,
        import_max_retries: generalSettings.import_max_retries,
      })
      setCategories(cats)
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
      setTestStatus({ kind: 'idle' })
      await load()
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  async function handleTestConnection() {
    setTestStatus({ kind: 'testing' })
    try {
      const result = await testTorBoxConnection(apiKey)
      if (result.ok) {
        setTestStatus({ kind: 'ok', latencyMs: result.latency_ms })
      } else {
        setTestStatus({ kind: 'error', message: result.error })
      }
    } catch (err) {
      setTestStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  async function handleGeneralSubmit(e: FormEvent) {
    e.preventDefault()
    if (!form) return
    setGeneralStatus({ kind: 'saving' })
    try {
      const { restart_required } = await updateGeneralSettings(apiKey, form)
      setGeneralStatus({ kind: restart_required ? 'restart' : 'saved' })
      await load()
    } catch (err) {
      setGeneralStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  async function handleAddCategory(e: FormEvent) {
    e.preventDefault()
    if (!newCategory.trim()) return
    setCategoryStatus({ kind: 'saving' })
    try {
      await addCategory(apiKey, newCategoryProtocol, newCategory.trim())
      setNewCategory('')
      setCategoryStatus({ kind: 'idle' })
      setCategories(await getCategories(apiKey))
    } catch (err) {
      setCategoryStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  const configured = settings?.torbox?.configured ?? false

  return (
    <div className="settings">
      <section className="settings-card">
        <h2>General</h2>
        {general && (
          <div className="api-key-row">
            <code className="api-key-value">{keyRevealed ? general.api_key : '•'.repeat(24)}</code>
            <button type="button" onClick={() => setKeyRevealed((v) => !v)} title={keyRevealed ? 'Hide' : 'Reveal'}>
              {keyRevealed ? 'Hide' : 'Reveal'}
            </button>
            <button type="button" onClick={handleCopyKey} title="Copy to clipboard">
              {copyStatus === 'copied' ? 'Copied!' : 'Copy'}
            </button>
          </div>
        )}
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

        {form && (
          <form className="general-form" onSubmit={handleGeneralSubmit}>
            <label>
              Download dir
              <input
                type="text"
                value={form.download_dir}
                onChange={(e) => setForm({ ...form, download_dir: e.target.value })}
              />
            </label>
            <label>
              Log level
              <select value={form.log_level} onChange={(e) => setForm({ ...form, log_level: e.target.value })}>
                {LOG_LEVELS.map((level) => (
                  <option key={level} value={level}>
                    {level}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Import interval (seconds)
              <input
                type="number"
                min={1}
                value={form.import_interval_seconds}
                onChange={(e) => setForm({ ...form, import_interval_seconds: Number(e.target.value) })}
              />
            </label>
            <label>
              Import max retries
              <input
                type="number"
                min={1}
                value={form.import_max_retries}
                onChange={(e) => setForm({ ...form, import_max_retries: Number(e.target.value) })}
              />
            </label>

            <p className="settings-help">
              The four above apply immediately, no restart needed. Port and data dir need a restart to take effect —
              edit them here to save the new value for next time.
            </p>
            <label>
              Port
              <input type="number" min={1} max={65535} value={form.port} onChange={(e) => setForm({ ...form, port: Number(e.target.value) })} />
            </label>
            <label>
              Data dir
              <input type="text" value={form.data_dir} onChange={(e) => setForm({ ...form, data_dir: e.target.value })} />
            </label>

            <button type="submit" disabled={generalStatus.kind === 'saving'}>
              {generalStatus.kind === 'saving' ? 'Saving…' : 'Save'}
            </button>
            {generalStatus.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
            {generalStatus.kind === 'restart' && (
              <p className="settings-warning">Saved — port and/or data dir changed, restart AcerviNode to apply.</p>
            )}
            {generalStatus.kind === 'error' && <p className="settings-error">Failed to save: {generalStatus.message}</p>}
          </form>
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

        {configured && (
          <>
            <button type="button" className="test-connection-btn" onClick={handleTestConnection} disabled={testStatus.kind === 'testing'}>
              {testStatus.kind === 'testing' ? 'Testing…' : 'Test connection'}
            </button>
            {testStatus.kind === 'ok' && (
              <p className="settings-success">Connected — {testStatus.latencyMs}ms</p>
            )}
            {testStatus.kind === 'error' && <p className="settings-error">Connection failed: {testStatus.message}</p>}
          </>
        )}
      </section>

      <section className="settings-card">
        <h2>Categories</h2>
        <p className="settings-help">
          Populated as Sonarr/Radarr declare categories, or add one manually below — useful for pre-filling the "Add
          Download" form's category field.
        </p>
        {categories && (
          <div className="category-lists">
            <div>
              <h3>Torrent</h3>
              {categories.torrent.length === 0 ? (
                <p className="text-muted">None yet</p>
              ) : (
                <ul className="category-list">
                  {categories.torrent.map((c) => (
                    <li key={c}>{c}</li>
                  ))}
                </ul>
              )}
            </div>
            <div>
              <h3>Usenet</h3>
              {categories.usenet.length === 0 ? (
                <p className="text-muted">None yet</p>
              ) : (
                <ul className="category-list">
                  {categories.usenet.map((c) =>
                    c === '*' ? (
                      <li key={c} title="SABnzbd's default catch-all category — always exists">
                        Default
                      </li>
                    ) : (
                      <li key={c}>{c}</li>
                    ),
                  )}
                </ul>
              )}
            </div>
          </div>
        )}
        <form className="add-category-form" onSubmit={handleAddCategory}>
          <select value={newCategoryProtocol} onChange={(e) => setNewCategoryProtocol(e.target.value as 'torrent' | 'usenet')}>
            <option value="torrent">Torrent</option>
            <option value="usenet">Usenet</option>
          </select>
          <input type="text" placeholder="Category name" value={newCategory} onChange={(e) => setNewCategory(e.target.value)} />
          <button type="submit" disabled={categoryStatus.kind === 'saving' || !newCategory.trim()}>
            {categoryStatus.kind === 'saving' ? 'Adding…' : 'Add'}
          </button>
        </form>
        {categoryStatus.kind === 'error' && <p className="settings-error">Failed to add: {categoryStatus.message}</p>}
      </section>
    </div>
  )
}
