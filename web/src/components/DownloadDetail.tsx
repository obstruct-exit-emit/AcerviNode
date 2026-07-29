import { useEffect, useState } from 'react'
import {
  ApiError,
  getDownload,
  getFileLink,
  getZipLink,
  reAddDownload,
  retryDownload,
  type DownloadDetail as DownloadDetailData,
} from '../api'
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
  const [retryStatus, setRetryStatus] = useState<{ kind: 'idle' | 'retrying' | 'error'; message?: string }>({ kind: 'idle' })
  const [readdStatus, setReaddStatus] = useState<{ kind: 'idle' | 'readding' | 'error'; message?: string }>({ kind: 'idle' })
  const [resolvingPath, setResolvingPath] = useState<string | null>(null)
  const [zipStatus, setZipStatus] = useState<{ kind: 'idle' | 'resolving' | 'error'; message?: string }>({ kind: 'idle' })

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

  async function handleRetry() {
    setRetryStatus({ kind: 'retrying' })
    try {
      const updated = await retryDownload(apiKey, id)
      setDetail((d) => (d ? { ...d, ...updated } : d))
      setRetryStatus({ kind: 'idle' })
    } catch (err) {
      setRetryStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  async function handleReAdd() {
    if (!confirm('Re-add this download as brand new? Use this when Retry alone doesn\'t help — e.g. the original torrent/NZB has expired on the provider\'s side.')) {
      return
    }
    setReaddStatus({ kind: 'readding' })
    try {
      const updated = await reAddDownload(apiKey, id)
      setDetail((d) => (d ? { ...d, ...updated } : d))
      setReaddStatus({ kind: 'idle' })
    } catch (err) {
      setReaddStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  async function handleDownloadFile(f: { path: string; provider_file_id?: string }) {
    if (!f.provider_file_id) return
    setResolvingPath(f.path)
    try {
      const { url } = await getFileLink(apiKey, id, f.provider_file_id)
      // A resolved link is the provider's own CDN URL, not one of ours — no
      // Authorization header needed (or sendable) for this second
      // navigation, unlike every other call in this app.
      window.open(url, '_blank', 'noopener,noreferrer')
    } catch (err) {
      alert(err instanceof ApiError ? err.message : String(err))
    } finally {
      setResolvingPath(null)
    }
  }

  // Opt-in alternative to downloading files individually — one archive
  // instead of one browser download per file.
  async function handleDownloadZip() {
    setZipStatus({ kind: 'resolving' })
    try {
      const { url } = await getZipLink(apiKey, id)
      window.open(url, '_blank', 'noopener,noreferrer')
      setZipStatus({ kind: 'idle' })
    } catch (err) {
      setZipStatus({ kind: 'error', message: err instanceof ApiError ? err.message : String(err) })
    }
  }

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

            {/* Retry/Re-add only make sense for a Managed download —
                internal/importer never auto-fetches a Manual one at all, so
                there's nothing for either action to do; the row just
                reflects the provider's own live state instead. */}
            {detail.state === 'error' && detail.added_via === 'arr' && (
              <div className="detail-retry">
                <button type="button" className="retry-btn" onClick={handleRetry} disabled={retryStatus.kind === 'retrying'}>
                  {retryStatus.kind === 'retrying' ? 'Retrying…' : 'Retry'}
                </button>
                <button type="button" className="readd-btn" onClick={handleReAdd} disabled={readdStatus.kind === 'readding'}>
                  {readdStatus.kind === 'readding' ? 'Re-adding…' : 'Re-add'}
                </button>
                {retryStatus.kind === 'error' && <p className="settings-error">Failed to retry: {retryStatus.message}</p>}
                {readdStatus.kind === 'error' && <p className="settings-error">Failed to re-add: {readdStatus.message}</p>}
              </div>
            )}

            <div className="files-header">
              <h3>Files</h3>
              {detail.files.length > 0 && (
                <button type="button" className="zip-btn" onClick={handleDownloadZip} disabled={zipStatus.kind === 'resolving'}>
                  {zipStatus.kind === 'resolving' ? 'Resolving…' : 'Download all (zip)'}
                </button>
              )}
            </div>
            {zipStatus.kind === 'error' && <p className="settings-error">Failed to resolve zip: {zipStatus.message}</p>}
            {detail.files.length === 0 ? (
              <p className="empty">No files yet.</p>
            ) : (
              <table className="detail-files">
                <thead>
                  <tr>
                    <th>Path</th>
                    <th>Size</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {detail.files.map((f) => (
                    <tr key={f.path}>
                      <td className="mono">{f.path}</td>
                      <td>{formatBytes(f.size_bytes)}</td>
                      <td>
                        {f.provider_file_id && (
                          <button
                            type="button"
                            className="download-file-btn"
                            onClick={() => handleDownloadFile(f)}
                            disabled={resolvingPath === f.path}
                            title="Download directly from the provider"
                          >
                            {resolvingPath === f.path ? 'Resolving…' : 'Download'}
                          </button>
                        )}
                      </td>
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
