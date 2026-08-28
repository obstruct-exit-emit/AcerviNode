package backup

import (
	"errors"
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
	// 0644 on purpose: SQLite's VACUUM INTO creates its file under the
	// process umask, which is world-readable on a normal install. Writing
	// this restrictively would have let the permission test pass without the
	// chmod that actually fixes it -- which is exactly what it did until a
	// mutation check caught it.
	return os.WriteFile(path, []byte("snapshot"), 0o644)
}

func TestRunOnce_WritesASnapshot(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, "", time.Hour, 7)

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
	r := New(&fakeDB{}, dir, "", time.Hour, 3)

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
	r := New(&fakeDB{}, dir, "", time.Hour, 0)
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
	r := New(&fakeDB{}, dir, "", time.Hour, 1)
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
	r := New(&fakeDB{err: fmt.Errorf("disk on fire")}, dir, "", time.Hour, 1)
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
	r := New(&fakeDB{}, dir, "", time.Hour, 7)
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
	r := New(&fakeDB{}, filepath.Join(t.TempDir(), "nope"), "", time.Hour, 7)
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
	r := New(db, t.TempDir(), "", 30*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if got := waitForCount(t, db, 3, 3*time.Second); got < 3 {
		t.Errorf("BackupTo called %d times, want at least 3 — the loop isn't rescheduling", got)
	}
}

// snapshotAged writes a snapshot file stamped as though it were taken age ago.
// takenAt prefers the timestamp in the name, so this controls the schedule.
func snapshotAged(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	name := filePrefix + time.Now().UTC().Add(-age).Format(timeLayout) + fileSuffix
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

// TestRun_DoesNotSnapshotWhenARecentOneExists is the guarantee that used to be
// "never back up at startup", now stated in terms of what it was actually
// protecting: a restart loop must not fill the directory with snapshots of a
// database nobody had a chance to change.
//
// The old absolute version had an unintended consequence -- an instance
// restarted more often than the interval never backed up at all -- so the rule
// is now that a *recent* snapshot suppresses the next one, however many times
// the process restarts.
func TestRun_DoesNotSnapshotWhenARecentOneExists(t *testing.T) {
	dir := t.TempDir()
	snapshotAged(t, dir, 0)
	db := &countingDB{}
	r := New(db, dir, "", time.Hour, 7)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	time.Sleep(1500 * time.Millisecond) // comfortably past dueNow
	if got := db.count(); got != 0 {
		t.Errorf("BackupTo called %d times with a fresh snapshot on disk, want 0", got)
	}
}

// TestRun_SnapshotsWhenOverdue is the bug this scheduling exists to fix. An
// instance restarted more often than its interval used to restart the clock
// every time and so never backed up; on the development box that produced
// three days of drift while every setting in the database changed.
func TestRun_SnapshotsWhenOverdue(t *testing.T) {
	dir := t.TempDir()
	snapshotAged(t, dir, 3*time.Hour) // interval long past
	db := &countingDB{}
	r := New(db, dir, "", time.Hour, 7)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if got := waitForCount(t, db, 1, 4*time.Second); got < 1 {
		t.Error("BackupTo never called with a snapshot older than the interval")
	}
}

// Nothing on disk means nothing to protect, so the first snapshot is taken
// rather than waiting out a full interval for a baseline that does not exist.
func TestRun_SnapshotsWhenNothingOnDisk(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), "", time.Hour, 7)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if got := waitForCount(t, db, 1, 4*time.Second); got < 1 {
		t.Error("BackupTo never called on an empty backup directory")
	}
}

func TestInitialDelay(t *testing.T) {
	t.Run("disabled stays disabled", func(t *testing.T) {
		r := New(&fakeDB{}, t.TempDir(), "", 0, 7)
		// Must stay non-positive: newTimer reads that as "never fire", and
		// returning dueNow here would back up on an install that switched
		// backups off.
		if got := r.initialDelay(0); got > 0 {
			t.Errorf("initialDelay(0) = %v, want <= 0", got)
		}
	})

	t.Run("waits out the remainder", func(t *testing.T) {
		dir := t.TempDir()
		snapshotAged(t, dir, 30*time.Minute)
		r := New(&fakeDB{}, dir, "", time.Hour, 7)
		got := r.initialDelay(time.Hour)
		if got < 25*time.Minute || got > 31*time.Minute {
			t.Errorf("initialDelay = %v, want roughly the 30 minutes remaining", got)
		}
	})

	t.Run("a future timestamp waits at most one interval", func(t *testing.T) {
		dir := t.TempDir()
		snapshotAged(t, dir, -5*time.Hour) // stamped in the future
		r := New(&fakeDB{}, dir, "", time.Hour, 7)
		// A clock corrected after the fact must not park the scheduler for
		// five hours.
		if got := r.initialDelay(time.Hour); got > time.Hour {
			t.Errorf("initialDelay = %v, want no more than the interval", got)
		}
	})
}

