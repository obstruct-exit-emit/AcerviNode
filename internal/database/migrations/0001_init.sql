CREATE TABLE downloads (
    id                   TEXT PRIMARY KEY,
    provider             TEXT NOT NULL,
    provider_download_id TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('torrent', 'usenet')),
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
    error_message        TEXT
);

CREATE INDEX idx_downloads_kind ON downloads (kind);
CREATE INDEX idx_downloads_hash ON downloads (hash);
CREATE UNIQUE INDEX idx_downloads_provider_download_id ON downloads (provider, provider_download_id);

CREATE TABLE download_files (
    id                      TEXT PRIMARY KEY,
    download_id             TEXT NOT NULL REFERENCES downloads (id) ON DELETE CASCADE,
    provider_file_id        TEXT,
    path                    TEXT NOT NULL,
    size_bytes              INTEGER NOT NULL DEFAULT 0,
    download_url            TEXT,
    download_url_expires_at TIMESTAMP
);

CREATE INDEX idx_download_files_download_id ON download_files (download_id);
