import type { Download } from '../api'
import { supportsDirectoryPicker } from '../fsAccess'
import { formatBytes, formatRelativeTime } from '../format'
import { getDownloadMode } from '../preferences'
import { StateBadge } from './StateBadge'

// Computed fresh (not module-scope) so it reflects a preference change made
// in Settings' Downloads section without needing a page reload — Downloads
// and Settings are separate views, but DownloadsTable remounts every time
// its view becomes active again.
function downloadAllTitle(): string {
  if (getDownloadMode() === 'zip') {
    return 'Download all files as one provider-zipped archive'
  }
  return supportsDirectoryPicker()
    ? 'Download all files into a folder you pick, straight from the provider'
    : 'Download all files individually, straight from the provider (opens one tab per file)'
}

interface Props {
  downloads: Download[]
  onDelete: (d: Download) => void
  onRetry: (d: Download) => void
  onDownloadAll: (d: Download) => void
  downloadingAllId: string | null
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

export function DownloadsTable({ downloads, onDelete, onRetry, onDownloadAll, downloadingAllId, onSelect, allowRetry, showCategory, emptyMessage }: Props) {
  if (downloads.length === 0) {
    return <p className="empty">{emptyMessage}</p>
  }

  return (
    <table className="downloads">
      <thead>
        <tr>
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
        {downloads.map((d) => (
          <tr key={d.id} className="row-clickable" onClick={() => onSelect(d)}>
            <td className="name-cell" title={d.name}>
              {d.name}
              {d.error_message && <div className="error-message">{d.error_message}</div>}
            </td>
            <td>{d.protocol}</td>
            {showCategory && <td>{d.category || '—'}</td>}
            <td>
              <StateBadge state={d.state} addedVia={d.added_via} />
            </td>
            <td>
              <div className="progress-track">
                <div className="progress-fill" style={{ width: `${Math.round(d.progress * 100)}%` }} />
              </div>
              <span className="progress-label">{Math.round(d.progress * 100)}%</span>
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
                <button
                  className="download-all-btn-sm"
                  onClick={(e) => {
                    e.stopPropagation()
                    onDownloadAll(d)
                  }}
                  disabled={downloadingAllId === d.id}
                  title={downloadAllTitle()}
                >
                  {downloadingAllId === d.id ? '…' : '⬇'}
                </button>
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
        ))}
      </tbody>
    </table>
  )
}
