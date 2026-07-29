-- SQLite CHECK constraints can't be altered in place; widening kind to allow
-- 'webdl' (TorBox's Web Downloads / hoster-debrid service, e.g. Mega,
-- 1Fichier, Mediafire — see internal/debrid/webdownload.go) means recreating
-- the table. download_files' foreign key to downloads.id is unaffected — no
-- id values change, only downloads' own schema does.
CREATE TABLE downloads_new (
    id                   TEXT PRIMARY KEY,
    provider             TEXT NOT NULL,
    provider_download_id TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('torrent', 'usenet', 'webdl')),
    hash                 TEXT,
    name                 TEXT NOT NULL,
    category             TEXT,
    save_path            TEXT,
    size_bytes           INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL DEFAULT 'queued',
    progress             REAL NOT NULL DEFAULT 0,
    added_at             TIMESTAMP NOT NULL,
    updated_at           TIMESTAMP NOT NULL,
    completed_at         TIMESTAMP,
    error_message        TEXT,
    retry_count          INTEGER NOT NULL DEFAULT 0,
    next_retry_at        TIMESTAMP,
    source               TEXT,
    added_via            TEXT NOT NULL DEFAULT 'arr' CHECK (added_via IN ('arr', 'manual'))
);

INSERT INTO downloads_new (
    id, provider, provider_download_id, kind, hash, name, category, save_path,
    size_bytes, state, progress, added_at, updated_at, completed_at, error_message,
    retry_count, next_retry_at, source, added_via
)
SELECT
    id, provider, provider_download_id, kind, hash, name, category, save_path,
    size_bytes, state, progress, added_at, updated_at, completed_at, error_message,
    retry_count, next_retry_at, source, added_via
FROM downloads;

DROP TABLE downloads;

ALTER TABLE downloads_new RENAME TO downloads;

CREATE INDEX idx_downloads_kind ON downloads (kind);
CREATE INDEX idx_downloads_hash ON downloads (hash);
CREATE UNIQUE INDEX idx_downloads_provider_download_id ON downloads (provider, provider_download_id);
CREATE INDEX idx_downloads_added_via ON downloads (added_via);
