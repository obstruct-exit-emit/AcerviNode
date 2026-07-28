import type { Download } from '../api'
import { supportsDirectoryPicker } from '../fsAccess'
import { formatBytes, formatRelativeTime } from '../format'
import { StateBadge } from './StateBadge'

const DOWNLOAD_ALL_TITLE = supportsDirectoryPicker()
  ? 'Download all files into a folder you pick, straight from the provider'
  : 'Download all files individually, straight from the provider (opens one tab per file)'

interface Props {
  downloads: Download[]
  onDelete: (d: Download) => void
  onRetry: (d: Download) => void
  onDownloadAll: (d: Download) => void
  downloadingAllId: string | null
  onSelect: (d: Download) => void
}

// States a download's files are actually resolvable in — matches what
// filesForDownload (backend) can query live from the provider.
const HAS_FILES_STATES = new Set(['provider_completed', 'ready_for_import'])

export function DownloadsTable({ downloads, onDelete, onRetry, onDownloadAll, downloadingAllId, onSelect }: Props) {
  if (downloads.length === 0) {
    return <p className="empty">No downloads yet. Add one through Sonarr/Radarr, or via the qBittorrent/SABnzbd compat APIs directly.</p>
  }

  return (
    <table className="downloads">
      <thead>
        <tr>
          <th>Name</th>
          <th>Protocol</th>
          <th>Category</th>
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
            <td>{d.category || '—'}</td>
            <td>
              <StateBadge state={d.state} />
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
              {d.state === 'error' && (
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
              {HAS_FILES_STATES.has(d.state) && (
                <button
                  className="download-all-btn-sm"
                  onClick={(e) => {
                    e.stopPropagation()
                    onDownloadAll(d)
                  }}
                  disabled={downloadingAllId === d.id}
                  title={DOWNLOAD_ALL_TITLE}
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
