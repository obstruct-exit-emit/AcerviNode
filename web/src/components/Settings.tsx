import { Fragment, useEffect, useState, type FormEvent, type ReactNode } from 'react'
import {
  getCategories,
  getGeneralSettings,
  getProviderSettings,
  getStatus,
  getProviderAccount,
  regenerateApiKey,
  regenerateCertificate,
  removeCategory,
  restartServer,
  setCategoryPath,
  setProviderApiKey,
  setDefaultProvider,
  testProviderConnection,
  updateGeneralSettings,
  ApiError,
  type Categories,
  type GeneralSettings,
  type GeneralUpdateInput,
  type ProviderSetting,
  type StatusInfo,
  type ProviderAccount,
} from '../api'
import { getDefaultDirectory, pickAndRememberDirectory, forgetDefaultDirectory, supportsDirectoryPicker } from '../fsAccess'
import { formatBytes, formatRelativeTime } from '../format'
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
// Reorganized (2026-08-04, requested directly) — General's "Import &
// cleanup" section had grown to 10 fields across 5 genuinely unrelated
// concerns (retry/concurrency, fetch timeout, two different retention
// policies, a stuck-download watchdog, file filtering), each new feature
// this session having nowhere better to land than that one catch-all.
// Split into three focused groups (Import/Filtering/Cleanup) instead —
// General keeps only what it always had otherwise: API key, instance
// identity, HTTPS. Provider's own "Polling & timeout" section (import
// interval, fast poll interval, provider request timeout) deliberately
// stays put rather than merging into Import — those are specifically about
// how often/how patiently AcerviNode talks to *this* provider, which would
// need to be per-provider if Phase 4 (multi-provider) ever unblocks, unlike
// Import's retry/concurrency/fetch-timeout fields, which apply to the
// fetch-to-disk pipeline regardless of which provider a download came from.
const settingsGroups = [
  { name: 'General', blurb: "This instance's API key, network, and logging." },
  { name: 'Provider', blurb: 'The TorBox account AcerviNode resolves every download through, and how often it polls it.' },
  { name: 'Import', blurb: 'How a completed download gets fetched to local disk: retries, concurrency, and timeouts.' },
  { name: 'Filtering', blurb: 'Skip specific files, by size or name pattern, when fetching a download to local disk.' },
  { name: 'Cleanup', blurb: 'Automatically remove a finished, errored, or stuck download after a while.' },
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

// allCategoryNames unions every category name AcerviNode knows about from
// any source — paths (has a save-path override, possibly empty), torrent
// (known to the qBittorrent shim), usenet (known to the SABnzbd shim) — so
// a category that's only ever been reactively declared by a real Sonarr/
// Radarr (never given an override) still gets a full row, same as a
// pre-seeded default or a manually registered one. Sorted, case-sensitive
// (Readarr's "Readarr" vs. "readarr" are genuinely distinct — see
// cmd/acervinode/default_categories.go).
function allCategoryNames(categories: Categories): string[] {
  const names = new Set<string>([...Object.keys(categories.paths), ...categories.torrent, ...categories.usenet])
  return [...names].sort((a, b) => a.localeCompare(b))
}

// One row of the Categories list — every known category name, however it
// came to be known (a pre-seeded *arr default, a real Sonarr/Radarr
// declaring one, or a manual registration), rendered identically: an
// editable path override plus a Delete action. Kept as its own component,
// keyed by category name, so an in-progress edit in one row survives a
// `load()` triggered by saving or deleting a different row (see Settings'
// onSaved).
function CategoryPathRow({
  name,
  currentPath,
  apiKey,
  onSaved,
}: {
  name: string
  currentPath: string
  apiKey: string
  onSaved: () => void
}) {
  const [path, setPath] = useState(currentPath)
  const [savedPath, setSavedPath] = useState(currentPath)
  const [status, setStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'deleting' | 'error'; message?: string }>({ kind: 'idle' })

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

  async function handleDelete() {
    if (!confirm(`Remove category "${name}"? If Sonarr/Radarr is still configured with it, it'll simply reappear next time it's used.`)) return
    setStatus({ kind: 'deleting' })
    try {
      await removeCategory(apiKey, name)
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
      <button type="button" onClick={handleSave} disabled={status.kind === 'saving' || status.kind === 'deleting' || path.trim() === savedPath}>
        {status.kind === 'saving' ? 'Saving…' : 'Save'}
      </button>
      <button type="button" className="category-delete-btn" onClick={handleDelete} disabled={status.kind === 'deleting'} title="Remove category">
        {status.kind === 'deleting' ? '…' : '✕'}
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
  // Collapsed by default — the configured flag and plan (shown in the
  // header row regardless of this) are usually all there is to check; the
  // key form/account detail/status/polling knobs underneath are needed far
  // less often.
  // Expanded per provider, not one flag for the whole tab: each provider is
  // its own card, the same shape the single TorBox card always had.
  const [expandedProviders, setExpandedProviders] = useState<Record<string, boolean>>({})
  const [settings, setSettings] = useState<ProviderSetting[] | null>(null)
  // Keyed by provider name: with more than one configured, each card needs
  // its own draft key, save state and test result, or typing into one would
  // show progress on all of them.
  const [providerKeys, setProviderKeys] = useState<Record<string, string>>({})
  const [status, setStatus] = useState<Record<string, { kind: 'idle' | 'saving' | 'saved' | 'error'; message?: string }>>({})
  const [general, setGeneral] = useState<GeneralSettings | null>(null)
  const [form, setForm] = useState<GeneralUpdateInput | null>(null)
  const [keyRevealed, setKeyRevealed] = useState(false)
  const [copyStatus, setCopyStatus] = useState<'idle' | 'copied'>('idle')
  const [regenStatus, setRegenStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })
  const [generalStatus, setGeneralStatus] = useState<{ kind: 'idle' | 'saving' | 'saved' | 'restart' | 'error'; message?: string }>({
    kind: 'idle',
  })
  const [testStatus, setTestStatus] = useState<
    Record<string, { kind: 'idle' | 'testing' | 'ok' | 'error'; message?: string; latencyMs?: number }>
  >({})
  const [defaultStatus, setDefaultStatus] = useState<{ kind: 'idle' | 'saving' | 'error'; message?: string }>({ kind: 'idle' })
  // Keyed by provider: each account is its own live call and its own
  // panel, inside that provider's card.
  const [accounts, setAccounts] = useState<Record<string, ProviderAccount>>({})
  // Whether the (separately-fetched, potentially slow — see load()) account
  // status call is still in flight, so the Provider tab can say so instead
  // of just silently showing nothing until it resolves.
  const [accountLoading, setAccountLoading] = useState(false)
  // internal/importer's own health signals (tick liveness, per-kind
  // rate-limit cooldowns, per-kind error counts) — a fast, purely local
  // call, part of the main load() below rather than fetched separately the
  // way account status is.
  const [health, setHealth] = useState<StatusInfo | null>(null)
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
    // Captured from the settled response rather than read back from state:
    // setSettings won't have applied by the time the account fetch below
    // runs, and asking for the accounts of providers we haven't loaded yet
    // would fetch nothing.
    let providerList: ProviderSetting[] = []
    try {
      const [providerSettings, generalSettings, cats, statusInfo] = await Promise.all([
        getProviderSettings(apiKey),
        getGeneralSettings(apiKey),
        getCategories(apiKey),
        // Fast and purely local (no live provider network call — see
        // getStatus's own doc comment), unlike getProviderAccount below, so
        // it's safe to bundle here with everything else.
        getStatus(apiKey),
      ])
      setSettings(providerSettings)
      providerList = providerSettings
      setGeneral(generalSettings)
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
        provider_request_timeout_seconds: generalSettings.provider_request_timeout_seconds,
        tls_enabled: generalSettings.tls_enabled,
        tls_port: generalSettings.tls_port,
        // Round-tripped unchanged, same as data_dir — neither has an
        // editable field in this form (config/env-only, see GeneralSettings).
        tls_cert_file: generalSettings.tls_cert_file,
        tls_key_file: generalSettings.tls_key_file,
        min_fetch_file_size_bytes: generalSettings.min_fetch_file_size_bytes,
        max_fetch_file_size_bytes: generalSettings.max_fetch_file_size_bytes,
        include_file_regex: generalSettings.include_file_regex,
        exclude_file_regex: generalSettings.exclude_file_regex,
        stuck_download_timeout_minutes: generalSettings.stuck_download_timeout_minutes,
        cleanup_error_after_days: generalSettings.cleanup_error_after_days,
      })
      setCategories(cats)
      setHealth(statusInfo)
    } catch {
      // The dashboard's own polling will surface auth/connectivity errors;
      // this view just leaves the form usable either way.
    }

    // Deliberately NOT part of the Promise.all above — each of these is a
    // live call to the provider itself (see getProviderAccount's own doc
    // comment), which can take up to the provider client's own timeout
    // (seen live taking a real 30s when TorBox itself was degraded) before
    // resolving, even though it's designed to resolve with available:false
    // rather than throw. Bundling them with the rest of load() meant the
    // *entire* Settings page — API key, Import & cleanup, Categories, none
    // of which have anything to do with a provider's health — sat blank
    // that whole time. Fetched independently so the rest of the page is
    // usable immediately regardless of how slow or broken any provider is.
    //
    // Settled together rather than sequentially so one unreachable provider
    // doesn't delay another's panel.
    setAccountLoading(true)
    try {
      const results = await Promise.allSettled(
        providerList.map(async (p) => [p.name, await getProviderAccount(apiKey, p.name)] as const),
      )
      const next: Record<string, ProviderAccount> = {}
      for (const r of results) {
        if (r.status === 'fulfilled') next[r.value[0]] = r.value[1]
      }
      setAccounts(next)
    } catch {
      // Same treatment as the block above — routine, not fatal.
    } finally {
      setAccountLoading(false)
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

  async function handleProviderSubmit(e: FormEvent, provider: string) {
    e.preventDefault()
    const key = (providerKeys[provider] ?? '').trim()
    if (!key) return
    setStatus((s) => ({ ...s, [provider]: { kind: 'saving' } }))
    try {
      await setProviderApiKey(apiKey, provider, key)
      setProviderKeys((k) => ({ ...k, [provider]: '' }))
      setStatus((s) => ({ ...s, [provider]: { kind: 'saved' } }))
      setTestStatus((s) => ({ ...s, [provider]: { kind: 'idle' } }))
      await load()
    } catch (err) {
      setStatus((s) => ({ ...s, [provider]: { kind: 'error', message: err instanceof ApiError ? err.message : String(err) } }))
    }
  }

  // Clearing a key switches a provider off without editing config.yaml. It
  // stays listed, so it can be set up again later.
  async function handleClearProviderKey(provider: string) {
    setStatus((s) => ({ ...s, [provider]: { kind: 'saving' } }))
    try {
      await setProviderApiKey(apiKey, provider, '')
      setStatus((s) => ({ ...s, [provider]: { kind: 'saved' } }))
      setTestStatus((s) => ({ ...s, [provider]: { kind: 'idle' } }))
      await load()
    } catch (err) {
      setStatus((s) => ({ ...s, [provider]: { kind: 'error', message: err instanceof ApiError ? err.message : String(err) } }))
    }
  }

  async function handleTestConnection(provider: string) {
    setTestStatus((s) => ({ ...s, [provider]: { kind: 'testing' } }))
    try {
      const result = await testProviderConnection(apiKey, provider)
      setTestStatus((s) => ({
        ...s,
        [provider]: result.ok
          ? { kind: 'ok', latencyMs: result.latency_ms }
          : { kind: 'error', message: result.error },
      }))
    } catch (err) {
      setTestStatus((s) => ({ ...s, [provider]: { kind: 'error', message: err instanceof ApiError ? err.message : String(err) } }))
    }
  }

  async function handleMakeDefault(provider: string) {
    setDefaultStatus({ kind: 'saving' })
    try {
      await setDefaultProvider(apiKey, provider)
      setDefaultStatus({ kind: 'idle' })
      await load()
    } catch (err) {
      setDefaultStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
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

  // providerLabel prettifies the entry name for display only — the name
  // itself is what routes a download, and a second account on the same
  // service is a distinct entry, so anything unrecognised is shown as-is
  // rather than guessed at.
  const providerLabel = (name: string) =>
    ({ torbox: 'TorBox', alldebrid: 'AllDebrid' } as Record<string, string>)[name] ?? name

  const providers = settings ?? []
  // Only worth offering a default when there is actually a choice to make.
  const showDefaultControls = providers.length > 1
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
        <>
          {providers.map((p) => {
            const saveState = status[p.name] ?? { kind: 'idle' as const }
            const test = testStatus[p.name] ?? { kind: 'idle' as const }
            const draft = providerKeys[p.name] ?? ''
            const account = accounts[p.name]
            const expanded = expandedProviders[p.name] ?? false
            const toggle = () => setExpandedProviders((e) => ({ ...e, [p.name]: !expanded }))
            return (
              <section className="settings-card" key={p.name}>
                <div
                  className="settings-card-toggle"
                  onClick={toggle}
                  role="button"
                  tabIndex={0}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault()
                      toggle()
                    }
                  }}
                >
                  <h2>{providerLabel(p.name)}</h2>
                  <span className="settings-card-summary">
                    {p.default && showDefaultControls && <span className="badge badge-ready_for_import">Default</span>}
                    {p.configured ? (
                      <span className="badge badge-ready_for_import">Configured</span>
                    ) : (
                      <span className="badge badge-queued">Not configured</span>
                    )}
                  </span>
                  <span className={`settings-card-chevron${expanded ? ' settings-card-chevron-open' : ''}`}>▸</span>
                </div>
                {expanded && (
                  <>
                    <p className="settings-help">
                      {p.configured
                        ? 'Enter a new key below to replace the current one — takes effect immediately, no restart needed.'
                        : `Add your ${p.name} API key to enable the qBittorrent and SABnzbd compat shims.`}
                    </p>
                    <form onSubmit={(e) => handleProviderSubmit(e, p.name)}>
                      <input
                        type="password"
                        placeholder={`${p.name} API key`}
                        value={draft}
                        onChange={(e) => setProviderKeys((k) => ({ ...k, [p.name]: e.target.value }))}
                      />
                      <button type="submit" disabled={saveState.kind === 'saving' || !draft.trim()}>
                        {saveState.kind === 'saving' ? 'Saving…' : 'Save'}
                      </button>
                    </form>
                    {saveState.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
                    {saveState.kind === 'error' && <p className="settings-error">Failed to save: {saveState.message}</p>}

                    {p.configured && (
                      <>
                        <button
                          type="button"
                          className="test-connection-btn"
                          onClick={() => handleTestConnection(p.name)}
                          disabled={test.kind === 'testing'}
                        >
                          {test.kind === 'testing' ? 'Testing…' : 'Test connection'}
                        </button>
                        {showDefaultControls && !p.default && (
                          <button
                            type="button"
                            className="test-connection-btn"
                            onClick={() => handleMakeDefault(p.name)}
                            disabled={defaultStatus.kind === 'saving'}
                          >
                            Make default
                          </button>
                        )}
                        <button type="button" className="test-connection-btn" onClick={() => handleClearProviderKey(p.name)}>
                          Clear key
                        </button>
                        {test.kind === 'ok' && <p className="settings-success">Connected — {test.latencyMs}ms</p>}
                        {test.kind === 'error' && <p className="settings-error">Connection failed: {test.message}</p>}
                      </>
                    )}

                    {/* This provider's own account — its plan, its expiry,
                        its restrictions. Inside its card because none of it
                        generalises: two providers have two different plans,
                        and a cooldown belongs to whichever account applied
                        it. */}
                    {account?.available && account.cooldown_until && new Date(account.cooldown_until).getTime() > Date.now() && (
                      <p className="settings-error">
                        ⚠ {providerLabel(p.name)} is restricting this account until{' '}
                        {new Date(account.cooldown_until).toLocaleString()} — every download's progress can look
                        frozen with no other visible cause while this is active. Not something AcerviNode can do
                        anything about; it'll clear on its own.
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
                    {/* Only while a fetch is actually in flight: account
                        doesn't distinguish "never fetched" from "fetched,
                        unavailable". */}
                    {p.configured && !account && accountLoading && (
                      <p className="settings-help">Checking account status…</p>
                    )}
                  </>
                )}
              </section>
            )
          })}

          {defaultStatus.kind === 'error' && (
            <p className="settings-error">Failed to set default: {defaultStatus.message}</p>
          )}
          {showDefaultControls && (
            <p className="settings-help">
              New downloads go to the default provider unless the “+ Add” form names another. Both compat shims
              always use the default — neither the qBittorrent nor the SABnzbd protocol has a field to carry a
              provider.
            </p>
          )}

        <section className="settings-card">

          {health && (
            <Section
              title="Status"
              help="Whether AcerviNode's own background polling is actually alive and making progress — distinct from the account panel above, which is about the provider's own account state."
            >
              <dl className="detail-meta">
                <dt>Background polling</dt>
                <dd>{health.last_tick_at ? `last ran ${formatRelativeTime(health.last_tick_at)}` : 'never run yet'}</dd>
                {(['torrent', 'usenet', 'webdl'] as const).map((kind) => {
                  const k = health.kinds[kind]
                  if (!k) return null
                  const rateLimited = k.rate_limited_until && new Date(k.rate_limited_until).getTime() > Date.now()
                  return (
                    <Fragment key={kind}>
                      <dt>{kind}</dt>
                      <dd>
                        {k.last_successful_list_at ? `last synced ${formatRelativeTime(k.last_successful_list_at)}` : 'never synced yet'}
                        {rateLimited && <> · rate-limited until {new Date(k.rate_limited_until as string).toLocaleTimeString()}</>}
                        {k.error_count > 0 && <> · {k.error_count} in error</>}
                      </dd>
                    </Fragment>
                  )
                })}
              </dl>
            </Section>
          )}

          {form && (
            <Section title="Polling & timeout" help="How often, and how patiently, AcerviNode talks to the provider itself. Applies immediately, no restart needed.">
              <form className="general-form" onSubmit={handleGeneralSubmit}>
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
                  Request timeout (seconds)
                  <input
                    type="number"
                    min={1}
                    value={form.provider_request_timeout_seconds}
                    onChange={(e) => setForm({ ...form, provider_request_timeout_seconds: Number(e.target.value) })}
                  />
                </label>
                <button type="submit" disabled={generalStatus.kind === 'saving'}>
                  {generalStatus.kind === 'saving' ? 'Saving…' : 'Save'}
                </button>
              </form>
              <p className="settings-help">
                Import interval is the bulk poll that syncs every tracked download's status from the provider. Fast
                poll interval checks each actively downloading Managed download individually, so a finished download
                is noticed within a few seconds instead of waiting for the next full import interval — separate
                from (and much cheaper than) the import interval, since it only ever checks downloads already known
                to be in progress. The default (3s) was tuned against a real provider to stay responsive without
                risking a rate limit; raise it if you routinely have many downloads active at once. Request timeout
                bounds a single call to the provider's own API (unlike the fetch idle timeout under Import &amp;
                cleanup, this one covers small API calls, not file downloads, so it's a plain total deadline) —
                lower it to fail faster during a provider outage, raise it if you're seeing timeouts on a slow
                connection to the provider.
              </p>
              {generalStatus.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
              {generalStatus.kind === 'error' && <p className="settings-error">Failed to save: {generalStatus.message}</p>}
            </Section>
          )}
        </section>
        </>
      )}

      {group === 'Import' && (
        <section className="settings-card">
          <h2>Import</h2>
          <p className="settings-help">
            Import max retries is how many times a failed fetch-to-disk attempt is retried before a Managed
            download is given up on and moved to "error". Max concurrent downloads bounds how many
            provider_completed downloads are fetched to disk at once. The fetch idle timeout only fires after
            this many seconds pass with zero bytes received — a large file on a slow connection that's still
            steadily, actively transferring is never affected by this however long the whole download takes;
            only a connection that's actually gone quiet (stuck connecting, or stalled mid-transfer) trips it.
            See the Provider tab for how often AcerviNode polls the debrid provider itself — a separate
            concern from how the fetch-to-disk pipeline itself behaves once a download's actually being
            fetched.
          </p>
          {form && (
            <form className="settings-form-stack" onSubmit={handleGeneralSubmit}>
              <div className="general-form">
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
                  Import fetch idle timeout (seconds)
                  <input
                    type="number"
                    min={1}
                    value={form.import_fetch_timeout_seconds}
                    onChange={(e) => setForm({ ...form, import_fetch_timeout_seconds: Number(e.target.value) })}
                  />
                </label>
                <button type="submit" disabled={generalStatus.kind === 'saving'}>
                  {generalStatus.kind === 'saving' ? 'Saving…' : 'Save'}
                </button>
              </div>
              {generalStatus.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
              {generalStatus.kind === 'error' && <p className="settings-error">Failed to save: {generalStatus.message}</p>}
            </form>
          )}
        </section>
      )}

      {group === 'Filtering' && (
        <section className="settings-card">
          <h2>Filtering</h2>
          <p className="settings-help">
            Minimum/maximum file size and the include/exclude patterns (matched against each file's path
            within the download, e.g. "Show/episode.en.srt") apply when fetching a download's files to local
            disk — skip samples/junk, an oversized bonus file, or only fetch certain file types. Any
            combination can be set at once; a file has to satisfy all of them to be kept. Purely local — never
            changes what the provider itself considers part of the download, only which of its files actually
            get written to disk.
          </p>
          {form && (
            <form className="settings-form-stack" onSubmit={handleGeneralSubmit}>
              <div className="general-form">
                <label>
                  Minimum file size to fetch (bytes, 0 = off)
                  <input
                    type="number"
                    min={0}
                    value={form.min_fetch_file_size_bytes}
                    onChange={(e) => setForm({ ...form, min_fetch_file_size_bytes: Number(e.target.value) })}
                  />
                </label>
                <label>
                  Maximum file size to fetch (bytes, 0 = off)
                  <input
                    type="number"
                    min={0}
                    value={form.max_fetch_file_size_bytes}
                    onChange={(e) => setForm({ ...form, max_fetch_file_size_bytes: Number(e.target.value) })}
                  />
                </label>
                <label>
                  Include files matching (regex, blank = off)
                  <input
                    type="text"
                    placeholder={String.raw`\.(mkv|mp4)$`}
                    value={form.include_file_regex}
                    onChange={(e) => setForm({ ...form, include_file_regex: e.target.value })}
                  />
                </label>
                <label>
                  Exclude files matching (regex, blank = off)
                  <input
                    type="text"
                    placeholder="sample"
                    value={form.exclude_file_regex}
                    onChange={(e) => setForm({ ...form, exclude_file_regex: e.target.value })}
                  />
                </label>
                <button type="submit" disabled={generalStatus.kind === 'saving'}>
                  {generalStatus.kind === 'saving' ? 'Saving…' : 'Save'}
                </button>
              </div>
              {generalStatus.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
              {generalStatus.kind === 'error' && <p className="settings-error">Failed to save: {generalStatus.message}</p>}
            </form>
          )}
        </section>
      )}

      {group === 'Cleanup' && (
        <section className="settings-card">
          <h2>Cleanup</h2>
          <p className="settings-help">
            Cleanup only ever touches a Managed download once it's reached "ready for import" (already handed
            off to Sonarr/Radarr) and stayed there this long — a Manual download is never auto-deleted this
            way. Cleanup for errored downloads is separate, and applies to both Managed and Manual downloads —
            an error already means AcerviNode gave up (retry-exhausted) or the provider itself lost track of
            it, not an in-progress state worth preserving. The stuck download timeout auto-errors a download
            that's sat queued/downloading with no genuine change reported by the provider for this long —
            keyed on whether anything actually changed, not simply how long it's been running, so a large
            download still steadily making progress on a slow connection is never affected however long the
            whole thing takes. 0 disables each of these independently (the default).
          </p>
          {form && (
            <form className="settings-form-stack" onSubmit={handleGeneralSubmit}>
              <div className="general-form">
                <label>
                  Clean up Managed downloads after (days, 0 = off)
                  <input
                    type="number"
                    min={0}
                    value={form.cleanup_after_days}
                    onChange={(e) => setForm({ ...form, cleanup_after_days: Number(e.target.value) })}
                  />
                </label>
                <label>
                  Clean up errored downloads after (days, 0 = off)
                  <input
                    type="number"
                    min={0}
                    value={form.cleanup_error_after_days}
                    onChange={(e) => setForm({ ...form, cleanup_error_after_days: Number(e.target.value) })}
                  />
                </label>
                <label>
                  Stuck download timeout (minutes, 0 = off)
                  <input
                    type="number"
                    min={0}
                    value={form.stuck_download_timeout_minutes}
                    onChange={(e) => setForm({ ...form, stuck_download_timeout_minutes: Number(e.target.value) })}
                  />
                </label>
                <button type="submit" disabled={generalStatus.kind === 'saving'}>
                  {generalStatus.kind === 'saving' ? 'Saving…' : 'Save'}
                </button>
              </div>
              {generalStatus.kind === 'saved' && <p className="settings-success">Saved — applied immediately.</p>}
              {generalStatus.kind === 'error' && <p className="settings-error">Failed to save: {generalStatus.message}</p>}
            </form>
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
            "music"/"lidarr", Readarr's "Readarr"/"readarr") is already registered below, editable and deletable the
            same as anything you add yourself — this form is only for a custom name you've typed in instead. Leave
            the path blank to just register the name; fill it in to also redirect that category's completed
            downloads to a specific directory instead of the default <code>download_dir/&lt;category&gt;</code>{' '}
            (e.g. to route it to a different disk or mount). Only affects Managed (Sonarr/Radarr) downloads —
            category has no effect on Manual ones. Deleting a category (✕) removes it entirely; if Sonarr/Radarr is
            still actively configured with it, it'll simply reappear the next time it's used, same as it would
            against a real qBittorrent/SABnzbd install.
          </p>

          {categories &&
            allCategoryNames(categories).map((name) => (
              <CategoryPathRow key={name} name={name} currentPath={categories.paths[name] ?? ''} apiKey={apiKey} onSaved={load} />
            ))}

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
