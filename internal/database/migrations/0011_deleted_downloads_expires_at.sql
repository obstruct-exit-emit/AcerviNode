-- Per-tombstone expiry, replacing a single global grace period.
--
-- deleted_downloads was written on the premise that a tombstone only ever
-- has to outlive the provider's own listing lag, because "an id that's
-- genuinely gone never legitimately reappears". That premise silently
-- assumed the provider-side delete actually succeeded. It doesn't always:
-- handleDeleteDownload treats that call as best-effort and removes the
-- local row regardless (a provider outage or rate limit must not leave a
-- row the user can't get rid of). When it fails, the item really is still
-- on the account — so the short window guarantees the exact ghost this
-- table exists to prevent, just delayed until it lapses.
--
-- Confirmed live, not theorised: two downloads were deleted while the
-- account happened to be rate-limited, the provider deletes returned 429,
-- and both reappeared as ghost Manual downloads once the window passed —
-- including one that had been Managed, which came back in the wrong tab.
--
-- Existing rows keep exactly the behaviour they had (deleted_at + 5m).
ALTER TABLE deleted_downloads ADD COLUMN expires_at TIMESTAMP;

UPDATE deleted_downloads
SET expires_at = datetime(deleted_at, '+5 minutes')
WHERE expires_at IS NULL;
