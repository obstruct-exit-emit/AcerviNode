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
	"os"
	"path/filepath"
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

	// fetchProgress backs SetFetchProgress/FetchProgress/ClearFetchProgress
	// — see fetchProgressStore's own doc comment (fetch_progress.go).
	fetchProgress fetchProgressStore
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
	// Airlocked is provider-side permanent storage (TorBox AirLock) — see
	// debrid.DownloadStatus.Airlocked. Cached here rather than persisted
	// for the same reason as the rest of this struct: it's the provider's
	// state, changeable from outside AcerviNode at any time.
	Airlocked bool
}

// BackupTo writes a consistent snapshot of the database to path.
//
// Uses SQLite's own VACUUM INTO rather than copying the file. A plain copy
// of a live database is not safe: with WAL enabled the committed state is
// split across the main file and the -wal, so a copy taken mid-write can be
// torn or simply miss recent commits. VACUUM INTO writes a single
// self-contained file from one consistent view, and compacts it on the way
// out, so the result is a smaller file that opens cleanly on its own.
//
// path must not already exist — SQLite refuses to overwrite, which is a
// useful guard rather than something to work around: a backup that silently
// replaced a good file with a failed write would be worse than no backup.
//
// Runs on the same single connection as everything else (see Open's
// SetMaxOpenConns(1)), so it briefly blocks other queries. That is a real
// cost, but a proportionate one — this database holds configuration and
// download history, not bulk data, and a snapshot takes milliseconds.
func (db *DB) BackupTo(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup %s already exists", path)
	}
	// Parameterised rather than interpolated: a path is user-supplied
	// configuration, and VACUUM INTO takes it as a value.
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("backup database to %s: %w", path, err)
	}
	return nil
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
	// WAL + synchronous=NORMAL instead of SQLite's own defaults (a rollback
	// journal, synchronous=FULL) — found investigating real "hanging/
	// stuttering" reports: every write was fsyncing the whole database file
	// on every single commit, and since SetMaxOpenConns(1) above already
	// serializes every operation (read or write, from any goroutine) through
	// this one connection, that fsync latency was directly how long every
	// *other* pending query — including the web UI's own list poll — had to
	// wait its turn. Doesn't change the single-connection design at all
	// (WAL's usual headline benefit, concurrent readers not blocked by a
	// writer, doesn't even apply here — there's only ever one connection to
	// begin with); the actual win is that a WAL commit is an append, not a
	// full-file fsync, so each individual write is faster, which is what
	// actually shortens everyone else's wait. Measured live, single sqlite3
	// process, 200 individual-transaction writes against a real copy of this
	// project's own database: ~0.2ms/write before, ~0.06ms/write after — a
	// small absolute difference on this dev machine's fast disk, but the
	// same relative win would matter far more on slower storage, which is
	// exactly where stuttering would actually become noticeable. A no-op,
	// not an error, on an in-memory (":memory:") database, e.g. in tests —
	// SQLite silently keeps "memory" journaling for those, per its own docs.
	if _, err := sqlDB.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable WAL journal mode: %w", err)
	}
	if _, err := sqlDB.Exec(`PRAGMA synchronous = NORMAL`); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set synchronous=NORMAL: %w", err)
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
