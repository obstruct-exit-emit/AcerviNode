interface Props {
  count: number
  onDelete: () => void
  onClear: () => void
  // Omitted entirely for the Manual tab — retry only makes sense for a
  // Managed download (see DownloadsTable's own allowRetry doc comment).
  // retryCount is the subset of the selection actually in error state (the
  // only state a retry can act on) — 0 hides the button rather than
  // offering an action that would silently do nothing.
  onRetry?: () => void
  retryCount?: number
}

// A slim bar that only exists once something's selected — appears above the
// table it belongs to, replacing nothing, so the table looks identical to
// before this existed the rest of the time. See DownloadsTable's
// selectedIds/onToggleSelect/onToggleSelectAll props, which this acts on.
export function BulkActionBar({ count, onDelete, onClear, onRetry, retryCount }: Props) {
  if (count === 0) return null

  return (
    <div className="bulk-action-bar">
      <span className="bulk-action-count">{count} selected</span>
      {onRetry && retryCount ? (
        <button type="button" className="bulk-retry-btn" onClick={onRetry}>
          Retry {retryCount}
        </button>
      ) : null}
      <button type="button" className="bulk-delete-btn" onClick={onDelete}>
        Delete {count}
      </button>
      <button type="button" className="bulk-clear-btn" onClick={onClear} title="Clear selection">
        ✕
      </button>
    </div>
  )
}
