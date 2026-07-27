import { STATE_LABELS } from '../format'

export function StateBadge({ state }: { state: string }) {
  return <span className={`badge badge-${state}`}>{STATE_LABELS[state] ?? state}</span>
}
