-- deleted_downloads is a short-lived tombstone: recorded whenever a download
-- is deleted, checked by internal/importer's discoverManual before adopting
-- an "untracked" provider item as a fresh discovery. Guards against a real,
-- observed race: a provider's delete isn't always instantly reflected in its
-- own listing endpoints (TorBox's mylist can still show an item for a brief
-- window after a delete call returns success), and the background importer
-- polls independently of any specific delete request — if a tick lands in
-- that window, the still-technically-present item looks exactly like a
-- brand-new item to discoverManual, and gets silently re-adopted as a ghost
-- Manual download for something that was just intentionally deleted.
CREATE TABLE deleted_downloads (
    provider              TEXT NOT NULL,
    kind                  TEXT NOT NULL,
    provider_download_id  TEXT NOT NULL,
    deleted_at            TIMESTAMP NOT NULL,
    PRIMARY KEY (provider, kind, provider_download_id)
);
