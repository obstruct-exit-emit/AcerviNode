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
