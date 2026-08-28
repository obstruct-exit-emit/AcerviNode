package main

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApplyPendingRestore_NothingStagedIsANoOp(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "acervinode.db")
	write(t, live, "current")

	if err := applyPendingRestore(dir); err != nil {
		t.Fatalf("applyPendingRestore() error = %v", err)
	}
	got, _ := os.ReadFile(live)
	if string(got) != "current" {
		t.Errorf("database = %q, want it untouched when nothing is staged", got)
	}
}

func TestApplyPendingRestore_SwapsAndKeepsThePrevious(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "acervinode.db")
	write(t, live, "current")
	write(t, filepath.Join(dir, pendingRestoreName), "snapshot")

	if err := applyPendingRestore(dir); err != nil {
		t.Fatalf("applyPendingRestore() error = %v", err)
	}

	got, err := os.ReadFile(live)
	if err != nil || string(got) != "snapshot" {
		t.Errorf("database = %q (%v), want the snapshot in place", got, err)
	}
	// A restore is the operation here most likely to be regretted, so the
	// database it replaced is kept rather than deleted.
	prev, err := os.ReadFile(filepath.Join(dir, displacedName))
	if err != nil || string(prev) != "current" {
		t.Errorf("previous database = %q (%v), want it kept aside", prev, err)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingRestoreName)); !os.IsNotExist(err) {
		t.Error("staged file still present — the next startup would restore again")
	}
}

// The -wal and -shm sidecars belong to the database being replaced. Left in
// place they would be read as the restored one's, which is a corrupt pairing
// and exactly the sort of thing that looks like data loss.
func TestApplyPendingRestore_ClearsTheOldSidecars(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "acervinode.db")
	write(t, live, "current")
	write(t, live+"-wal", "wal")
	write(t, live+"-shm", "shm")
	write(t, filepath.Join(dir, pendingRestoreName), "snapshot")

	if err := applyPendingRestore(dir); err != nil {
		t.Fatalf("applyPendingRestore() error = %v", err)
	}
	for _, sidecar := range []string{live + "-wal", live + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("%s survived the restore", filepath.Base(sidecar))
		}
	}
}

// A first run has no database yet. Restoring into that is legitimate, not an
// error to bail on.
func TestApplyPendingRestore_NoExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, pendingRestoreName), "snapshot")

	if err := applyPendingRestore(dir); err != nil {
		t.Fatalf("applyPendingRestore() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "acervinode.db"))
	if err != nil || string(got) != "snapshot" {
		t.Errorf("database = %q (%v), want the snapshot in place", got, err)
	}
}

func TestStageRestore_CopiesWithoutMovingTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "acervinode-20260101-000000.db")
	write(t, snapshot, "snapshot")
	dataDir := t.TempDir()

	if err := stageRestore(snapshot, dataDir); err != nil {
		t.Fatalf("stageRestore() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, pendingRestoreName))
	if err != nil || string(got) != "snapshot" {
		t.Errorf("staged = %q (%v), want a copy of the snapshot", got, err)
	}
	// Copied, never moved: restoring a snapshot must not consume it.
	if _, err := os.Stat(snapshot); err != nil {
		t.Errorf("the snapshot itself was disturbed: %v", err)
	}
}

// Staging twice must land on the second choice, not append to the first.
func TestStageRestore_ReplacesAnEarlierStage(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "acervinode-20260101-000000.db")
	second := filepath.Join(dir, "acervinode-20260102-000000.db")
	write(t, first, "aaaaaaaaaaaaaaaaaaaa")
	write(t, second, "bbb")
	dataDir := t.TempDir()

	if err := stageRestore(first, dataDir); err != nil {
		t.Fatal(err)
	}
	if err := stageRestore(second, dataDir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dataDir, pendingRestoreName))
	if string(got) != "bbb" {
		t.Errorf("staged = %q, want the second snapshot to replace the first entirely", got)
	}
}
