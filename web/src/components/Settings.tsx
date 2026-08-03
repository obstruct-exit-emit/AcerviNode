import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import {
  getCategories,
  getGeneralSettings,
  getProviderSettings,
  getTorBoxAccount,
  regenerateApiKey,
  regenerateCertificate,
  restartServer,
  setCategoryPath,
  setTorBoxApiKey,
  testTorBoxConnection,
  updateGeneralSettings,
  ApiError,
  type Categories,
  type GeneralSettings,
  type GeneralUpdateInput,
  type ProviderSettings,
  type TorBoxAccount,
} from '../api'
import { getDefaultDirectory, pickAndRememberDirectory, forgetDefaultDirectory, supportsDirectoryPicker } from '../fsAccess'
import { formatBytes } from '../format'
import { SecuritySettings } from './SecuritySettings'

// Settings groups, *arr-style (matching LibriNode's own SettingsView): pages
// organized by concern instead of one long scroll, each with a one-line
// blurb so the group a user lands on always explains itself. No icons —
// unlike LibriNode's own version, AcerviNode's UI doesn't use emoji as UI
// chrome anywhere else (only the 📦 header logo), and a row of five
// different emoji next to plain-text tabs read as visually inconsistent,
// especially against the top-level Manual/Managed/Settings tabs, which are
// text-only. Smaller than LibriNode's own six groups, too — AcerviNode's
// whole settings surface is one provider, categories, and login accounts,
// not a full media-manager's worth of libraries/quality profiles/indexers.
const settingsGroups = [
  { name: 'General', blurb: "This instance's API key and import/cleanup behavior." },
  { name: 'Provider', blurb: 'The TorBox account AcerviNode resolves every download through.' },
  { name: 'Categories', blurb: 'Pre-register categories for Sonarr/Radarr, and optionally redirect their downloads to a specific directory.' },
  { name: 'Downloads', blurb: "This browser's remembered folder for the Manual tab's downloads." },
  { name: 'Security', blurb: 'Login accounts on top of the API key, and their roles.' },
] as const
type SettingsGroup = (typeof settingsGroups)[number]['name']

// Section groups related fields inside a card under a small heading, with
// optional help text — so a long form (General's, especially) reads as a
// few labelled blocks instead of one undifferentiated stack of inputs.
function Section({ title, help, children }: { title: string; help?: ReactNode; children: ReactNode }) {
  return (
    <div className="settings-section">
      <h3>{title}</h3>
      {help != null && <p className="settings-help">{help}</p>}
      {children}
    </div>
  )
}

