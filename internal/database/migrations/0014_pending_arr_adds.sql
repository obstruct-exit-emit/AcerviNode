-- An *arr add that reached the provider but whose reply never reached us
-- leaves nothing tracked locally: the item sits on the account, the next
-- discovery pass finds it untracked, and adopts it as a Manual download. The
-- practical symptom is "a Managed download turned into a Manual one" — it
-- never became Managed at all, because the row was never created.
--
-- Nothing rewrites added_via, so this cannot be fixed after the fact by
-- flipping a column: the fix is to know the add was ours. This records the
-- intent of a failed *arr add so discovery can recognise the item as one when
-- it turns up, and adopt it as Managed with its original category and save
-- path instead.
--
-- Matching is by infohash for torrents, which is exact and available on both
-- sides. Usenet downloads have no client-side hash (debrid.DownloadStatus.Hash
-- is documented empty for them), so those match on name and are best-effort.
--
-- Rows expire: an add that genuinely failed at the provider must not leave a
-- trap that mislabels an unrelated download months later.
CREATE TABLE pending_arr_adds (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    kind       TEXT NOT NULL,
    hash       TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    category   TEXT NOT NULL DEFAULT '',
    save_path  TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_pending_arr_adds_lookup ON pending_arr_adds (provider, kind, expires_at);
