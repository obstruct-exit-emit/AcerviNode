import { useEffect, useRef } from 'react'
import type { Download } from '../api'
import { formatBytes, formatDuration, formatRelativeTime, formatSpeed } from '../format'
import { StateBadge } from './StateBadge'

interface Props {
  downloads: Download[]
  onDelete: (d: Download) => void
  onRetry: (d: Download) => void
  onDownloadAll: (d: Download) => void
  // Bulk selection — a row's checkbox and the header's select-all checkbox.
  // Lives in the parent (App.tsx) rather than local state here, since the
  // bulk action bar rendered above the table needs the same set.
  selectedIds: Set<string>
  onToggleSelect: (id: string) => void
  onToggleSelectAll: () => void
  // Every row with a "Download all" currently in flight — a Set (not a
  // single id) because more than one row can genuinely be downloading at
  // once now that a batch can be handed off to the Downloads popup window
  // and another row started right after. Each row only ever reads its own
  // entry, so they no longer fight over one shared value.
  busyIds: Set<string>
  // Cumulative bytes written so far, per row id — only populated for the
  // streamed-to-folder path (File System Access); a row present in busyIds
  // but absent here shows the plain "…" indicator instead (zip resolution,
  // and the tab-per-file fallback, hand off with nothing to track).
  downloadProgress: Record<string, { loaded: number; total: number }>
  onSelect: (d: Download) => void
  // Retry only makes sense for a Managed (added_via=arr) download that
  // internal/importer's own fetch pipeline can act on — a Manual download is
  // never auto-fetched at all, so there's nothing for Retry to do; the row
  // just reflects the provider's own live state instead. See
  // docs/providers.md#managed-vs-manual.
  allowRetry: boolean
  // Category only means anything for a Managed download (it drives
  // save-path resolution — see docs/configuration.md#categories-and-save-paths).
  // Deliberately not offered for Manual downloads for now — see ROADMAP.md's
  // "Manual categories" entry for the reasoning and revisit trigger.
  showCategory: boolean
  emptyMessage: string
}

// States a download's files are actually resolvable in — matches what
// filesForDownload (backend) can query live from the provider.
const HAS_FILES_STATES = new Set(['provider_completed', 'ready_for_import'])

export function DownloadsTable({
  downloads,
  onDelete,
  onRetry,
  onDownloadAll,
  busyIds,
  downloadProgress,
  onSelect,
  selectedIds,
  onToggleSelect,
  onToggleSelectAll,
  allowRetry,
  showCategory,
  emptyMessage,
}: Props) {
  const selectAllRef = useRef<HTMLInputElement>(null)
  const selectedCount = downloads.filter((d) => selectedIds.has(d.id)).length
  const allSelected = downloads.length > 0 && selectedCount === downloads.length

  // React has no JSX prop for a checkbox's indeterminate state (it's a DOM
  // property, not an HTML attribute) — has to be set imperatively.
  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = selectedCount > 0 && !allSelected
    }
  }, [selectedCount, allSelected])

  if (downloads.length === 0) {
    return <p className="empty">{emptyMessage}</p>
  }

  return (
    <table className="downloads">
      <thead>
        <tr>
          <th className="select-cell">
            <input
              ref={selectAllRef}
              type="checkbox"
              checked={allSelected}
              onChange={onToggleSelectAll}
              aria-label="Select all"
            />
          </th>
          <th>Name</th>
          <th>Protocol</th>
          {showCategory && <th>Category</th>}
          <th>State</th>
          <th>Progress</th>
          <th>Size</th>
          <th>Added</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {downloads.map((d) => {
          const busy = busyIds.has(d.id)
          const progress = downloadProgress[d.id]
          return (
          <tr key={d.id} className="row-clickable" onClick={() => onSelect(d)}>
            <td className="select-cell" onClick={(e) => e.stopPropagation()}>
              <input
                type="checkbox"
                checked={selectedIds.has(d.id)}
                onChange={() => onToggleSelect(d.id)}
                aria-label={`Select ${d.name}`}
              />
            </td>
            <td className="name-cell" title={d.name}>
              {d.name}
              {d.error_message && <div className="error-message">{d.error_message}</div>}
            </td>
            <td>{d.protocol}</td>
            {showCategory && <td>{d.category || '—'}</td>}
            <td>
              <StateBadge state={d.state} addedVia={d.added_via} phase={d.phase} />
            </td>
            <td>
              <div className="progress-track">
                <div className="progress-fill" style={{ width: `${Math.round(d.progress * 100)}%` }} />
              </div>
              <span className="progress-label">
                {Math.round(d.progress * 100)}%
                {/* Only worth showing while actually moving — a completed,
                    queued, or stalled download's ETA is 0/unknown and would
                    just be noise here (the full breakdown, including why
                    it's stalled, lives in the detail view). */}
                {d.state === 'downloading' && d.eta_seconds > 0 && (
                  <span className="progress-eta"> · {formatDuration(d.eta_seconds)}</span>
                )}
              </span>
              {d.state === 'downloading' && d.download_speed_bytes > 0 && (
                <div className="progress-speed">{formatSpeed(d.download_speed_bytes)}</div>
              )}
            </td>
            <td>{formatBytes(d.size_bytes)}</td>
            <td title={d.added_at}>{formatRelativeTime(d.added_at)}</td>
            <td className="actions-cell">
              {allowRetry && d.state === 'error' && (
                <button
                  className="retry-btn-sm"
                  onClick={(e) => {
                    e.stopPropagation()
                    onRetry(d)
                  }}
                  title="Retry"
                >
                  ↻
                </button>
              )}
              {/* Manual-download actions only make sense for a Manual
                  download — a Managed one is already being auto-fetched to
                  local disk by internal/importer, so there's nothing to
                  manually grab. */}
              {d.added_via === 'manual' && HAS_FILES_STATES.has(d.state) && (
                busy && progress && progress.total > 0 ? (
                  <span
                    className="download-progress-mini"
                    title={`${formatBytes(progress.loaded)} / ${formatBytes(progress.total)}`}
                  >
                    <span
                      className="download-progress-mini-fill"
                      style={{ width: `${Math.min(100, Math.round((progress.loaded / progress.total) * 100))}%` }}
                    />
                  </span>
                ) : (
                  <button
                    className="download-all-btn-sm"
                    onClick={(e) => {
                      e.stopPropagation()
                      onDownloadAll(d)
                    }}
                    disabled={busy}
                    title="Download all files"
                  >
                    {busy ? '…' : '⬇'}
                  </button>
                )
              )}
              <button
                className="delete-btn"
                onClick={(e) => {
                  e.stopPropagation()
                  onDelete(d)
                }}
                title="Delete"
              >
                ✕
              </button>
            </td>
          </tr>
          )
        })}
      </tbody>
    </table>
  )
}
