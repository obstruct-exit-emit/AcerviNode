package database

import (
	"context"
	"fmt"
	"time"
)

// IsDiscoveryBaselineSeeded reports whether SeedDiscoveryBaseline has already
// run for provider+kind — see internal/importer's discovery step, which
// calls this once per tick to decide whether an unmatched provider item
// should be adopted as a manual download or recorded into the baseline
// instead (the very first run for a given provider+kind never adopts
// anything — see SeedDiscoveryBaseline).
func (db *DB) IsDiscoveryBaselineSeeded(ctx context.Context, provider string, kind Kind) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM discovery_seeded WHERE provider = ? AND kind = ?`,
		provider, string(kind)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check discovery baseline seeded: %w", err)
	}
	return n > 0, nil
}

// SeedDiscoveryBaseline records every currently-unmatched provider download
// ID as permanently ignored, then marks provider+kind as seeded — called
// exactly once, the first time internal/importer's discovery step finds
// unmatched items for a provider+kind with no prior baseline. This is what
// stops discovery from flooding the Manual tab with an account's entire
// pre-existing history the moment this feature ships: everything present at
// seed time is excluded forever; only items that show up afterward are ever
// adopted.
func (db *DB) SeedDiscoveryBaseline(ctx context.Context, provider string, kind Kind, providerDownloadIDs []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed discovery baseline: %w", err)
	}
	defer tx.Rollback()

	for _, id := range providerDownloadIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO discovery_baseline (provider, kind, provider_download_id) VALUES (?, ?, ?)`,
			provider, string(kind), id,
		); err != nil {
			return fmt.Errorf("seed discovery baseline entry %s: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO discovery_seeded (provider, kind, seeded_at) VALUES (?, ?, ?)`,
		provider, string(kind), time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("mark discovery seeded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed discovery baseline: %w", err)
	}
	return nil
}

// DiscoveryBaseline returns the set of provider download IDs permanently
// excluded from discovery for provider+kind (see SeedDiscoveryBaseline).
func (db *DB) DiscoveryBaseline(ctx context.Context, provider string, kind Kind) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT provider_download_id FROM discovery_baseline WHERE provider = ? AND kind = ?`,
		provider, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list discovery baseline: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan discovery baseline entry: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// HasAnyDownloads reports whether this database has ever tracked a single
// download, of any kind, Managed or Manual. internal/importer's
// discoverManual checks this once per tick to tell a genuinely fresh
// install (nothing tracked yet at all) apart from an established instance
// seeing a particular provider+kind for the first time (a newly added
// second provider, say) — only the former should adopt everything already
// sitting in the account instead of baselining it away forever. See
// discoverManual's own doc comment.
func (db *DB) HasAnyDownloads(ctx context.Context) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM downloads)`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check any downloads exist: %w", err)
	}
	return n > 0, nil
}

// recentlyDeletedGracePeriod is how long a tombstone recorded by
// RecordDeletedDownload keeps an item excluded from discovery — see
// RecentlyDeletedDownloads. Generous on purpose: a provider's own delete
// isn't always instantly reflected in its listing endpoints (confirmed live
// against a real account: TorBox's mylist could still briefly show a torrent
// just after its delete call returned success), and there's essentially no
// downside to erring long here — a provider_download_id that's genuinely
// gone never legitimately reappears (a fresh add always gets a new one), so
// this only ever blocks re-adopting the exact same now-defunct id.
const recentlyDeletedGracePeriod = 5 * time.Minute

// RecordDeletedDownload tombstones a just-deleted download so
// internal/importer's discoverManual doesn't immediately re-adopt it as a
// "new" discovery if the provider's own listing endpoints haven't caught up
// with the delete yet — see handleDeleteDownload (internal/api), the only
// caller, and RecentlyDeletedDownloads. Opportunistically prunes tombstones
// older than recentlyDeletedGracePeriod on every call rather than needing a
// separate cleanup job — this table is small and write-infrequent, so a
// delete-then-insert on every real deletion is cheap.
func (db *DB) RecordDeletedDownload(ctx context.Context, provider string, kind Kind, providerDownloadID string) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM deleted_downloads WHERE deleted_at < ?`,
		time.Now().UTC().Add(-recentlyDeletedGracePeriod),
	); err != nil {
		return fmt.Errorf("prune old deleted-download tombstones: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deleted_downloads (provider, kind, provider_download_id, deleted_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (provider, kind, provider_download_id) DO UPDATE SET deleted_at = excluded.deleted_at`,
		provider, string(kind), providerDownloadID, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("record deleted download %s: %w", providerDownloadID, err)
	}
	return nil
}

// RecentlyDeletedDownloads returns the set of provider download IDs for
// provider+kind tombstoned within recentlyDeletedGracePeriod — discoverManual
// skips adopting any of these, rather than re-creating a ghost Manual
// download for something that was just intentionally deleted (see
// RecordDeletedDownload).
func (db *DB) RecentlyDeletedDownloads(ctx context.Context, provider string, kind Kind) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT provider_download_id FROM deleted_downloads WHERE provider = ? AND kind = ? AND deleted_at >= ?`,
		provider, string(kind), time.Now().UTC().Add(-recentlyDeletedGracePeriod))
	if err != nil {
		return nil, fmt.Errorf("list recently deleted downloads: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan deleted-download entry: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
