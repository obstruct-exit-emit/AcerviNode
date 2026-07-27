import { useState, type FormEvent } from 'react'

interface Props {
  onSubmit: (key: string) => void
  error?: string
}

export function ApiKeyGate({ onSubmit, error }: Props) {
  const [value, setValue] = useState('')

  function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (value.trim()) onSubmit(value.trim())
  }

  return (
    <div className="gate">
      <form className="gate-card" onSubmit={handleSubmit}>
        <h1>📦 AcerviNode</h1>
        <p>Enter the <code>api_key</code> from your <code>config.yaml</code>.</p>
        <input
          type="password"
          autoFocus
          placeholder="API key"
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
        {error && <p className="gate-error">{error}</p>}
        <button type="submit">Continue</button>
      </form>
    </div>
  )
}
