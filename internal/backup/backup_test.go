package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// countingDB is fakeDB's concurrency-safe sibling, for the tests that run
// the scheduler loop in its own goroutine. Reading a plain int written by
// that goroutine would be a data race the -race build correctly rejects.
type countingDB struct {
	mu    sync.Mutex
	calls int
}

func (c *countingDB) BackupTo(_ context.Context, path string) error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("snapshot"), 0o600)
}

func (c *countingDB) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// waitForCount polls until the fake has been called at least n times, or the
// deadline passes. Polling rather than sleeping a fixed span keeps these
// tests fast when they pass and still generous when the machine is loaded.
func waitForCount(t *testing.T, db *countingDB, n int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := db.count(); got >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return db.count()
}

// TestRun_SnapshotsOnTheInterval covers the scheduler loop itself, which
// every other test in this file skips in favour of calling RunOnce directly
// — leaving the one path that actually runs unattended for months untested.
func TestRun_SnapshotsOnTheInterval(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), 30*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if got := waitForCount(t, db, 3, 3*time.Second); got < 3 {
		t.Errorf("BackupTo called %d times, want at least 3 — the loop isn't rescheduling", got)
	}
}

// TestRun_DoesNotSnapshotAtStartup pins the deliberate choice not to back up
// the moment the process starts: a restart loop would otherwise fill the
// directory with snapshots of a database nobody had a chance to change, and
// push the useful older ones out of the retention window.
func TestRun_DoesNotSnapshotAtStartup(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), time.Hour, 7)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	time.Sleep(120 * time.Millisecond)
	if got := db.count(); got != 0 {
		t.Errorf("BackupTo called %d times before the first interval elapsed, want 0", got)
	}
}

// TestRun_ZeroIntervalNeverFires is how backups are switched off. A zero
// duration must mean "never", not "immediately" — the difference between a
// disabled feature and a hot loop writing snapshots as fast as it can.
func TestRun_ZeroIntervalNeverFires(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), 0, 7)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	if got := db.count(); got != 0 {
		t.Errorf("BackupTo called %d times with a zero interval, want 0 (backups disabled)", got)
	}
}

// TestRun_SetConfigRetunesARunningLoop proves a changed interval takes
// effect without waiting out the old one — the reason SetConfig signals the
// loop at all rather than just storing the value.
func TestRun_SetConfigRetunesARunningLoop(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), time.Hour, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	time.Sleep(80 * time.Millisecond)
	if got := db.count(); got != 0 {
		t.Fatalf("fired %d times on the original hour-long interval, want 0", got)
	}

	r.SetConfig(30*time.Millisecond, 0)
	if got := waitForCount(t, db, 2, 3*time.Second); got < 2 {
		t.Errorf("BackupTo called %d times after retuning to 30ms, want at least 2", got)
	}
}

// TestRun_SetConfigToZeroStopsTheLoop is the retune in the other direction:
// switching backups off while running must actually stop them.
func TestRun_SetConfigToZeroStopsTheLoop(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), 25*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	waitForCount(t, db, 2, 3*time.Second)
	r.SetConfig(0, 0)
	time.Sleep(60 * time.Millisecond) // let any in-flight tick land
	settled := db.count()

	time.Sleep(200 * time.Millisecond)
	if got := db.count(); got != settled {
		t.Errorf("BackupTo called %d more times after backups were switched off", got-settled)
	}
}

// TestRun_StopsOnContextCancel proves shutdown is clean rather than the
// goroutine outliving the process's own lifecycle.
func TestRun_StopsOnContextCancel(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), 25*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	waitForCount(t, db, 2, 3*time.Second)

	cancel()
	time.Sleep(60 * time.Millisecond)
	settled := db.count()

	time.Sleep(200 * time.Millisecond)
	if got := db.count(); got != settled {
		t.Errorf("BackupTo called %d more times after the context was cancelled", got-settled)
	}
}
