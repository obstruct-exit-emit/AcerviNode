package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDB writes a placeholder file, standing in for SQLite's own snapshot.
type fakeDB struct {
	calls int
	err   error
}

func (f *fakeDB) BackupTo(_ context.Context, path string) error {
	if f.err != nil {
		return f.err
	}
	f.calls++
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("snapshot"), 0o600)
}

func TestRunOnce_WritesASnapshot(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, time.Hour, 7)

	path, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), filePrefix) || !strings.HasSuffix(path, fileSuffix) {
		t.Errorf("wrote %q, want the acervinode-<stamp>.db shape", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("snapshot not on disk: %v", err)
	}
}

// Pruning keeps the newest and removes the rest. Names sort in time order,
// which is what makes this work without reading mtimes back off disk.
func TestPrune_KeepsNewest(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, time.Hour, 3)

	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("%s2026080%d-120000%s", filePrefix, i, fileSuffix)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.prune(); err != nil {
		t.Fatalf("prune() error = %v", err)
	}

	got, err := r.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("kept %d snapshots, want 3", len(got))
	}
	// Newest first, and the newest three are the ones retained.
	for i, want := range []string{"20260806", "20260805", "20260804"} {
		if !strings.Contains(got[i].Name, want) {
			t.Errorf("kept[%d] = %s, want the one containing %s", i, got[i].Name, want)
		}
	}
}

// keep <= 0 retains everything, so a mistyped value can never wipe the lot.
func TestPrune_KeepZeroRetainsEverything(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, time.Hour, 0)
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("%s2026080%d-120000%s", filePrefix, i, fileSuffix)
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
	if err := r.prune(); err != nil {
		t.Fatalf("prune() error = %v", err)
	}
	got, _ := r.List()
	if len(got) != 4 {
		t.Errorf("kept %d, want all 4 — keep<=0 must not delete", len(got))
	}
}

// Anything not written by this package is left strictly alone, including a
// snapshot deliberately renamed to stop it being pruned.
func TestPrune_IgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, time.Hour, 1)
	for i := 1; i <= 3; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s2026080%d-120000%s", filePrefix, i, fileSuffix)), []byte("x"), 0o600)
	}
	keepMe := filepath.Join(dir, "keep-this-one.db")
	os.WriteFile(keepMe, []byte("mine"), 0o600)

	if err := r.prune(); err != nil {
		t.Fatalf("prune() error = %v", err)
	}
	if _, err := os.Stat(keepMe); err != nil {
		t.Errorf("a file this package didn't write was deleted: %v", err)
	}
}

// A failed snapshot must not prune: trimming first would mean a failed
// backup had also thrown away a good one.
func TestRunOnce_FailureDoesNotPrune(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{err: fmt.Errorf("disk on fire")}, dir, time.Hour, 1)
	for i := 1; i <= 3; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s2026080%d-120000%s", filePrefix, i, fileSuffix)), []byte("x"), 0o600)
	}

	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want the failure surfaced")
	}
	got, _ := r.List()
	if len(got) != 3 {
		t.Errorf("kept %d after a failed backup, want all 3 left alone", len(got))
	}
}

// The timestamp comes from the name, not mtime: copying or restoring a
// backup rewrites mtime while the name still says when it was taken.
func TestList_TakenAtComesFromTheName(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, time.Hour, 7)
	name := filePrefix + "20260815-133000" + fileSuffix
	os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)

	got, err := r.List()
	if err != nil || len(got) != 1 {
		t.Fatalf("List() = %v, %v", got, err)
	}
	want := time.Date(2026, 8, 15, 13, 30, 0, 0, time.UTC)
	if !got[0].TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v (parsed from the filename)", got[0].TakenAt, want)
	}
}

// A missing directory is "nothing taken yet", not an error — it's created
// by the first backup rather than up front.
func TestList_MissingDirIsEmpty(t *testing.T) {
	r := New(&fakeDB{}, filepath.Join(t.TempDir(), "nope"), time.Hour, 7)
	got, err := r.List()
	if err != nil {
		t.Fatalf("List() error = %v, want nil for a missing dir", err)
	}
	if len(got) != 0 {
		t.Errorf("List() = %v, want empty", got)
	}
}
