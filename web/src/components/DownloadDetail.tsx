import { useEffect, useState } from 'react'
import {
  ApiError,
  getDownload,
  getFileLink,
  reAddDownload,
  retryDownload,
  type Download,
  type DownloadDetail as DownloadDetailData,
} from '../api'
import { forceDownload } from '../fsAccess'
import { formatBytes, formatDuration, formatRelativeTime, formatSpeed } from '../format'
import { StateBadge } from './StateBadge'

const POLL_INTERVAL_MS = 4000

interface Props {
  apiKey: string
  id: string
  onClose: () => void
  // Same "Download all" entry point the downloads table's per-row button
  // uses (handleDownloadAll in App.tsx) — reused here rather than
  // duplicated, so this button opens the exact same DownloadOptionsDialog,
  // covering every mode (folder/zip/individual) in one place instead of
  // this view having its own separate always-both-visible buttons.
  onDownloadAll: (d: Download) => void
  busy: boolean
  progress?: { loaded: number; total: number }
}

export function DownloadDetail({ apiKey, id, onClose, onDownloadAll, busy, progress }: Props) {
  const [detail, setDetail] = useState<DownloadDetailData | null>(null)
  const [error, setError] = useState<string | undefined>(undefined)
  const [retryStatus, setRetryStatus] = useState<{ kind: 'idle' | 'retrying' | 'error'; message?: string }>({ kind: 'idle' })
  const [readdStatus, setReaddStatus] = useState<{ kind: 'idle' | 'readding' | 'error'; message?: string }>({ kind: 'idle' })
  const [resolvingPath, setResolvingPath] = useState<string | null>(null)

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

    // A self-rescheduling timeout, not setInterval — this endpoint blocks
    // server-side on a live provider call that can take up to
    // provider_request_timeout_seconds (30s default) when the provider
    // itself is slow. setInterval fires on a fixed cadence regardless of
    // whether the previous call finished, so a single slow poll used to let
    // several more pile up behind it (each one its own live provider call,
    // on both the browser and TorBox's own account) instead of just running
    // late. Waiting for load() to actually finish before scheduling the next
    // one means a slow poll is at worst late, never compounding.
    let timer: ReturnType<typeof setTimeout>
    async function loop() {
      await load()
      if (!cancelled) timer = setTimeout(loop, POLL_INTERVAL_MS)
    }
    loop()
    return () => {
      cancelled = true
      clearTimeout(timer)
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
      // forceDownload (blob + a synthetic <a download> click), not a plain
      // link open — the resolved link is the provider's own CDN URL with
      // no Content-Disposition: attachment, so a plain navigation renders
      // it inline (plays the video, shows the image) in every browser
      // instead of downloading it. See fsAccess.forceDownload.
      const name = f.path.split('/').filter(Boolean).pop() ?? f.path
      await forceDownload(url, name)
    } catch (err) {
      alert(err instanceof ApiError ? err.message : String(err))
    } finally {
      setResolvingPath(null)
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

        {/* The initial GET blocks server-side on a live provider file-list
            call (see internal/api's handleGetDownload) — normally fast, but
            it can take up to provider_request_timeout_seconds if the
            provider itself is slow. Without this, that whole window showed
            nothing but the bare header, indistinguishable from a hung/broken
            modal. */}
        {!detail && !error && <p className="empty">Loading…</p>}

        {detail && (
          <>
            <dl className="detail-meta">
              <div>
                <dt>State</dt>
                <dd>
                  <StateBadge state={detail.state} addedVia={detail.added_via} phase={detail.phase} />
                </dd>
              </div>
              <div>
                <dt>Progress</dt>
                <dd>{Math.round(detail.progress * 100)}%</dd>
              </div>
              {/* ETA/speed/swarm info are only meaningful while actually
                  downloading — a queued, completed, or errored download's
                  values are 0/stale and would just be confusing here. */}
              {detail.state === 'downloading' && (
                <>
                  {/* Also gated on !detail.phase — see DownloadsTable's
                      identical guard for why: TorBox's own eta/speed don't
                      necessarily reset to 0 the moment a usenet download
                      finishes transferring and moves into a phase, and both
                      describe the transfer itself, which is already over by
                      then regardless of what's still being reported. */}
                  {!detail.phase && detail.eta_seconds > 0 && (
                    <div>
                      <dt>ETA</dt>
                      <dd>{formatDuration(detail.eta_seconds)}</dd>
                    </div>
                  )}
                  {!detail.phase && detail.download_speed_bytes > 0 && (
                    <div>
                      <dt>Speed</dt>
                      <dd>{formatSpeed(detail.download_speed_bytes)}</dd>
                    </div>
                  )}
                  {/* Seeders/leechers are torrent-only — usenet/webdl have
                      no BitTorrent-swarm concept, so both are always 0
                      there and this row just wouldn't render. */}
                  {detail.protocol === 'torrent' && (detail.seeders > 0 || detail.leechers > 0) && (
                    <div>
                      <dt>Swarm</dt>
                      <dd>
                        {detail.seeders} {detail.seeders === 1 ? 'seed' : 'seeds'}, {detail.leechers}{' '}
                        {detail.leechers === 1 ? 'peer' : 'peers'}
                      </dd>
                    </div>
                  )}
                </>
              )}
              {/* Only rendered when true: an airlocked download is the
                  exception, so a row saying "not airlocked" on everything
                  else would be noise. Tells the user this one is safe from
                  the provider's retention policy — the thing that
                  otherwise eventually makes a download vanish. */}
              {detail.airlocked && (
                <div>
                  <dt>Storage</dt>
                  <dd>AirLock — exempt from provider retention</dd>
                </div>
              )}
              <div>
                <dt>Protocol</dt>
                <dd>{detail.protocol}</dd>
              </div>
              <div>
                <dt>Provider</dt>
                <dd>{detail.provider}</dd>
              </div>
              {detail.added_via === 'arr' && (
                <div>
                  <dt>Category</dt>
                  <dd>{detail.category || '—'}</dd>
                </div>
              )}
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
              {/* Two different facts that used to share one "Cached" label.
                  Cached by provider is the provider's own timestamp for the
                  content, often long before this download existed because
                  someone else's download put it in the cache. Available is
                  when AcerviNode first saw *this* row complete. The first is
                  shown only when the provider reports one — AllDebrid unlocks
                  links rather than caching and reports nothing. */}
              {detail.provider_cached_at && (
                <div>
                  <dt>Cached by provider</dt>
                  <dd title={detail.provider_cached_at}>
                    {formatRelativeTime(detail.provider_cached_at)}
                  </dd>
                </div>
              )}
              <div>
                <dt>Available</dt>
                <dd title={detail.cached_at ?? undefined}>
                  {detail.cached_at ? formatRelativeTime(detail.cached_at) : '—'}
                </dd>
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

            {/* Retry only makes sense for a Managed download — internal/
                importer never auto-fetches a Manual one at all, so there's
                nothing for it to do; the row just reflects the provider's
                own live state instead. Re-add works for either bucket, as
                long as a source link was actually stored (has_source) —
                nothing to resubmit for a file upload, or a discovered
                download with no original link ever known. */}
            {detail.state === 'error' && (detail.added_via === 'arr' || detail.has_source) && (
              <div className="detail-retry">
                {detail.added_via === 'arr' && (
                  <button type="button" className="retry-btn" onClick={handleRetry} disabled={retryStatus.kind === 'retrying'}>
                    {retryStatus.kind === 'retrying' ? 'Retrying…' : 'Retry'}
                  </button>
                )}
                {detail.has_source && (
                  <button type="button" className="readd-btn" onClick={handleReAdd} disabled={readdStatus.kind === 'readding'}>
                    {readdStatus.kind === 'readding' ? 'Re-adding…' : 'Re-add'}
                  </button>
                )}
                {retryStatus.kind === 'error' && <p className="settings-error">Failed to retry: {retryStatus.message}</p>}
                {readdStatus.kind === 'error' && <p className="settings-error">Failed to re-add: {readdStatus.message}</p>}
              </div>
            )}

            <div className="files-header">
              <h3>Files</h3>
              {/* Manual-download actions only make sense for a Manual
                  download — a Managed one is already being auto-fetched to
                  local disk by internal/importer, so there's nothing to
                  manually grab. One "Download all" button opens
                  DownloadOptionsDialog (see onDownloadAll) — folder/zip/
                  individual are all choices inside it now, rather than
                  this view having its own separate always-both-visible
                  zip button. */}
              {detail.added_via === 'manual' && detail.files.length > 0 && (
                <button
                  type="button"
                  className="zip-btn"
                  onClick={() => onDownloadAll(detail)}
                  disabled={busy}
                  title="Download all files"
                >
                  {busy
                    ? progress && progress.total > 0
                      ? `Downloading… ${Math.round((progress.loaded / progress.total) * 100)}%`
                      : 'Downloading…'
                    : 'Download all'}
                </button>
              )}
            </div>
            {detail.files.length === 0 ? (
              <p className="empty">
                {detail.files_error ? `Couldn't get files: ${detail.files_error}` : 'No files yet.'}
              </p>
            ) : (
              <table className="detail-files">
                <thead>
                  <tr>
                    <th>Path</th>
                    <th>Size</th>
                    {detail.added_via === 'manual' && <th />}
                  </tr>
                </thead>
                <tbody>
                  {detail.files.map((f) => (
                    <tr key={f.path}>
                      <td className="mono">{f.path}</td>
                      <td>{formatBytes(f.size_bytes)}</td>
                      {detail.added_via === 'manual' && (
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
                      )}
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