// TestRun_ZeroIntervalNeverFires is how backups are switched off. A zero
// duration must mean "never", not "immediately" — the difference between a
// disabled feature and a hot loop writing snapshots as fast as it can.
func TestRun_ZeroIntervalNeverFires(t *testing.T) {
	db := &countingDB{}
	r := New(db, t.TempDir(), "", 0, 7)

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
	r := New(db, t.TempDir(), "", time.Hour, 0)

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
	r := New(db, t.TempDir(), "", 25*time.Millisecond, 0)

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
	r := New(db, t.TempDir(), "", 25*time.Millisecond, 0)

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

// The config file, not the database, holds the provider keys, the API key and
// every login account. A snapshot without it restores the download history and
// leaves you locked out, which is the opposite of what a backup is for.
func TestRunOnce_CopiesTheConfigBesideTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("api_key: secret\nport: 7846\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(&fakeDB{}, dir, cfg, time.Hour, 7)

	path, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	copied := strings.TrimSuffix(path, fileSuffix) + configSuffix
	got, err := os.ReadFile(copied)
	if err != nil {
		t.Fatalf("config copy not written beside the snapshot: %v", err)
	}
	if !strings.Contains(string(got), "api_key: secret") {
		t.Errorf("config copy = %q, want the original contents", got)
	}
}

// Both halves carry things nobody else should read: the database has the whole
// download history, the config has credentials. VACUUM INTO creates its file
// under the process umask, so the mode has to be set rather than inherited.
func TestRunOnce_SnapshotAndConfigAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("api_key: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(&fakeDB{}, dir, cfg, time.Hour, 7)

	path, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	for _, f := range []string{path, strings.TrimSuffix(path, fileSuffix) + configSuffix} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %o, want no group or world access", filepath.Base(f), perm)
		}
	}
}

// An install running entirely from environment variables has no config file,
// and that is not a failure.
func TestRunOnce_MissingConfigIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, filepath.Join(t.TempDir(), "absent.yaml"), time.Hour, 7)

	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Errorf("RunOnce() error = %v, want a missing config to be tolerated", err)
	}
}

// The pair shares a timestamp, so pruning one must take the other. A config
// copy left behind would accumulate forever and still hold old credentials.
func TestPrune_RemovesTheConfigCopyToo(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("api_key: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(&fakeDB{}, dir, cfg, time.Hour, 1)

	var paths []string
	for i := 0; i < 3; i++ {
		p, err := r.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		time.Sleep(1100 * time.Millisecond) // stamps are second-granular
	}

	yamls, err := filepath.Glob(filepath.Join(dir, "*"+configSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(yamls) != 1 {
		t.Errorf("%d config copies left after pruning to 1 snapshot, want 1", len(yamls))
	}
}

func TestDelete_RemovesTheSnapshotAndItsConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("api_key: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(&fakeDB{}, dir, cfg, time.Hour, 7)
	path, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(filepath.Base(path)); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("snapshot still on disk after Delete")
	}
	if _, err := os.Stat(strings.TrimSuffix(path, fileSuffix) + configSuffix); !os.IsNotExist(err) {
		t.Error("config copy left behind — it holds the credentials, so it is the half that matters")
	}
}

// The name reaches Delete straight from a URL path segment. isSnapshotName
// checks only a prefix and a suffix, which every one of these satisfies, so
// the timestamp parse and the filepath.Base comparison are what actually stop
// them. Each must be refused before anything touches the filesystem.
func TestDelete_RefusesAnythingItDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	r := New(&fakeDB{}, dir, "", time.Hour, 7)

	// A real file one directory up, to prove nothing escapes to it.
	outside := filepath.Join(dir, "..", "acervinode-20260101-000000.db")
	if err := os.WriteFile(outside, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"acervinode-../../etc/shadow.db",
		"acervinode-" + filepath.Join("..", "acervinode-20260101-000000") + fileSuffix,
		"../acervinode-20260101-000000.db",
		"acervinode-notatimestamp.db",
		"acervinode-.db",
		"acervinode-20260101-000000.yaml",
		"config.yaml",
		"",
		".",
		"..",
	} {
		if err := r.Delete(name); !errors.Is(err, ErrNotASnapshot) {
			t.Errorf("Delete(%q) error = %v, want ErrNotASnapshot", name, err)
		}
	}

	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the backup directory was removed: %v", err)
	}
}

func TestDelete_MissingSnapshotReportsNotExist(t *testing.T) {
	r := New(&fakeDB{}, t.TempDir(), "", time.Hour, 7)
	err := r.Delete("acervinode-20260101-000000.db")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Delete() error = %v, want it to report not-exist so the API can 404", err)
	}
}

// A snapshot taken before config copies existed has no .yaml beside it, and
// deleting it is not a failure.
func TestDelete_ToleratesAMissingConfigCopy(t *testing.T) {
	dir := t.TempDir()
	name := filePrefix + "20260101-000000" + fileSuffix
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(&fakeDB{}, dir, "", time.Hour, 7)
	if err := r.Delete(name); err != nil {
		t.Errorf("Delete() error = %v, want a lone snapshot to delete cleanly", err)
	}
}
