import { useState } from 'react'
import { setTorBoxApiKey, setupInstance, testTorBoxConnection } from '../api'

// SetupWizard is the first-run experience: a fresh instance is claimed by
// creating a login account (no API key involved — see api.setupInstance),
// then optionally connecting TorBox right away. Much shorter than
// LibriNode's own wizard (which walks through library folders, metadata,
// indexers, and download clients) since AcerviNode's whole setup surface is
// just "one provider" — everything else already has a sensible default and
// lives in Settings afterwards.
const steps = ['Account', 'TorBox', 'Done'] as const

export default function SetupWizard({ onDone }: { onDone: () => void }) {
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
      .catch((err: unknown) => setNotice(`✗ ${err instanceof Error ? err.message : String(err)}`))
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
                <input autoFocus value={username} onChange={(e) => setUsername(e.target.value)} />
              </label>
              <label>
                Password (min. 8 characters)
                <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} />
              </label>
              <label>
                Confirm password
                <input type="password" value={confirmPw} onChange={(e) => setConfirmPw(e.target.value)} />
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

        {step > 0 && step < 2 && (
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
