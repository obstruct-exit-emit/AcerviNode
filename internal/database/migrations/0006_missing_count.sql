-- missing_count backs proactive vanished-Manual-download detection (see
-- RefreshFromProvider) — how many consecutive successful provider listings a
-- tracked AddedViaManual download has been absent from. Defaults to 0 for
-- every existing row (nothing's "missing" until a future tick actually finds
-- it absent).
ALTER TABLE downloads ADD COLUMN missing_count INTEGER NOT NULL DEFAULT 0;
