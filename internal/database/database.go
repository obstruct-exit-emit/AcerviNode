// Package database owns AcerviNode's SQLite connection and embedded schema
// migrations. Both compat shims (qBittorrent and SABnzbd) share the single
// "downloads" table defined here — see downloads.go for the CRUD surface.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps a *sql.DB opened against AcerviNode's SQLite database, with
// migrations already applied.
type DB struct {
	*sql.DB

	// refreshMu/refreshState back two related, in-memory-only concerns
	// RefreshFromProvider owns — see refreshGuardAllows, the only thing
	// that reads or writes this map:
	//
	//  1. Ordering guard against a real, observed race: multiple
	//     independent pollers (each compat shim's own reactive refresh,
	//     internal/importer's bulk tick, its fast per-download poll) can
	//     all be mid-flight against the provider for the same download at
	//     once. The connection pool serializes the resulting UPDATEs (see
	//     SetMaxOpenConns(1) below), but serialization only guarantees
	//     they don't corrupt each other — not that they land in the same
	//     order their underlying provider data was actually fetched in. A
	//     slower request started earlier can finish (and write) after a
	//     faster one started later, silently regressing progress/state
	//     back to stale data with nothing to ever correct it. Found live:
	//     a real torrent's reported progress in /api/v2/torrents/info
	//     stuck at 13.9% while the same download's own database row (and
	//     TorBox's own API, queried directly) had already reached 50%+.
	//
	//  2. LiveStatus's own storage: fast-moving provider fields (ETA,
	//     torrent swarm info, usenet post-processing phase) that are
	//     deliberately never persisted to the downloads table — both
	//     compat shims already read these fresh off of every poll's own
	//     statuses slice directly, but internal/api's native
	//     API/web UI has no such poll of its own to read from (it only
	//     ever reads the database). Caching the latest value here, right
	//     where RefreshFromProvider already has it, lets the native API
	//     show the same live data without adding yet another synchronous
	//     provider call per request — which, fresh off fixing concern 1
	//     above, is exactly the kind of extra concurrent polling this
	//     package needs less of, not more.
	//
	// Keyed by database.Download.ID.
	refreshMu    sync.Mutex
	refreshState map[string]refreshCacheEntry
}

// refreshCacheEntry is refreshMu/refreshState's value type — see DB's own
// doc comment.
type refreshCacheEntry struct {
	// fetchedAt is when the update that produced live was fetched (not
	// when it was written) — the ordering guard compares this, never live
	// itself.
	fetchedAt time.Time
	live      LiveStatus
}

// LiveStatus is a snapshot of a download's fast-moving, provider-reported
// fields that are deliberately never persisted to the downloads table —
// see DB.refreshState's own doc comment for why, and LiveStatus (the
// method) for how a caller reads this.
type LiveStatus struct {
	ETASeconds int64
	// Seeders/Leechers/DownloadSpeedBytes are torrent-only — always 0 for
	// usenet/webdl, which have no BitTorrent-swarm concept.
	Seeders            int64
	Leechers           int64
	DownloadSpeedBytes int64
	// Phase is usenet-only — see debrid.DownloadStatus.Phase.
	Phase string
}

// Open opens (creating if necessary) the SQLite database at dsn and applies
// any migrations that haven't run yet. dsn is a modernc.org/sqlite data
// source, e.g. a file path or ":memory:".
func Open(dsn string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// modernc.org/sqlite serializes internally, but a single connection keeps
	// migration ordering and busy-database errors predictable for this v0 slice.
	// It also means the foreign_keys pragma (per-connection, off by default in
	// SQLite) stays in effect for every query, enabling the download_files
	// cascade delete.
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable foreign_keys: %w", err)
	}

	db := &DB{DB: sqlDB, refreshState: map[string]refreshCacheEntry{}}
	if err := db.migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TIMESTAMP NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UTC()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}
	}

	return nil
}

// migrationVersion extracts the leading integer from a migration filename
// like "0001_init.sql" -> 1.
func migrationVersion(filename string) (int, error) {
	prefix, _, ok := strings.Cut(filename, "_")
	if !ok {
		return 0, fmt.Errorf("migration filename %q missing '_' separator", filename)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration filename %q has non-numeric version: %w", filename, err)
	}
	return version, nil
}
