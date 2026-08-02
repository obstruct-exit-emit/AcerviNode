export function formatBytes(bytes: number): string {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unitIndex = 0
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024
    unitIndex++
  }
  return `${value.toFixed(unitIndex === 0 ? 0 : 1)} ${units[unitIndex]}`
}

// formatSpeed matches formatBytes' own unit scaling, plus a trailing "/s" —
// used for a download's live transfer speed (see Download.download_speed_bytes).
export function formatSpeed(bytesPerSecond: number): string {
  return `${formatBytes(bytesPerSecond)}/s`
}

// formatDuration renders a live ETA (seconds) compactly for a human reader —
// "2m 15s", "1h 03m" — distinct from the H:MM:SS format the SABnzbd shim
// sends *arr apps (internal/sabnzbd's own formatTimeLeft): that shape exists
// to match real SABnzbd's wire format for a protocol parser, not for a
// person reading a dashboard. Non-positive (unknown/stalled/done) reports
// "—", matching how the rest of this dashboard shows "nothing to say" for a
// value with no meaningful number yet.
export function formatDuration(seconds: number): string {
  if (seconds <= 0) return '—'
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const secs = Math.floor(seconds % 60)
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, '0')}m`
  if (minutes > 0) return `${minutes}m ${String(secs).padStart(2, '0')}s`
  return `${secs}s`
}

export function formatRelativeTime(iso: string): string {
  const date = new Date(iso)
  const seconds = Math.round((date.getTime() - Date.now()) / 1000)
  const divisions: [Intl.RelativeTimeFormatUnit, number][] = [
    ['second', 60],
    ['minute', 60],
    ['hour', 24],
    ['day', 30],
    ['month', 12],
    ['year', Infinity],
  ]
  const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })
  let duration = seconds
  for (const [unit, amount] of divisions) {
    if (Math.abs(duration) < amount) {
      return rtf.format(Math.round(duration), unit)
    }
    duration /= amount
  }
  return rtf.format(Math.round(duration), 'year')
}

export const STATE_LABELS: Record<string, string> = {
  queued: 'Queued',
  downloading: 'Downloading',
  provider_completed: 'Fetching',
  ready_for_import: 'Ready',
  error: 'Error',
}
