import type { Download } from '../api'
import { formatBytes, formatRelativeTime } from '../format'
import { StateBadge } from './StateBadge'

interface Props {
  downloads: Download[]
  onDelete: (d: Download) => void
  onRetry: (d: Download) => void
  onSelect: (d: Download) => void
}

export function DownloadsTable({ downloads, onDelete, onRetry, onSelect }: Props) {
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
