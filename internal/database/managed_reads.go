package database

import (
	"context"
	"fmt"
)

// The Managed-only read path for the compat shims.
//
// An *arr app must never be shown, or be able to act on, a Manual download.
// Both shims already record AddedViaArr on everything they add — the reads are
// what was missing, and the assumption that *arr's own category scoping made
// that harmless is wrong: the GetItems loop in both Sonarr's and Radarr's
// qBittorrent client reads a torrent's category and never filters on it, and
// SABnzbd's GetQueue/GetHistory do the same. Every row a shim reported went
// into *arr's queue.
//
// On a real account that means the operator's whole discovered library. The
// worst of it is silent: Sonarr's QueueSpecification rejects a release when a
// queued item already matches that episode, so a Manual copy sitting in the
// queue stops the real grab and presents as an indexer problem. Manual rows
// also never leave provider_completed (that is what Manual means) and so read
// as perpetually "downloading", errored ones read as permanent warnings, and a
// Manual row in SABnzbd history reports "Failed" — the one status Sonarr maps
// to DownloadItemStatus.Failed, which blocklists a release and re-searches.
//
// These are deliberately separate functions rather than a flag on the existing
// ones: the native API and web UI are exactly where a Manual download is
// supposed to be visible, so the unfiltered versions stay the default and the
// narrower contract is the thing a caller has to ask for by name.

// ListManagedDownloads returns every Managed download of kind, newest first —
// the listing both compat shims read. See the package-level reasoning above.
func (db *DB) ListManagedDownloads(ctx context.Context, kind Kind) ([]*Download, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+downloadColumns+` FROM downloads WHERE kind = ? AND added_via = ? ORDER BY added_at DESC`,
		string(kind), string(AddedViaArr))
	if err != nil {
		return nil, fmt.Errorf("list managed downloads: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetManagedDownloadByHash is GetDownloadByHash scoped to Managed downloads,
// returning nil for a Manual one exactly as it would for a hash nobody tracks.
// Hiding a Manual download from the listing is not enough on its own: without
// this, an *arr app naming its hash directly could still delete the operator's
// own download, files included.
func (db *DB) GetManagedDownloadByHash(ctx context.Context, hash string) (*Download, error) {
	return db.scanOneDownload(ctx,
		`SELECT `+downloadColumns+` FROM downloads WHERE hash = ? AND kind = 'torrent' AND added_via = ?`,
		hash, string(AddedViaArr))
}

// GetManagedDownloadByID is GetDownloadByID scoped to Managed downloads — the
// SABnzbd shim's equivalent of GetManagedDownloadByHash, since its API is keyed
// on our own row id (the nzo_id it reports) rather than an infohash.
func (db *DB) GetManagedDownloadByID(ctx context.Context, id string) (*Download, error) {
	return db.scanOneDownload(ctx,
		`SELECT `+downloadColumns+` FROM downloads WHERE id = ? AND added_via = ?`,
		id, string(AddedViaArr))
}
