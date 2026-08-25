import { useEffect, useState } from 'react'
import type { ProviderSetting } from '../api'

interface Props {
  // The provider that just stopped being usable, and why — shown so the
  // dialog explains itself rather than appearing out of nowhere.
  vacated: string
  reason: 'reset' | 'removed'
  // Candidates to take over. Already filtered to providers that could
  // actually serve a download.
  candidates: ProviderSetting[]
  label: (name: string) => string
  onChoose: (provider: string) => void
  onKeep: () => void
}

// Asked when the default provider is reset or removed, because that is the
// one moment the intent is genuinely ambiguous and worth a question.
//
// Resetting to re-key a provider and resetting to abandon it look identical
// from here, and they want opposite handling: the first wants the default
// left alone so everything snaps back when the key returns, the second wants
// it moved. Guessing gets one of them wrong every time, so this asks instead
// — once, at the only point where the answer matters.
//
// Dismissing is a real choice, not a cancel: the default stays put, adds
// route to a configured provider meanwhile, and the provider card carries a
// notice until it's resolved. Nothing is stuck either way.
export default function ChooseDefaultDialog({ vacated, reason, candidates, label, onChoose, onKeep }: Props) {
  const [picked, setPicked] = useState(candidates[0]?.name ?? '')

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onKeep()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onKeep])

  return (
    <div className="detail-overlay" onClick={onKeep}>
      <div className="detail-panel choose-default-panel" onClick={(e) => e.stopPropagation()}>
        <div className="detail-header">
          <h2>Pick a new default</h2>
        </div>
        <p className="settings-help">
          {label(vacated)} was your default provider, and has just been {reason === 'reset' ? 'reset' : 'removed'}. New
          downloads that don't name a provider go to the default, so it's worth pointing somewhere that can take them.
        </p>

        <div className="choose-default-options">
          {candidates.map((c) => (
            <label key={c.name}>
              <input
                type="radio"
                name="new-default"
                value={c.name}
                checked={picked === c.name}
                onChange={() => setPicked(c.name)}
              />
              {label(c.name)}
            </label>
          ))}
        </div>

        <div className="provider-actions">
          <button type="button" onClick={() => onChoose(picked)} disabled={!picked}>
            Make {label(picked)} default
          </button>
          <button type="button" className="test-connection-btn" onClick={onKeep}>
            Leave it on {label(vacated)}
          </button>
        </div>
        <p className="settings-help">
          Leaving it is fine: adds fall through to a provider that has credentials in the meantime, and everything
          returns to {label(vacated)} the moment it's set up again.
        </p>
      </div>
    </div>
  )
}
