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
	if count != 6 {
		t.Errorf("schema_migrations count = %d, want 6", count)
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
	if count != 6 {
		t.Errorf("schema_migrations count after reopen = %d, want 6 (migrations re-applied)", count)
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
