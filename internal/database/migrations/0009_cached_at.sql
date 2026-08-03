-- cached_at records the first time a download's state was observed as
-- provider_completed — the provider itself finished, whether or not
-- internal/importer has since fetched it to local disk (completed_at, a
-- separate column, only fires once files are actually on disk and stays
-- NULL forever for a Manual download that's never fetched). NULL for
-- every existing row until a future refresh sets it.
ALTER TABLE downloads ADD COLUMN cached_at TIMESTAMP;
