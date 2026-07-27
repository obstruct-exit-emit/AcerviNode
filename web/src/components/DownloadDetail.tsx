import { useEffect, useState } from 'react'
import { ApiError, getDownload, type DownloadDetail as DownloadDetailData } from '../api'
import { formatBytes, formatRelativeTime } from '../format'
import { StateBadge } from './StateBadge'

const POLL_INTERVAL_MS = 4000

interface Props {
  apiKey: string
  id: string
  onClose: () => void
}

export function DownloadDetail({ apiKey, id, onClose }: Props) {
  const [detail, setDetail] = useState<DownloadDetailData | null>(null)
  const [error, setError] = useState<string | undefined>(undefined)

  useEffect(() => {
    let cancelled = false
    setDetail(null)
    setError(undefined)

    async function load() {
      try {
        const d = await getDownload(apiKey, id)
        if (!cancelled) setDetail(d)
      } catch (err) {
        if (cancelled) return
        setError(err instanceof ApiError ? err.message : err instanceof Error ? err.message : String(err))
      }
    }

    load()
    const interval = setInterval(load, POLL_INTERVAL_MS)
    return () => {
      cancelled = true
      clearInterval(interval)
    }
  }, [apiKey, id])

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  return (
    <div className="detail-overlay" onClick={onClose}>
      <div className="detail-panel" onClick={(e) => e.stopPropagation()}>
        <div className="detail-header">
          <h2 title={detail?.name}>{detail?.name ?? 'Download'}</h2>
          <button className="detail-close" onClick={onClose} title="Close">
            ✕
          </button>
        </div>

        {error && <p className="load-error">Couldn't load this download: {error}</p>}

        {detail && (
          <>
            <dl className="detail-meta">
              <div>
                <dt>State</dt>
                <dd>
                  <StateBadge state={detail.state} />
                </dd>
              </div>
              <div>
                <dt>Progress</dt>
                <dd>{Math.round(detail.progress * 100)}%</dd>
              </div>
              <div>
                <dt>Protocol</dt>
                <dd>{detail.protocol}</dd>
              </div>
              <div>
                <dt>Provider</dt>
                <dd>{detail.provider}</dd>
              </div>
              <div>
                <dt>Category</dt>
                <dd>{detail.category || '—'}</dd>
              </div>
              <div>
                <dt>Size</dt>
                <dd>{formatBytes(detail.size_bytes)}</dd>
              </div>
              <div>
                <dt>Hash</dt>
                <dd className="mono">{detail.hash || '—'}</dd>
              </div>
              <div>
                <dt>Save path</dt>
                <dd className="mono">{detail.save_path || '—'}</dd>
              </div>
              <div>
                <dt>Added</dt>
                <dd title={detail.added_at}>{formatRelativeTime(detail.added_at)}</dd>
              </div>
              <div>
                <dt>Updated</dt>
                <dd title={detail.updated_at}>{formatRelativeTime(detail.updated_at)}</dd>
              </div>
              <div>
                <dt>Completed</dt>
                <dd title={detail.completed_at ?? undefined}>
                  {detail.completed_at ? formatRelativeTime(detail.completed_at) : '—'}
                </dd>
              </div>
              {!!detail.retry_count && (
                <div>
                  <dt>Retries</dt>
                  <dd>
                    {detail.retry_count}
                    {detail.next_retry_at && (
                      <span className="text-muted"> · next {formatRelativeTime(detail.next_retry_at)}</span>
                    )}
                  </dd>
                </div>
              )}
            </dl>

            {detail.error_message && <p className="detail-error-message">{detail.error_message}</p>}

            <h3>Files</h3>
            {detail.files.length === 0 ? (
              <p className="empty">No files yet.</p>
            ) : (
              <table className="detail-files">
                <thead>
                  <tr>
                    <th>Path</th>
                    <th>Size</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.files.map((f) => (
                    <tr key={f.path}>
                      <td className="mono">{f.path}</td>
                      <td>{formatBytes(f.size_bytes)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </>
        )}
      </div>
    </div>
  )
}