// One row of the "Save path overrides" list — kept as its own component,
// keyed by category name, so an in-progress edit in one row survives a
// `load()` triggered by saving a different row (see Settings' onSaved).
function CategoryPathRow({ name, currentPath, apiKey, onSaved }: { name: string; currentPath: string; apiKey: string; onSaved: () => void }) {
  const [path, setPath] = useState(currentPath)
  const [savedPath, setSavedPath] = useState(currentPath)
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'error'; message?: string }>({ kind: 'idle' })

  async function handleSave() {
    const trimmed = path.trim()
    setStatus({ kind: 'saving' })
    try {
      await setCategoryPath(apiKey, name, trimmed)
      setPath(trimmed)
      setSavedPath(trimmed)
      setStatus({ kind: 'saved' })
      onSaved()
    } catch (err) {
      setStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  return (
    <div className="category-path-row">
      <span className="category-path-name">{name}</span>
      <input
        type="text"
        placeholder="Default: download_dir/<category>/<name>"
        value={path}
        onChange={(e) => {
          setPath(e.target.value)
          if (status.kind !== 'idle') setStatus({ kind: 'idle' })
        }}
      />
      <button type="button" onClick={handleSave} disabled={status.kind === 'saving' || path.trim() === savedPath}>
        {status.kind === 'saving' ? 'Saving…' : 'Save'}
      </button>
      {status.kind === 'saved' && <span className="settings-success">Saved</span>}
      {status.kind === 'error' && <span className="settings-error">{status.message}</span>}
    </div>
  )
}

interface Props {
  apiKey: string
}

const LOG_LEVELS = ['debug', 'info', 'warn', 'error']

export function Settings({ apiKey }: Props) {
  const [group, setGroup] = useState<SettingsGroup>('General')
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
  const [account, setAccount] = useState<TorBoxAccount | null>(null)
  const [categories, setCategories] = useState<Categories | null>(null)
  const [newOverrideCategory, setNewOverrideCategory] = useState('')
  const [newOverridePath, setNewOverridePath] = useState('')
  const [newOverrideStatus, setNewOverrideStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })
  const [defaultFolderName, setDefaultFolderName] = useState<string | null>(null)
  const [folderStatus, setFolderStatus] = useState<{ kind: 'idle' | 'error'; message?: string }>({ kind: 'idle' })
  const [restartStatus, setRestartStatus] = useState<{ kind: 'idle' | 'restarting' | 'error'; message?: string; supervised?: boolean }>({
    kind: 'idle',
  })
  const [regenCertStatus, setRegenCertStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })

  async function load() {
    try {
      const [providerSettings, generalSettings, cats, accountStatus] = await Promise.all([
        getProviderSettings(apiKey),
        getGeneralSettings(apiKey),
        getCategories(apiKey),
        // Not configured, or configured with a provider that doesn't support
        // account status, both come back as available:false rather than
        // throwing — see handleGetAccountStatus's doc comment — so this is
        // safe to call unconditionally alongside the rest of load().
        getTorBoxAccount(apiKey),
      ])
      setSettings(providerSettings)
      setGeneral(generalSettings)
      setAccount(accountStatus)
      setForm({
        port: generalSettings.port,
        data_dir: generalSettings.data_dir,
        download_dir: generalSettings.download_dir,
        log_level: generalSettings.log_level,
        import_interval_seconds: generalSettings.import_interval_seconds,
        import_max_retries: generalSettings.import_max_retries,
        max_concurrent_downloads: generalSettings.max_concurrent_downloads,
        import_fetch_timeout_seconds: generalSettings.import_fetch_timeout_seconds,
        cleanup_after_days: generalSettings.cleanup_after_days,
        download_dir_mode: generalSettings.download_dir_mode,
        fast_poll_interval_seconds: generalSettings.fast_poll_interval_seconds,
        tls_enabled: generalSettings.tls_enabled,
        tls_port: generalSettings.tls_port,
        // Round-tripped unchanged, same as data_dir — neither has an
        // editable field in this form (config/env-only, see GeneralSettings).
        tls_cert_file: generalSettings.tls_cert_file,
        tls_key_file: generalSettings.tls_key_file,
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

  useEffect(() => {
    if (!supportsDirectoryPicker()) return
    // queryPermission alone needs no user gesture, so this is safe to check
    // on mount rather than only inside a click handler.
    getDefaultDirectory().then((handle) => setDefaultFolderName(handle?.name ?? null))
  }, [])

  async function handleChangeFolder() {
    setFolderStatus({ kind: 'idle' })
    try {
      const handle = await pickAndRememberDirectory()
      if (handle) setDefaultFolderName(handle.name)
    } catch (err) {
      setFolderStatus({ kind: 'error', message: err instanceof Error ? err.message : String(err) })
    }
  }

  async function handleForgetFolder() {
    await forgetDefaultDirectory()
    setDefaultFolderName(null)
  }

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
    if (!confirm('Regenerate the AcerviNode API key? The current key stops working immediately everywhere — any Sonarr/Radarr client or script using it — until updated with the new one. The web UI itself is unaffected, since it authenticates via your login session, not the API key.')) {
      return
    }
    setRegenStatus({ kind: 'saving' })
    try {
      const { api_key } = await regenerateApiKey(apiKey)
      setRegenStatus({ kind: 'idle' })
      setGeneral((g) => (g ? { ...g, api_key } : g))
    } catch (err) {
      setRegenStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  async function handleRestartNow() {
    if (!confirm('Restart AcerviNode now? The web UI and any in-progress requests will briefly disconnect while it comes back up.')) {
      return
    }
    setRestartStatus({ kind: 'restarting' })
    try {
      const { supervised } = await restartServer(apiKey)
      setRestartStatus({ kind: 'restarting', supervised })
    } catch (err) {
      setRestartStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  async function handleRegenerateCert() {
    if (
      !confirm(
        "Regenerate the TLS certificate? Every device that already trusted the current one will need to click through the browser warning again after this. Use this only if the certificate no longer matches how you reach AcerviNode (e.g. its IP changed) — a restart is required afterward either way.",
      )
    ) {
      return
    }
    setRegenCertStatus({ kind: 'saving' })
    try {
      await regenerateCertificate(apiKey)
      setRegenCertStatus({ kind: 'idle' })
      setGeneralStatus({ kind: 'restart' })
    } catch (err) {
      setRegenCertStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
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

  // Sets a save-path override for a category directly, by name — no
  // prerequisite "declare this category first" step. The backend never
  // required one (SetCategoryPath accepts any non-empty name), the old UI
  // just artificially gated the form on Sonarr/Radarr (or a manual "add
  // category" step) having been seen first.
  //
  // path is optional: submitting with just a category name registers it
  // with both compat shims (no path override applied) — the only way to
  // pre-declare a category before Sonarr/Radarr ever connects. This matters
  // specifically for the SABnzbd shim: real SABnzbd (and this shim,
  // faithfully) has no API to create a category on the fly, so Radarr's own
  // "Test" step rejects a category outright if AcerviNode doesn't already
  // know about it — found live.
  async function handleAddOverride(e: FormEvent) {
    e.preventDefault()
    const category = newOverrideCategory.trim()
    const path = newOverridePath.trim()
    if (!category) return
    setNewOverrideStatus({ kind: 'saving' })
    try {
      await setCategoryPath(apiKey, category, path)
      setNewOverrideCategory('')
      setNewOverridePath('')
      setNewOverrideStatus({ kind: 'idle' })
      await load()
    } catch (err) {
      setNewOverrideStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  const configured = settings?.torbox?.configured ?? false
  const current = settingsGroups.find((g) => g.name === group) ?? settingsGroups[0]

  return (
    <div className="settings">
      <header className="settings-header">
        <h1>Settings</h1>
        <p className="muted">{current.blurb}</p>
      </header>

      <nav className="subnav" aria-label="Settings sections">
        {settingsGroups.map((g) => (
          <button
            key={g.name}
            type="button"
            className={g.name === group ? 'tab tab-active' : 'tab'}
            aria-current={g.name === group ? 'page' : undefined}
            onClick={() => setGroup(g.name)}
          >
            {g.name}
          </button>
        ))}
      </nav>

      {group === 'General' && (
        <section className="settings-card">
          <Section
            title="API key"
            help="This is the key both compat shims and the native API check — the same one Sonarr/Radarr need when adding AcerviNode as a download client."
          >
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
            <button type="button" className="regenerate-btn" onClick={handleRegenerate} disabled={regenStatus.kind === 'saving'}>
              {regenStatus.kind === 'saving' ? 'Regenerating…' : 'Regenerate API key'}
            </button>
            {regenStatus.kind === 'error' && <p className="settings-error">Failed to regenerate: {regenStatus.message}</p>}
          </Section>

          {form && (
            <form className="settings-form-stack" onSubmit={handleGeneralSubmit}>
              <Section title="Import & cleanup" help="Applies immediately, no restart needed.">
                <div className="general-form">
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
                    Fast poll interval (seconds)
                    <input
                      type="number"
                      min={1}
                      value={form.fast_poll_interval_seconds}
                      onChange={(e) => setForm({ ...form, fast_poll_interval_seconds: Number(e.target.value) })}
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
                  <label>
                    Max concurrent downloads
                    <input
                      type="number"
                      min={1}
                      value={form.max_concurrent_downloads}
                      onChange={(e) => setForm({ ...form, max_concurrent_downloads: Number(e.target.value) })}
                    />
                  </label>
                  <label>
                    Import fetch timeout (seconds)
                    <input
                      type="number"
                      min={1}
                      value={form.import_fetch_timeout_seconds}
                      onChange={(e) => setForm({ ...form, import_fetch_timeout_seconds: Number(e.target.value) })}
                    />
                  </label>
                  <label>
                    Clean up Managed downloads after (days, 0 = off)
                    <input
                      type="number"
                      min={0}
                      value={form.cleanup_after_days}
                      onChange={(e) => setForm({ ...form, cleanup_after_days: Number(e.target.value) })}
                    />
                  </label>
                </div>
                <p className="settings-help">
                  The fast poll interval checks each actively downloading Managed download individually, so a
                  finished download is noticed within a few seconds instead of waiting for the next full import
                  interval — separate from (and much cheaper than) the import interval above, since it only ever
                  checks downloads already known to be in progress. The default (3s) was tuned against a real
                  provider to stay responsive without risking a rate limit; raise it if you routinely have many
                  downloads active at once. Max concurrent downloads bounds how many provider_completed downloads
                  are fetched to disk at once (previously always strictly one at a time). The fetch timeout covers a
                  single file's whole transfer, not just connecting — raise it if large files on a slow connection
                  are failing partway through. Cleanup only ever touches a Managed download once it's reached "ready
                  for import" (already handed off to Sonarr/Radarr) and stayed there this long — a Manual download
                  is never auto-deleted. 0 disables cleanup entirely (the default).
                </p>
              </Section>

              <Section title="Instance">
                <div className="general-form">
                  <label>
                    Download dir
                    <input type="text" value={form.download_dir} onChange={(e) => setForm({ ...form, download_dir: e.target.value })} />
                  </label>
                  <label>
                    Download directory permissions
                    <input
                      type="text"
                      placeholder="0777"
                      pattern="[0-7]{3,4}"
                      value={form.download_dir_mode}
                      onChange={(e) => setForm({ ...form, download_dir_mode: e.target.value })}
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
                    Port
                    <input
                      type="number"
                      min={1}
                      max={65535}
                      value={form.port}
                      onChange={(e) => setForm({ ...form, port: Number(e.target.value) })}
                    />
                  </label>
                </div>
                <p className="settings-help">
                  Download dir applies immediately (no restart) and is the fallback destination when a Managed
                  download's *arr app didn't supply its own path. Download directory permissions (octal, e.g.
                  "0777") control who can move or delete files AcerviNode fetches — world-writable by default, so
                  Sonarr/Radarr can hardlink or clean up a completed download regardless of what user/container they
                  run as; tighten this (e.g. "0755") only if AcerviNode's own user already matches the rest of your
                  stack. Applies immediately, including retroactively to a directory that already exists. Port needs
                  a restart to take effect — edit it here to save the new value for next time. Data dir isn't shown
                  here at all — changing it doesn't move your existing database, so editing it in this form would
                  look like everything vanished after a restart. Set it via <code>config.yaml</code> or{' '}
                  <code>ACERVINODE_DATA_DIR</code> instead, and move the database file yourself first.
                </p>
              </Section>

              <Section
                title="HTTPS"
                help="Adds a second, encrypted listener alongside the existing plain-HTTP one — nothing already
                pointed at http://... (Sonarr/Radarr, scripts, bookmarks) is affected either way. Mainly useful for
                the browser's folder-picker download mode, which only works over HTTPS or localhost. Uses a
                self-signed certificate generated automatically on first start — your browser will show a one-time
                'not trusted' warning to click through, the same as any self-signed cert. Requires a restart to
                apply."
              >
                <div className="general-form">
                  <label className="toggle-row">
                    <input
                      type="checkbox"
                      checked={form.tls_enabled}
                      onChange={(e) => setForm({ ...form, tls_enabled: e.target.checked })}
                    />
                    Enable HTTPS
                  </label>
                  <label>
                    HTTPS port
                    <input
                      type="number"
                      min={1}
                      max={65535}
                      value={form.tls_port}
                      onChange={(e) => setForm({ ...form, tls_port: Number(e.target.value) })}
                    />
                  </label>
                </div>
                {general?.tls_enabled && (
                  <>
                    <button type="button" onClick={handleRegenerateCert} disabled={regenCertStatus.kind === 'saving'}>
                      {regenCertStatus.kind === 'saving' ? 'Regenerating…' : 'Regenerate certificate'}
                    </button>
                    <p className="settings-help">
                      Only needed if the certificate no longer matches how you reach AcerviNode (e.g. its IP
                      changed) — every device that already trusted the old one will need to click through the
                      browser warning again afterward.
                    </p>
                    {regenCertStatus.kind === 'error' && (
                      <p className="settings-error">Failed to regenerate: {regenCertStatus.message}</p>
                    )}
                  </>
                )}
              </Section>

              <div className="general-form">
                <button type="submit" disabled={generalStatus.kind === 'saving'}>
                  {generalStatus.kind === 'saving' ? 'Saving…' : 'Save'}
                </button>
                {generalStatus.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
                {generalStatus.kind === 'restart' && (
                  <div className="restart-notice">
                    <p className="settings-warning">Saved — restart AcerviNode to apply.</p>
                    <button type="button" onClick={handleRestartNow} disabled={restartStatus.kind === 'restarting'}>
                      Restart now
                    </button>
                  </div>
                )}
                {generalStatus.kind === 'error' && <p className="settings-error">Failed to save: {generalStatus.message}</p>}
                {restartStatus.kind === 'restarting' && restartStatus.supervised === false && (
                  <p className="settings-warning">
                    AcerviNode isn't running under a supervisor (e.g. systemd) — it will stop now, but nothing will
                    automatically start it back up.
                  </p>
                )}
                {restartStatus.kind === 'restarting' && restartStatus.supervised !== false && (
                  <p className="settings-success">Restarting — this page will recover automatically in a few seconds.</p>
                )}
                {restartStatus.kind === 'error' && <p className="settings-error">Failed to restart: {restartStatus.message}</p>}
              </div>
            </form>
          )}
        </section>
      )}

      {group === 'Provider' && (
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
            <input type="password" placeholder="TorBox API key" value={torboxKey} onChange={(e) => setTorboxKey(e.target.value)} />
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
              {testStatus.kind === 'ok' && <p className="settings-success">Connected — {testStatus.latencyMs}ms</p>}
              {testStatus.kind === 'error' && <p className="settings-error">Connection failed: {testStatus.message}</p>}
            </>
          )}

          {account?.available && account.cooldown_until && new Date(account.cooldown_until).getTime() > Date.now() && (
            <p className="settings-error">
              ⚠ TorBox is restricting this account until{' '}
              {new Date(account.cooldown_until).toLocaleString()} — every download's progress can look frozen with
              no other visible cause while this is active. Not something AcerviNode can do anything about; it'll
              clear on its own.
            </p>
          )}

          {account?.available && (
            <dl className="detail-meta account-status">
              <dt>Plan</dt>
              <dd>
                {account.plan_name}
                {account.is_subscribed ? ' (subscribed)' : ''}
              </dd>
              {account.premium_expires_at && (
                <>
                  <dt>Premium expires</dt>
                  <dd>{new Date(account.premium_expires_at).toLocaleDateString()}</dd>
                </>
              )}
              <dt>Total downloaded</dt>
              <dd>{formatBytes(account.total_bytes_downloaded ?? 0)}</dd>
            </dl>
          )}
        </section>
      )}

      {group === 'Categories' && (
        <section className="settings-card">
          <h2>Categories</h2>
          <p className="settings-help">
            Register a category name here <strong>before</strong> configuring it in Sonarr/Radarr — real SABnzbd has
            no way to create a category on the fly, so a brand new category typed into Radarr's SABnzbd client gets
            rejected outright by its own "Test" step unless AcerviNode already knows about it. Every *arr app's own
            <em> default</em> category (Radarr's "movies"/"radarr", Sonarr's "tv"/"tv-sonarr", Lidarr's
            "music"/"lidarr", Readarr's "Readarr"/"readarr") is already registered automatically — this is only
            needed for a custom name you've typed in instead. Leave the path blank to just register the name; fill
            it in to also redirect that category's completed downloads to a specific directory instead of the
            default <code>download_dir/&lt;category&gt;</code> (e.g. to route it to a different disk or mount). Only
            affects Managed (Sonarr/Radarr) downloads — category has no effect on Manual ones. Clear an existing
            override's path and save to remove it (the category itself stays registered).
          </p>

          {categories &&
            Object.entries(categories.paths)
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([name, path]) => <CategoryPathRow key={name} name={name} currentPath={path} apiKey={apiKey} onSaved={load} />)}

          <form className="add-category-form" onSubmit={handleAddOverride}>
            <input
              type="text"
              placeholder="Category (e.g. tv-sonarr)"
              value={newOverrideCategory}
              onChange={(e) => setNewOverrideCategory(e.target.value)}
            />
            <input
              type="text"
              placeholder="Path (optional, e.g. /mnt/tv)"
              value={newOverridePath}
              onChange={(e) => setNewOverridePath(e.target.value)}
            />
            <button type="submit" disabled={newOverrideStatus.kind === 'saving' || !newOverrideCategory.trim()}>
              {newOverrideStatus.kind === 'saving' ? 'Adding…' : 'Register'}
            </button>
          </form>
          {newOverrideStatus.kind === 'error' && <p className="settings-error">Failed to add: {newOverrideStatus.message}</p>}

          {categories && (categories.torrent.length > 0 || categories.usenet.length > 0) && (
            <p className="settings-help">
              Currently known — torrent (qBittorrent): {categories.torrent.join(', ') || 'none'}; usenet (SABnzbd):{' '}
              {categories.usenet.join(', ') || 'none'}.
            </p>
          )}
        </section>
      )}

      {group === 'Downloads' && (
        <section className="settings-card">
          <h2>Downloads</h2>
          <p className="settings-help">
            Applies to the Manual tab only — a Managed download is already being fetched to local disk automatically,
            so there's nothing to manually download. Which mode to use (a folder, a zip, or individual files) is
            chosen each time in the download dialog itself, which remembers your last choice as next time's default.
            This section just manages the remembered destination folder for "a folder you pick" mode (Chromium-based
            browsers only).
          </p>

          {supportsDirectoryPicker() && (
            <>
              <div className="api-key-row">
                <code className="api-key-value">{defaultFolderName ?? "Not set — you'll be asked to pick one on the next download"}</code>
                <button type="button" onClick={handleChangeFolder}>
                  {defaultFolderName ? 'Change folder' : 'Choose folder'}
                </button>
                {defaultFolderName && (
                  <button type="button" onClick={handleForgetFolder}>
                    Forget
                  </button>
                )}
              </div>
              <p className="settings-help">
                Picked once, then reused silently (no prompt) for every download after, as long as this browser still
                has permission for it. Only the folder's name is shown here; browsers don't expose its full path.
                Note: the browser won't let you pick Desktop/Documents/Downloads itself (a deliberate Chrome
                restriction) — choose a subfolder inside it instead.
              </p>
              {folderStatus.kind === 'error' && <p className="settings-error">Failed to change folder: {folderStatus.message}</p>}
            </>
          )}
        </section>
      )}

      {group === 'Security' && <SecuritySettings apiKey={apiKey} />}
    </div>
  )
}
