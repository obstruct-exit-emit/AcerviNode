import { useState } from 'react'
import { ApiError, getGeneralSettings, restartServer, setTorBoxApiKey, setupInstance, testTorBoxConnection, updateGeneralSettings } from '../api'

// SetupWizard is the first-run experience: a fresh instance is claimed by
// creating a login account (no API key involved — see api.setupInstance),
// then optionally connecting TorBox right away. Much shorter than
// LibriNode's own wizard (which walks through library folders, metadata,
// indexers, and download clients) since AcerviNode's whole setup surface is
// just "one provider" — everything else already has a sensible default and
// lives in Settings afterwards.
const steps = ['Account', 'TorBox', 'HTTPS', 'Done'] as const

export default function SetupWizard({ onDone, onAlreadySetUp }: { onDone: () => void; onAlreadySetUp: () => void }) {
  const [step, setStep] = useState(0)
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  // Step 0 — account.
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPw, setConfirmPw] = useState('')

  // Step 1 — TorBox key.
  const [torboxKey, setTorboxKey] = useState('')
  const [keySaved, setKeySaved] = useState(false)

  // Step 2 — HTTPS.
  const [tlsPort, setTlsPort] = useState<number | null>(null)
  const [restarted, setRestarted] = useState(false)
  const [supervised, setSupervised] = useState(true)

  function next() {
    setNotice('')
    setStep((s) => Math.min(s + 1, steps.length - 1))
  }
  function back() {
    setNotice('')
    setStep((s) => Math.max(s - 1, 0))
  }

  function claim() {
    if (password !== confirmPw) {
      setNotice("✗ Passwords don't match")
      return
    }
    setBusy(true)
    setNotice('')
    setupInstance(username.trim(), password)
      .then(() => next())
      .catch((err: unknown) => {
        // 403 here specifically means the instance already has an account —
        // stale setupNeeded state in this tab, not a real failure. Routing
        // straight to the login form beats leaving this a dead end: step 0
        // has no Back/Skip nav of its own to escape a plain error with.
        if (err instanceof ApiError && err.status === 403) {
          onAlreadySetUp()
          return
        }
        setNotice(`✗ ${err instanceof Error ? err.message : String(err)}`)
      })
      .finally(() => setBusy(false))
  }

  function testKey() {
    setBusy(true)
    setNotice('')
    // '' — the wizard just created a session via setupInstance, which is
    // always admin; no separate API key is needed or known yet.
    setTorBoxApiKey('', torboxKey.trim())
      .then(() => testTorBoxConnection(''))
      .then((r) => setNotice(r.ok ? `✓ Connected (${r.latency_ms}ms)` : `✗ ${r.error ?? 'connection failed'}`))
      .catch((err: unknown) => setNotice(`✗ ${err instanceof Error ? err.message : String(err)}`))
      .finally(() => setBusy(false))
  }

  function saveKey() {
    setBusy(true)
    setNotice('')
    setTorBoxApiKey('', torboxKey.trim())
      .then(() => {
        setKeySaved(true)
        next()
      })
      .catch((err: unknown) => setNotice(`✗ ${err instanceof Error ? err.message : String(err)}`))
      .finally(() => setBusy(false))
  }

  // Persists tls_enabled through the same general-settings endpoint Settings
  // itself uses (round-tripping every other field unchanged, same pattern as
  // Settings.tsx's own form), then restarts immediately — enabling TLS is
  // useless until the process actually rebinds with it on.
  function enableHttps() {
    setBusy(true)
    setNotice('')
    getGeneralSettings('')
      .then((g) => {
        setTlsPort(g.tls_port)
        return updateGeneralSettings('', {
          port: g.port,
          data_dir: g.data_dir,
          download_dir: g.download_dir,
          log_level: g.log_level,
          import_interval_seconds: g.import_interval_seconds,
          import_max_retries: g.import_max_retries,
          max_concurrent_downloads: g.max_concurrent_downloads,
          import_fetch_timeout_seconds: g.import_fetch_timeout_seconds,
          cleanup_after_days: g.cleanup_after_days,
          download_dir_mode: g.download_dir_mode,
          fast_poll_interval_seconds: g.fast_poll_interval_seconds,
          provider_request_timeout_seconds: g.provider_request_timeout_seconds,
          tls_enabled: true,
          tls_port: g.tls_port,
          tls_cert_file: g.tls_cert_file,
          tls_key_file: g.tls_key_file,
          min_fetch_file_size_bytes: g.min_fetch_file_size_bytes,
          include_file_regex: g.include_file_regex,
          exclude_file_regex: g.exclude_file_regex,
          stuck_download_timeout_minutes: g.stuck_download_timeout_minutes,
          cleanup_error_after_days: g.cleanup_error_after_days,
        })
      })
      .then(() => restartServer(''))
      .then((r) => {
        setSupervised(r.supervised)
        setRestarted(true)
      })
      .catch((err: unknown) => setNotice(`✗ ${err instanceof Error ? err.message : String(err)}`))
      .finally(() => setBusy(false))
  }

  return (
    <div className="gate">
      <section className="settings-card wizard">
        <div className="wizard-dots" aria-hidden>
          {steps.map((label, i) => (
            <span key={label} className={i === step ? 'dot active' : i < step ? 'dot done' : 'dot'} title={label} />
          ))}
        </div>

        {step === 0 && (
          <>
            <h2>Welcome to AcerviNode 📦</h2>
            <p className="muted">
              Let's get you set up. First, create the account you'll sign in with — no digging
              for the API key.
            </p>
            <div className="general-form">
              <label>
                Username
                <input autoFocus value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
              </label>
              <label>
                Password (min. 8 characters)
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="new-password"
                />
              </label>
              <label>
                Confirm password
                <input
                  type="password"
                  value={confirmPw}
                  onChange={(e) => setConfirmPw(e.target.value)}
                  autoComplete="new-password"
                />
              </label>
              <div className="settings-actions">
                <button disabled={busy || !username.trim() || password.length < 8} onClick={claim}>
                  Create account &amp; continue
                </button>
                {notice && <span className={notice.startsWith('✗') ? 'notice bad' : 'notice ok'}>{notice}</span>}
              </div>
            </div>
          </>
        )}

        {step === 1 && (
          <>
            <h2>Connect TorBox</h2>
            <p className="muted">
              AcerviNode resolves everything through TorBox — paste your API key from{' '}
              <a href="https://torbox.app/settings" target="_blank" rel="noreferrer">
                torbox.app/settings
              </a>
              . You can skip this and add it later in Settings.
            </p>
            <div className="general-form">
              <label>
                TorBox API key
                <input
                  type="password"
                  placeholder="Paste your key"
                  value={torboxKey}
                  onChange={(e) => setTorboxKey(e.target.value)}
                  autoComplete="off"
                />
              </label>
              <div className="settings-actions">
                <button className="toggle" disabled={busy || !torboxKey.trim()} onClick={testKey}>
                  Test
                </button>
                <button disabled={busy || !torboxKey.trim()} onClick={saveKey}>
                  Save &amp; continue
                </button>
                {notice && <span className={notice.startsWith('✗') ? 'notice bad' : 'notice ok'}>{notice}</span>}
              </div>
            </div>
          </>
        )}

        {step === 2 && (
          <>
            <h2>Enable HTTPS?</h2>
            <p className="muted">
              Adds a second, encrypted listener alongside the existing plain-HTTP one — nothing already using
              AcerviNode over plain HTTP is affected either way. Mainly useful for the browser's folder-picker
              download mode, which needs HTTPS (or <code>localhost</code>) to work at all. Uses a self-signed
              certificate generated automatically — your browser will show a one-time "not trusted" warning to
              click through. You can always turn this on later in Settings instead.
            </p>
            {!restarted ? (
              <div className="settings-actions">
                <button disabled={busy} onClick={enableHttps}>
                  {busy ? 'Enabling…' : 'Enable HTTPS & restart'}
                </button>
                {notice && <span className={notice.startsWith('✗') ? 'notice bad' : 'notice ok'}>{notice}</span>}
              </div>
            ) : (
              <>
                {supervised ? (
                  <p className="notice ok">
                    ✓ Restarting now. Once it's back, visit{' '}
                    <strong>
                      https://{window.location.hostname}
                      {tlsPort ? `:${tlsPort}` : ''}
                    </strong>{' '}
                    instead of this http:// address to use HTTPS — this page won't move there for you.
                  </p>
                ) : (
                  <p className="notice bad">
                    ✗ Saved, but AcerviNode isn't running under a supervisor (e.g. systemd) — it just stopped, and
                    nothing will automatically start it back up. Start it again yourself, then visit{' '}
                    <strong>
                      https://{window.location.hostname}
                      {tlsPort ? `:${tlsPort}` : ''}
                    </strong>
                    .
                  </p>
                )}
                <div className="settings-actions">
                  <button onClick={next}>Continue</button>
                </div>
              </>
            )}
          </>
        )}

        {step === 3 && (
          <>
            <h2>You're all set 🎉</h2>
            <p className="muted">
              {keySaved ? 'TorBox connected.' : 'No TorBox key yet — add one anytime in Settings.'}
            </p>
            <p className="muted">
              Add a magnet, NZB, or hoster link from the Manual tab's "+ Add" button, or point
              Sonarr/Radarr at AcerviNode as a qBittorrent or SABnzbd client. Everything else lives
              in Settings.
            </p>
            <div className="settings-actions">
              <button onClick={onDone}>Enter AcerviNode</button>
            </div>
          </>
        )}

        {step > 0 && step < 3 && !(step === 2 && restarted) && (
          <div className="wizard-nav">
            <button className="toggle" disabled={busy} onClick={back}>
              ← Back
            </button>
            <button className="toggle wizard-skip" disabled={busy} onClick={next}>
              Skip for now →
            </button>
          </div>
        )}
      </section>
    </div>
  )
}
