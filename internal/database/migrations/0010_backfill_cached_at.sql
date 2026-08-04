-- 0009's own comment assumed a future refresh would backfill every existing
-- row's cached_at — wrong for a row that was already sitting in
-- provider_completed at the time 0009 ran and never changes again:
-- RefreshFromProvider's no-op check (state/progress/size/error all
-- unchanged) skips the write entirely, so nothing ever revisits it. Found
-- live: a Manual download's detail view showed "Cached —" despite sitting
-- at 100% progress the whole time. added_at is the best available
-- approximation for these rows specifically — InsertDownload now stamps
-- cached_at at insert time for a row born already provider_completed (the
-- same bug, closed going forward), so any row still NULL here was, by
-- definition, either born that way or transitioned before 0009 ever existed
-- to record it — either way, "when it was added" is the closest true answer
-- left available, not a guess invented for this migration.
UPDATE downloads
SET cached_at = added_at
WHERE state = 'provider_completed' AND cached_at IS NULL;
