import { PHASE_LABELS, STATE_LABELS } from '../format'

// addedVia matters for exactly one state: "provider_completed" means very
// different things depending on it. For a Managed (arr) download it means
// "internal/importer is about to/currently fetching this to local disk" —
// STATE_LABELS' default "Fetching" is accurate there. For a Manual download
// it's never auto-fetched at all (see docs/providers.md#managed-vs-manual),
// so "Fetching" would be an outright lie — it just means the provider's
// done and the file is available to grab on demand.
//
// phase is deliberately NOT its own top-level state (see
// debrid.DownloadStatus.Phase's own doc comment) — TorBox's raw
// "processing"/"verifying"/"repairing"/"extracting" are all still "not done
// yet" from AcerviNode's own state machine's point of view (the same bucket
// everything else — retry logic, discoverManual, the refresh no-op check —
// already keys off), so widening state itself would ripple far for what's
// really just a more specific label. Showing phase here when present is a
// pure display choice on top of that: still the same badge-downloading
// color/bucket underneath, just a more accurate word than a generic
// "Downloading" for however long TorBox is actually still processing it.
export function StateBadge({ state, addedVia, phase }: { state: string; addedVia?: 'arr' | 'manual'; phase?: string }) {
  const label =
    addedVia === 'manual' && state === 'provider_completed'
      ? 'Available'
      : state === 'downloading' && phase && PHASE_LABELS[phase]
        ? PHASE_LABELS[phase]
        : (STATE_LABELS[state] ?? state)
  return <span className={`badge badge-${state}`}>{label}</span>
}
