package database

import (
	"path/filepath"
	"testing"
)

func TestOpen_AppliesMigrationsCleanly(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 10 {
		t.Errorf("schema_migrations count = %d, want 10", count)
	}

	for _, table := range []string{"downloads", "download_files"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}
}

func TestOpen_MigrationsAreIdempotentAcrossReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acervinode.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 10 {
		t.Errorf("schema_migrations count after reopen = %d, want 10 (migrations re-applied)", count)
	}
}

// TestOpen_EnablesWALMode guards a real fix for reported hanging/stuttering:
// SetMaxOpenConns(1) means every operation, read or write, already
// serializes through one connection regardless of journal mode — so the
// only thing journal mode changes here is how long each individual write
// holds that connection for. The default rollback journal fsyncs the whole
// database file on every commit; WAL (+ synchronous=NORMAL) doesn't, and
// measured live against a real copy of this project's own database, that
// made each write several times faster. A real file is required — WAL is a
// silent no-op on an in-memory database (see Open's own doc comment).
func TestOpen_EnablesWALMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acervinode.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var sync int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	// synchronous is reported back as an integer — 1 is NORMAL (0 OFF, 2 FULL,
	// 3 EXTRA), per SQLite's own pragma docs.
	if sync != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
