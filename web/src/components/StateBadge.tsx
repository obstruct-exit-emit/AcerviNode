import { STATE_LABELS } from '../format'

// addedVia matters for exactly one state: "provider_completed" means very
// different things depending on it. For a Managed (arr) download it means
// "internal/importer is about to/currently fetching this to local disk" —
// STATE_LABELS' default "Fetching" is accurate there. For a Manual download
// it's never auto-fetched at all (see docs/providers.md#managed-vs-manual),
// so "Fetching" would be an outright lie — it just means the provider's
// done and the file is available to grab on demand.
export function StateBadge({ state, addedVia }: { state: string; addedVia?: 'arr' | 'manual' }) {
  const label = addedVia === 'manual' && state === 'provider_completed' ? 'Available' : (STATE_LABELS[state] ?? state)
  return <span className={`badge badge-${state}`}>{label}</span>
}
