package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// pendingArrAddTTL is how long a failed *arr add stays claimable.
//
// Long enough to cover a provider that accepted an add and then went quiet for
// a while — the discovery pass that would adopt the item runs on the ordinary
// poll interval, so this only has to outlast a bad patch, not a bad day. Short
// enough that a genuinely-failed add does not leave a trap that mislabels an
// unrelated download later.
const pendingArrAddTTL = 2 * time.Hour

// PendingArrAdd is an *arr add that may have reached the provider even though
// it reported failure — see the 0014 migration for why this exists.
type PendingArrAdd struct {
	ID        string
	Provider  string
	Kind      Kind
	Hash      string
	Name      string
	Category  string
	SavePath  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// RecordPendingArrAdd notes an *arr add that failed after the request went out.
//
// Deliberately recorded for every add error rather than only the ambiguous
// ones. A definite rejection (a malformed magnet, say) leaves a row that
// matches nothing and expires harmlessly, whereas trying to classify which
// errors could still have landed means guessing at the provider's internals —
// and guessing wrong in the direction that loses the download.
func (db *DB) RecordPendingArrAdd(ctx context.Context, p *PendingArrAdd) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.ExpiresAt = now.Add(pendingArrAddTTL)
	_, err := db.ExecContext(ctx, `
		INSERT INTO pending_arr_adds (id, provider, kind, hash, name, category, save_path, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Provider, string(p.Kind), strings.ToLower(p.Hash), p.Name,
		p.Category, p.SavePath, p.CreatedAt, p.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("record pending arr add: %w", err)
	}
	return nil
}

// ClaimPendingArrAdd finds the pending add an untracked provider item belongs
// to, removing it so it can only be claimed once. Returns nil when the item is
// not one of ours.
//
// Hash first, and only then name. An infohash is exact and is what a torrent
// always carries; a name match is a fallback for usenet, which has no hash at
// all, and is the reason this whole path is best-effort there rather than
// certain.
func (db *DB) ClaimPendingArrAdd(ctx context.Context, provider string, kind Kind, hash, name string) (*PendingArrAdd, error) {
	now := time.Now().UTC()
	var (
		row  PendingArrAdd
		kstr string
	)
	scan := func(where, arg string) error {
		return db.QueryRowContext(ctx, `
			SELECT id, provider, kind, hash, name, category, save_path, created_at, expires_at
			FROM pending_arr_adds
			WHERE provider = ? AND kind = ? AND expires_at > ? AND `+where+`
			ORDER BY created_at
			LIMIT 1`,
			provider, string(kind), now, arg,
		).Scan(&row.ID, &row.Provider, &kstr, &row.Hash, &row.Name,
			&row.Category, &row.SavePath, &row.CreatedAt, &row.ExpiresAt)
	}

	var found bool
	if h := strings.ToLower(strings.TrimSpace(hash)); h != "" {
		if err := scan("hash = ?", h); err == nil {
			found = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("claim pending arr add by hash: %w", err)
		}
	}
	if !found {
		if n := strings.TrimSpace(name); n != "" {
			if err := scan("name = ?", n); err == nil {
				found = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("claim pending arr add by name: %w", err)
			}
		}
	}
	if !found {
		return nil, nil
	}

	row.Kind = Kind(kstr)
	if _, err := db.ExecContext(ctx, `DELETE FROM pending_arr_adds WHERE id = ?`, row.ID); err != nil {
		return nil, fmt.Errorf("consume pending arr add %s: %w", row.ID, err)
	}
	return &row, nil
}

// PurgeExpiredPendingArrAdds drops rows past their expiry. Cheap, and keeps a
// provider that is failing every add from growing this table without bound.
func (db *DB) PurgeExpiredPendingArrAdds(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM pending_arr_adds WHERE expires_at <= ?`, time.Now().UTC()); err != nil {
		return fmt.Errorf("purge expired pending arr adds: %w", err)
	}
	return nil
}
