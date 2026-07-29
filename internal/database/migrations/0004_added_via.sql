ALTER TABLE downloads ADD COLUMN added_via TEXT NOT NULL DEFAULT 'arr' CHECK (added_via IN ('arr', 'manual'));

CREATE INDEX idx_downloads_added_via ON downloads (added_via);

-- discovery_baseline/discovery_seeded back internal/importer's discovery step,
-- which adopts a download added directly through the provider's own site/app
-- (i.e. one AcerviNode has no local row for at all) as a manual download. The
-- baseline is what stops that from flooding the Manual tab with an account's
-- entire pre-existing history the first time this feature runs: every
-- currently-unmatched provider item is recorded here (once, per provider+kind
-- — see discovery_seeded) instead of being adopted, so only items that show
-- up afterward ever are.
CREATE TABLE discovery_baseline (
    provider              TEXT NOT NULL,
    kind                  TEXT NOT NULL,
    provider_download_id  TEXT NOT NULL,
    PRIMARY KEY (provider, kind, provider_download_id)
);

CREATE TABLE discovery_seeded (
    provider   TEXT NOT NULL,
    kind       TEXT NOT NULL,
    seeded_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (provider, kind)
);
