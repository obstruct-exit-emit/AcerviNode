package importer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// fakeProvider is a minimal provider backed by an httptest.Server standing
// in for a debrid CDN — real HTTP round trips, no network.
type fakeProvider struct {
	cdn       *httptest.Server
	files     []debrid.DownloadFile
	failLinks map[string]bool // fileID -> force RequestDownloadLink to fail
	statuses  []debrid.DownloadStatus
	listErr   error
	// listCalls is atomic because TestSetConfig_ResetsTickerInterval reads it
	// from the test goroutine while Importer.Run's own goroutine calls List
	// concurrently.
	listCalls atomic.Int32

	statusErr error
	// statusCalls is atomic for the same reason listCalls is — see
	// refreshActiveDownloads tests, which run Run's fast-poll goroutine
	// concurrently with the test's own assertions.
	statusCalls atomic.Int32

	// requestedAtMu guards requestedAt: TestTick_RespectsMaxConcurrent runs
	// multiple downloads' RequestDownloadLink calls concurrently against the
	// same shared fakeProvider, which a plain unsynchronized append can't
	// survive.
	requestedAtMu sync.Mutex
	requestedAt   []string // fileIDs RequestDownloadLink was called for, in order

	deleteErr    error
	deletedIDsMu sync.Mutex
	deletedIDs   []string // provider ids Delete was called for, in order — see cleanupOldDownloads tests
}

func (f *fakeProvider) Name() string { return "faketorbox" }

func (f *fakeProvider) List(_ context.Context) ([]debrid.DownloadStatus, error) {
	f.listCalls.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.statuses, nil
}

// Status looks id up in the same statuses fixture List() serves — a fake
// stand-in for TorBox's real id-filtered mylist lookup (see
// torbox.Client.GetTorrent). Distinct listCalls/statusCalls counters let a
// test tell the bulk path and the fast per-download path apart.
func (f *fakeProvider) Status(_ context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	f.statusCalls.Add(1)
	if f.statusErr != nil {
		return debrid.DownloadStatus{}, f.statusErr
	}
	for _, st := range f.statuses {
		if st.ID == id {
			return st, nil
		}
	}
	return debrid.DownloadStatus{}, fmt.Errorf("fakeProvider: status: %s not found", id)
}

func (f *fakeProvider) Files(_ context.Context, _ debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	return f.files, nil
}

func (f *fakeProvider) RequestDownloadLink(_ context.Context, _ debrid.ProviderDownloadID, fileID string) (string, error) {
	f.requestedAtMu.Lock()
	f.requestedAt = append(f.requestedAt, fileID)
	f.requestedAtMu.Unlock()
	if f.failLinks[fileID] {
		return "", errors.New("provider: link resolution failed")
	}
	return f.cdn.URL + "/" + fileID, nil
}

func (f *fakeProvider) requestedCount() int {
	f.requestedAtMu.Lock()
	defer f.requestedAtMu.Unlock()
	return len(f.requestedAt)
}

func (f *fakeProvider) Delete(_ context.Context, id debrid.ProviderDownloadID, _ bool) error {
	f.deletedIDsMu.Lock()
	f.deletedIDs = append(f.deletedIDs, string(id))
	f.deletedIDsMu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return nil
}

func (f *fakeProvider) deletedCount() int {
	f.deletedIDsMu.Lock()
	defer f.deletedIDsMu.Unlock()
	return len(f.deletedIDs)
}

func openTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTick_DownloadsFilesAndMarksReadyForImport(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	fileContents := map[string]string{
		"1": "subtitle contents",
		"2": "the actual movie bytes, pretend this is large",
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fileID := r.URL.Path[1:]
		w.Write([]byte(fileContents[fileID]))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn: cdn,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "Show/movie.en.srt", SizeBytes: int64(len(fileContents["1"]))},
			{ProviderFileID: "2", Path: "Show/movie.mkv", SizeBytes: int64(len(fileContents["2"]))},
		},
	}

	destDir := t.TempDir()
	d := &database.Download{
		ID: "dl-1", Provider: "fake", ProviderDownloadID: "provider-1", Kind: database.KindTorrent,
		Hash: "abc123", Name: "Show", Category: "tv-sonarr", SavePath: destDir,
		State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	for fileID, path := range map[string]string{"1": "Show/movie.en.srt", "2": "Show/movie.mkv"} {
		data, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(data) != fileContents[fileID] {
			t.Errorf("%s contents = %q, want %q", path, data, fileContents[fileID])
		}
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateReadyForImport {
		t.Errorf("state = %q, want ready_for_import", got.State)
	}
	if got.Progress != 1.0 {
		t.Errorf("progress = %v, want 1.0", got.Progress)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be set once files are on disk")
	}
}

func TestTick_UsesDownloadDirWhenNoSavePath(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("episode bytes"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn: cdn,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: int64(len("episode bytes"))},
		},
	}

	downloadDir := t.TempDir()
	d := &database.Download{
		ID: "dl-2", Provider: "fake", ProviderDownloadID: "provider-2", Kind: database.KindUsenet,
		Name: "Some Release", Category: "radarr", State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, nil, provider, downloadDir, time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := filepath.Join(downloadDir, "radarr", "Some Release", "episode.mkv")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected file at %s, stat error = %v", want, err)
	}

	// The computed fallback destination must be persisted back onto the row
	// — both compat shims report save_path/storage straight from this
	// column for the *arr app's own import step to read (handleInfo/
	// handleProperties, sabnzbd's handleHistory). Left empty, the *arr app
	// sees "Completed" with nowhere to import from and silently never does
	// — found live via a real LibriNode setup where every other symptom
	// looked fine.
	got, err := db.GetDownloadByID(ctx, "dl-2")
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	wantSavePath := filepath.Join(downloadDir, "radarr", "Some Release")
	if got.SavePath != wantSavePath {
		t.Errorf("SavePath after fetch = %q, want the computed fallback %q persisted", got.SavePath, wantSavePath)
	}
}

// seedReadyForImportDownload inserts a Managed, ready_for_import download
// with the given completedAt and a real file at destDir/name.mkv — a helper
// shared by the cleanup tests below.
func seedReadyForImportDownload(t *testing.T, db *database.DB, id, name string, completedAt time.Time, destDir string) *database.Download {
	t.Helper()
	ctx := context.Background()
	d := &database.Download{
		ID: id, Provider: "faketorbox", ProviderDownloadID: id, Kind: database.KindTorrent,
		Hash: id, Name: name, Category: "tv-sonarr", SavePath: destDir,
		State: database.StateReadyForImport, AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload(%s) error = %v", id, err)
	}
	if err := db.UpdateDownloadStatus(ctx, id, database.StateReadyForImport, 1, 0, &completedAt, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus(%s) error = %v", id, err)
	}
	if destDir != "" {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", destDir, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, "movie.mkv"), []byte("pretend movie bytes"), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
	}
	return d
}

// TestCleanupOldDownloads_DisabledByDefault proves cleanup_after_days=0 (the
// default) means Tick never touches an old ready_for_import download at
// all — no accidental deletion for anyone who hasn't opted in.
func TestCleanupOldDownloads_DisabledByDefault(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	destDir := filepath.Join(t.TempDir(), "Old.Movie")

	old := seedReadyForImportDownload(t, db, "old-1", "Old.Movie", time.Now().UTC().AddDate(0, 0, -30), destDir)

	provider := &fakeProvider{}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, old.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got == nil {
		t.Fatal("download was removed even though cleanup is disabled")
	}
	if _, err := os.Stat(filepath.Join(destDir, "movie.mkv")); err != nil {
		t.Errorf("local file removed even though cleanup is disabled: %v", err)
	}
	if provider.deletedCount() != 0 {
		t.Errorf("provider Delete called %d times, want 0", provider.deletedCount())
	}
}

// TestCleanupOldDownloads_RemovesOldReadyForImportDownload is the positive
// case: once enabled, a Managed download that's been ready_for_import
// longer than cleanup_after_days gets its local files removed, the
// provider-side download deleted, its row removed, and a delete tombstone
// recorded (the same race-avoidance a user-initiated delete gets — see
// database.RecordDeletedDownload) — while a too-recent one and a Manual
// download in the analogous "available" state are both left alone.
func TestCleanupOldDownloads_RemovesOldReadyForImportDownload(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	oldDestDir := filepath.Join(t.TempDir(), "Old.Movie")
	old := seedReadyForImportDownload(t, db, "old-1", "Old.Movie", time.Now().UTC().AddDate(0, 0, -30), oldDestDir)

	recentDestDir := filepath.Join(t.TempDir(), "Recent.Movie")
	recent := seedReadyForImportDownload(t, db, "recent-1", "Recent.Movie", time.Now().UTC().Add(-1*time.Hour), recentDestDir)

	manual := &database.Download{
		ID: "manual-1", Provider: "faketorbox", ProviderDownloadID: "manual-1", Kind: database.KindTorrent,
		Hash: "manual-1", Name: "Manual.Movie", State: database.StateProviderCompleted, AddedVia: database.AddedViaManual,
	}
	if err := db.InsertDownload(ctx, manual); err != nil {
		t.Fatalf("InsertDownload(manual) error = %v", err)
	}

	provider := &fakeProvider{}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	im.SetCleanupAfterDays(7)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if got, err := db.GetDownloadByID(ctx, old.ID); err != nil {
		t.Fatalf("GetDownloadByID(old) error = %v", err)
	} else if got != nil {
		t.Error("old download row still present, want it removed")
	}
	if _, err := os.Stat(oldDestDir); !os.IsNotExist(err) {
		t.Errorf("old download's local directory still present (err = %v), want removed", err)
	}
	if provider.deletedCount() != 1 || provider.deletedIDs[0] != old.ProviderDownloadID {
		t.Errorf("provider.deletedIDs = %v, want exactly [%s]", provider.deletedIDs, old.ProviderDownloadID)
	}
	tombstoned, err := db.RecentlyDeletedDownloads(ctx, old.Provider, old.Kind)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if !tombstoned[old.ProviderDownloadID] {
		t.Error("expected a delete tombstone recorded for the cleaned-up download")
	}

	if got, err := db.GetDownloadByID(ctx, recent.ID); err != nil {
		t.Fatalf("GetDownloadByID(recent) error = %v", err)
	} else if got == nil {
		t.Error("recent download was removed, want it left alone (not old enough)")
	}
	if _, err := os.Stat(filepath.Join(recentDestDir, "movie.mkv")); err != nil {
		t.Errorf("recent download's file removed: %v", err)
	}

	if got, err := db.GetDownloadByID(ctx, manual.ID); err != nil {
		t.Fatalf("GetDownloadByID(manual) error = %v", err)
	} else if got == nil {
		t.Error("manual download was removed, want it left alone (never eligible)")
	}
}

// TestCleanupOldDownloads_EmptyNameSkipsFileRemovalOnly proves a row with no
// Name (destDir would otherwise collapse to the bare category directory
// shared with other downloads) still gets cleaned up provider-side and in
// the database, but its local file removal is skipped rather than risking
// os.RemoveAll on something broader than intended.
func TestCleanupOldDownloads_EmptyNameSkipsFileRemovalOnly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	categoryDir := t.TempDir()
	sibling := filepath.Join(categoryDir, "sibling.txt")
	if err := os.WriteFile(sibling, []byte("unrelated file"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	d := &database.Download{
		ID: "noname-1", Provider: "faketorbox", ProviderDownloadID: "noname-1", Kind: database.KindTorrent,
		Hash: "noname-1", Name: "", Category: "tv-sonarr", SavePath: categoryDir,
		State: database.StateReadyForImport, AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	completedAt := time.Now().UTC().AddDate(0, 0, -30)
	if err := db.UpdateDownloadStatus(ctx, d.ID, database.StateReadyForImport, 1, 0, &completedAt, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}

	provider := &fakeProvider{}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	im.SetCleanupAfterDays(7)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if got, err := db.GetDownloadByID(ctx, d.ID); err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	} else if got != nil {
		t.Error("row still present, want it removed despite the skipped file cleanup")
	}
	if provider.deletedCount() != 1 {
		t.Errorf("provider Delete called %d times, want 1", provider.deletedCount())
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling file in the shared category dir was removed: %v", err)
	}
}

// TestCleanupOldDownloads_ProviderDeleteFailureStillCleansUpLocally proves
// the provider-side delete is best-effort, the same as
// handleDeleteDownload's own stance — a real, existing 500 from TorBox on a
// delete call must not leave the local row (and disk usage) stuck forever.
func TestCleanupOldDownloads_ProviderDeleteFailureStillCleansUpLocally(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	destDir := filepath.Join(t.TempDir(), "Old.Movie")

	old := seedReadyForImportDownload(t, db, "old-1", "Old.Movie", time.Now().UTC().AddDate(0, 0, -30), destDir)

	provider := &fakeProvider{deleteErr: errors.New("torbox: DATABASE_ERROR (status 500)")}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	im.SetCleanupAfterDays(7)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if got, err := db.GetDownloadByID(ctx, old.ID); err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	} else if got != nil {
		t.Error("row still present after a provider-delete failure, want it removed anyway (best-effort)")
	}
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		t.Errorf("local directory still present after a provider-delete failure (err = %v), want removed anyway", err)
	}
}

func TestTick_SkipsAlreadyDownloadedFiles(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	content := "already have this"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("CDN should not be hit for an already-downloaded file")
	}))
	t.Cleanup(cdn.Close)

	destDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(destDir, "file.mkv"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	provider := &fakeProvider{
		cdn: cdn,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "file.mkv", SizeBytes: int64(len(content))},
		},
	}

	d := &database.Download{
		ID: "dl-3", Provider: "fake", ProviderDownloadID: "provider-3", Kind: database.KindTorrent,
		Hash: "def456", Name: "X", SavePath: destDir, State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if n := provider.requestedCount(); n != 0 {
		t.Errorf("RequestDownloadLink called for already-present file: %d calls", n)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateReadyForImport {
		t.Errorf("state = %q, want ready_for_import", got.State)
	}
}

func TestTick_LeavesRowForRetryOnFailure(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "file.mkv", SizeBytes: 100},
		},
		failLinks: map[string]bool{"1": true},
	}
	// cdn is nil here on purpose — RequestDownloadLink fails before it would
	// ever be dereferenced.

	d := &database.Download{
		ID: "dl-4", Provider: "fake", ProviderDownloadID: "provider-4", Kind: database.KindTorrent,
		Hash: "fail1", Name: "Y", SavePath: t.TempDir(), State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	before := time.Now().UTC()
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("state = %q, want provider_completed (left for retry)", got.State)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", got.RetryCount)
	}
	if got.NextRetryAt == nil || !got.NextRetryAt.After(before) {
		t.Errorf("NextRetryAt = %v, want a time after %v (backed off)", got.NextRetryAt, before)
	}
	if got.ErrorMessage == "" {
		t.Error("ErrorMessage should be set even while still retrying, not just after giving up")
	}
}

// TestHandleFailure_BackoffGrowsWithEachAttempt drives three consecutive
// failures directly through handleFailure and checks next_retry_at moves
// further out each time — exponential, not fixed-interval, retrying.
func TestHandleFailure_BackoffGrowsWithEachAttempt(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := &database.Download{
		ID: "dl-backoff", Provider: "fake", ProviderDownloadID: "provider-backoff", Kind: database.KindTorrent,
		Hash: "backoff1", Name: "Backoff Test", SavePath: t.TempDir(), State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, nil, nil, t.TempDir(), 10*time.Second, 10)

	var previousWait time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		before := time.Now().UTC()
		im.handleFailure(ctx, d, errors.New("simulated failure"))

		got, err := db.GetDownloadByID(ctx, d.ID)
		if err != nil {
			t.Fatalf("GetDownloadByID() error = %v", err)
		}
		if got.RetryCount != attempt {
			t.Fatalf("attempt %d: RetryCount = %d, want %d", attempt, got.RetryCount, attempt)
		}
		if got.NextRetryAt == nil {
			t.Fatalf("attempt %d: NextRetryAt is nil", attempt)
		}
		wait := got.NextRetryAt.Sub(before)
		if attempt > 1 && wait <= previousWait {
			t.Errorf("attempt %d: wait = %v, want longer than previous attempt's %v", attempt, wait, previousWait)
		}
		previousWait = wait
		d = got // next iteration's handleFailure needs the updated RetryCount
	}
}

// TestHandleFailure_GivesUpAfterMaxRetries proves a download stops being
// retried and moves to StateError once it has failed MaxRetries times,
// rather than being retried forever.
func TestHandleFailure_GivesUpAfterMaxRetries(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := &database.Download{
		ID: "dl-giveup", Provider: "fake", ProviderDownloadID: "provider-giveup", Kind: database.KindTorrent,
		Hash: "giveup1", Name: "Give Up Test", SavePath: t.TempDir(), State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	const maxRetries = 3
	im := New(db, nil, nil, t.TempDir(), time.Millisecond, maxRetries)

	for i := 1; i <= maxRetries; i++ {
		im.handleFailure(ctx, d, errors.New("simulated failure"))
		got, err := db.GetDownloadByID(ctx, d.ID)
		if err != nil {
			t.Fatalf("GetDownloadByID() error = %v", err)
		}
		d = got
	}

	if d.State != database.StateError {
		t.Errorf("state after %d failures = %q, want error (gave up)", maxRetries, d.State)
	}
	if d.ErrorMessage == "" {
		t.Error("ErrorMessage should be set on give-up")
	}
	if d.CompletedAt != nil {
		t.Error("CompletedAt should stay unset — a give-up isn't a completion")
	}
}

// TestTick_DoesNotRetryBeforeNextRetryAt proves a download in backoff isn't
// retried early — the fake provider records every RequestDownloadLink call,
// so a Tick that shouldn't touch this download must leave that list empty.
func TestTick_DoesNotRetryBeforeNextRetryAt(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{
		files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "file.mkv", SizeBytes: 100}},
	}

	d := &database.Download{
		ID: "dl-backoff-wait", Provider: "fake", ProviderDownloadID: "provider-backoff-wait", Kind: database.KindTorrent,
		Hash: "waiting1", Name: "Waiting", SavePath: t.TempDir(), State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	if err := db.UpdateDownloadRetry(ctx, d.ID, 1, time.Now().UTC().Add(time.Hour), "backing off"); err != nil {
		t.Fatalf("UpdateDownloadRetry() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if n := provider.requestedCount(); n != 0 {
		t.Errorf("provider was contacted during backoff: %d calls", n)
	}
}

func TestTick_RejectsPathTraversal(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("malicious"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn: cdn,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "../../etc/passwd", SizeBytes: 9},
		},
	}

	destDir := t.TempDir()
	d := &database.Download{
		ID: "dl-5", Provider: "fake", ProviderDownloadID: "provider-5", Kind: database.KindTorrent,
		Hash: "evil1", Name: "Z", SavePath: destDir, State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(destDir), "etc", "passwd")); err == nil {
		t.Error("path traversal wrote a file outside the destination directory")
	}
	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("state = %q, want provider_completed (rejected, left for investigation)", got.State)
	}
}

// TestTick_ProactivelyRefreshesAndFetchesWithinOneTick proves the actual
// point of refreshStatuses: a row still sitting in StateQueued/StateDownloading
// is synced against the provider's List() *and* has its files fetched to
// ready_for_import within the same Tick call, without anything external
// (an *arr app polling /info, or a person hitting the qBittorrent shim) ever
// touching it — previously this could only ever happen reactively, so a
// download watched only through the native API/web UI could sit looking
// "queued" indefinitely even after the provider finished it.
func TestTick_ProactivelyRefreshesAndFetchesWithinOneTick(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("movie bytes"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn: cdn,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "movie.mkv", SizeBytes: int64(len("movie bytes"))},
		},
		statuses: []debrid.DownloadStatus{
			{ID: "provider-6", State: debrid.StateCompleted, Progress: 1, SizeBytes: 12345},
		},
	}

	destDir := t.TempDir()
	d := &database.Download{
		ID: "dl-6", Provider: "fake", ProviderDownloadID: "provider-6", Kind: database.KindTorrent,
		Hash: "proactive1", Name: "Proactive", SavePath: destDir, State: database.StateQueued,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateReadyForImport {
		t.Errorf("state = %q, want ready_for_import (refresh + fetch in one tick)", got.State)
	}
	if _, err := os.Stat(filepath.Join(destDir, "movie.mkv")); err != nil {
		t.Errorf("expected file fetched within the same tick: %v", err)
	}
}

// TestTick_RefreshDoesNotRegressReadyForImport proves a row already marked
// ready_for_import stays that way even if the provider is still reporting an
// earlier state (e.g. its own cache hasn't caught up yet) — matches
// database.RefreshFromProvider's own guarantee, exercised here through the
// full Tick path.
func TestTick_RefreshDoesNotRegressReadyForImport(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{
		statuses: []debrid.DownloadStatus{
			{ID: "provider-7", State: debrid.StateDownloading, Progress: 0.5},
		},
	}

	d := &database.Download{
		ID: "dl-7", Provider: "fake", ProviderDownloadID: "provider-7", Kind: database.KindTorrent,
		Hash: "ready1", Name: "Already Ready", SavePath: t.TempDir(),
		State: database.StateReadyForImport, Progress: 1,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateReadyForImport {
		t.Errorf("state = %q, want it to stay ready_for_import", got.State)
	}
}

// TestTick_CallsListEvenWhenNothingTracked proves List() still runs with
// nothing tracked locally — an older version of this test asserted the
// opposite (skipping List() entirely as a minor optimization), but discovery
// (see discoverManual) needs every tick's List() call to notice a first-ever
// manually-added download for a kind nothing's tracked yet, so that
// optimization was removed.
func TestTick_CallsListEvenWhenNothingTracked(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{}
	im := New(db, provider, provider, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if provider.listCalls.Load() != 2 {
		t.Errorf("List() called %d times, want 2 (once each for torrent/usenet)", provider.listCalls.Load())
	}
}

// TestRefreshKind_RateLimit_SetsCooldownAndSkipsFurtherCalls proves a
// debrid.ErrRateLimited from List() sets a cooldown for that kind, and that
// a subsequent Tick skips calling List() again entirely while still
// cooling — the whole point being not to keep hammering a provider that
// just rate-limited the account, unlike the previous behavior (retry every
// single tick regardless of why the last one failed).
func TestRefreshKind_RateLimit_SetsCooldownAndSkipsFurtherCalls(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{listErr: fmt.Errorf("faketorbox: list: %w", debrid.ErrRateLimited)}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)

	im.refreshKind(ctx, database.KindTorrent, provider, false)
	if provider.listCalls.Load() != 1 {
		t.Fatalf("List() called %d times, want 1", provider.listCalls.Load())
	}
	until, cooling := im.RateLimitCooldownUntil(database.KindTorrent)
	if !cooling {
		t.Fatal("expected torrent kind to be in a rate-limit cooldown")
	}
	if !until.After(time.Now()) {
		t.Errorf("cooldown until %v, want it in the future", until)
	}

	// A second refreshKind call while still cooling must not call List()
	// again at all.
	im.refreshKind(ctx, database.KindTorrent, provider, false)
	if provider.listCalls.Load() != 1 {
		t.Errorf("List() called %d times after a second refreshKind during cooldown, want still 1", provider.listCalls.Load())
	}
}

// TestRefreshKind_RateLimit_ScopedPerKind proves a rate limit on one kind's
// List() call doesn't affect another kind's cooldown — each provider slot
// (torrent/usenet/webdl) is backed by its own concrete provider instance in
// practice, and a rate limit on one shouldn't pause polling for the others.
func TestRefreshKind_RateLimit_ScopedPerKind(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	torrentProvider := &fakeProvider{listErr: fmt.Errorf("faketorbox: list: %w", debrid.ErrRateLimited)}
	usenetProvider := &fakeProvider{}
	im := New(db, torrentProvider, usenetProvider, t.TempDir(), time.Minute, 5)

	im.refreshKind(ctx, database.KindTorrent, torrentProvider, false)
	im.refreshKind(ctx, database.KindUsenet, usenetProvider, false)

	if _, cooling := im.RateLimitCooldownUntil(database.KindTorrent); !cooling {
		t.Error("expected torrent kind to be cooling")
	}
	if _, cooling := im.RateLimitCooldownUntil(database.KindUsenet); cooling {
		t.Error("usenet kind should not be affected by torrent's rate limit")
	}
}

// TestRefreshKind_RateLimit_ClearsOnSuccess proves a successful List() call
// clears any previous cooldown for that kind, rather than it lingering
// until it naturally expires — the next rate limit (if any) starts counting
// from scratch.
func TestRefreshKind_RateLimit_ClearsOnSuccess(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{listErr: fmt.Errorf("faketorbox: list: %w", debrid.ErrRateLimited)}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)

	im.refreshKind(ctx, database.KindTorrent, provider, false)
	if _, cooling := im.RateLimitCooldownUntil(database.KindTorrent); !cooling {
		t.Fatal("expected torrent kind to be cooling after the rate-limited call")
	}

	provider.listErr = nil
	// Directly clear the cooldown the way a real successful call inside
	// refreshKind would (rather than waiting out rateLimitBackoffBase in a
	// unit test) — refreshKind's own cooldown check would otherwise skip
	// the List() call entirely and never reach clearRateLimitHit.
	im.clearRateLimitHit(database.KindTorrent)
	im.refreshKind(ctx, database.KindTorrent, provider, false)

	if provider.listCalls.Load() != 2 {
		t.Errorf("List() called %d times, want 2 (cooldown cleared, so the second refreshKind actually called List)", provider.listCalls.Load())
	}
	if _, cooling := im.RateLimitCooldownUntil(database.KindTorrent); cooling {
		t.Error("expected no cooldown after a successful List() call")
	}
}

// TestRefreshActiveKind_OnlyChecksActiveManagedDownloads proves the fast
// path's scoping: it calls Status() (not List()) exactly once per
// queued/downloading Managed download, leaves a Manual download and an
// already-provider_completed Managed download alone, and applies whatever
// Status() reports through the same RefreshFromProvider logic the bulk path
// uses.
func TestRefreshActiveKind_OnlyChecksActiveManagedDownloads(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	managed := &database.Download{
		ID: "managed", Provider: "faketorbox", ProviderDownloadID: "managed-id",
		Kind: database.KindTorrent, Name: "Managed.Release", State: database.StateDownloading,
		AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, managed); err != nil {
		t.Fatalf("InsertDownload(managed) error = %v", err)
	}

	manual := &database.Download{
		ID: "manual", Provider: "faketorbox", ProviderDownloadID: "manual-id",
		Kind: database.KindTorrent, Name: "Manual.Release", State: database.StateDownloading,
		AddedVia: database.AddedViaManual,
	}
	if err := db.InsertDownload(ctx, manual); err != nil {
		t.Fatalf("InsertDownload(manual) error = %v", err)
	}

	alreadyDone := &database.Download{
		ID: "done", Provider: "faketorbox", ProviderDownloadID: "done-id",
		Kind: database.KindTorrent, Name: "Done.Release", State: database.StateProviderCompleted,
		AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, alreadyDone); err != nil {
		t.Fatalf("InsertDownload(alreadyDone) error = %v", err)
	}

	provider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "managed-id", Name: "Managed.Release", State: debrid.StateCompleted},
	}}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)

	im.refreshActiveKind(ctx, database.KindTorrent, provider)

	if provider.statusCalls.Load() != 1 {
		t.Errorf("Status() called %d times, want exactly 1 (only the active Managed row)", provider.statusCalls.Load())
	}
	if provider.listCalls.Load() != 0 {
		t.Errorf("List() called %d times, want 0 — the fast path must never call the bulk endpoint", provider.listCalls.Load())
	}

	got, err := db.GetDownloadByID(ctx, "managed")
	if err != nil {
		t.Fatalf("GetDownloadByID(managed) error = %v", err)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("managed download state = %q, want provider_completed", got.State)
	}
}

// TestRefreshActiveKind_RateLimit_SetsCooldownSharedWithBulkPath proves a
// rate limit hit on the fast path's Status() call sets the exact same
// per-kind cooldown refreshKind's List() path uses — the two paths back off
// together rather than fighting over independent budgets.
func TestRefreshActiveKind_RateLimit_SetsCooldownSharedWithBulkPath(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	active := &database.Download{
		ID: "managed", Provider: "faketorbox", ProviderDownloadID: "managed-id",
		Kind: database.KindTorrent, Name: "Managed.Release", State: database.StateDownloading,
		AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, active); err != nil {
		t.Fatalf("InsertDownload(active) error = %v", err)
	}

	provider := &fakeProvider{statusErr: fmt.Errorf("faketorbox: status: %w", debrid.ErrRateLimited)}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)

	im.refreshActiveKind(ctx, database.KindTorrent, provider)
	if _, cooling := im.RateLimitCooldownUntil(database.KindTorrent); !cooling {
		t.Fatal("expected torrent kind to be cooling after a rate-limited Status() call")
	}

	// The bulk path must see (and respect) the same cooldown state — it
	// should skip calling List() entirely rather than tripping the rate
	// limit a second time.
	im.refreshKind(ctx, database.KindTorrent, provider, false)
	if provider.listCalls.Load() != 0 {
		t.Errorf("List() called %d times, want 0 — bulk path should have skipped due to the fast path's cooldown", provider.listCalls.Load())
	}
}

// TestRefreshActiveKind_SkipsWhenCooling proves the fast path itself respects
// an existing cooldown (e.g. one the bulk path just set) rather than calling
// Status() anyway.
func TestRefreshActiveKind_SkipsWhenCooling(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	active := &database.Download{
		ID: "managed", Provider: "faketorbox", ProviderDownloadID: "managed-id",
		Kind: database.KindTorrent, Name: "Managed.Release", State: database.StateDownloading,
		AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, active); err != nil {
		t.Fatalf("InsertDownload(active) error = %v", err)
	}

	provider := &fakeProvider{}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	im.recordRateLimitHit(database.KindTorrent)

	im.refreshActiveKind(ctx, database.KindTorrent, provider)
	if provider.statusCalls.Load() != 0 {
		t.Errorf("Status() called %d times, want 0 — already cooling down", provider.statusCalls.Load())
	}
}

// TestRateLimitBackoffDuration_GrowsAndCaps proves the backoff formula
// itself: the first hit gets exactly rateLimitBackoffBase (not double it),
// it doubles each consecutive hit, and it never exceeds rateLimitBackoffMax
// no matter how many consecutive hits are recorded.
func TestRateLimitBackoffDuration_GrowsAndCaps(t *testing.T) {
	cases := []struct {
		hits int
		want time.Duration
	}{
		{1, rateLimitBackoffBase},
		{2, rateLimitBackoffBase * 2},
		{3, rateLimitBackoffBase * 4},
	}
	for _, c := range cases {
		if got := rateLimitBackoffDuration(c.hits); got != c.want {
			t.Errorf("rateLimitBackoffDuration(%d) = %v, want %v", c.hits, got, c.want)
		}
	}
	if got := rateLimitBackoffDuration(50); got != rateLimitBackoffMax {
		t.Errorf("rateLimitBackoffDuration(50) = %v, want capped at %v", got, rateLimitBackoffMax)
	}
}

// TestDiscoverManual_FirstRunSeedsBaselineWithoutAdopting proves the very
// first tick that sees unmatched provider items never adopts them — it only
// records them as a permanent baseline to ignore (see
// database.SeedDiscoveryBaseline), which is what stops shipping this feature
// from flooding the Manual tab with an account's entire pre-existing history.
// TestDiscoverManual_SeedsEvenWithNothingUntrackedYet proves a provider+kind
// with zero unmatched items on its first tick still gets marked seeded —
// not just skipped — so a genuinely new item that shows up later is
// correctly adopted instead of being wrongly absorbed into "pre-existing"
// (which is what would happen if seeding were gated on there being
// something to seed at that first-ever check).
func TestDiscoverManual_SeedsEvenWithNothingUntrackedYet(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{} // nothing tracked, nothing at the provider either
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 1 error = %v", err)
	}

	seeded, err := db.IsDiscoveryBaselineSeeded(ctx, "faketorbox", database.KindTorrent)
	if err != nil {
		t.Fatalf("IsDiscoveryBaselineSeeded() error = %v", err)
	}
	if !seeded {
		t.Fatal("IsDiscoveryBaselineSeeded() = false after a first tick with nothing untracked, want true")
	}

	// A genuinely new item shows up on a later tick — must be adopted, not
	// silently ignored as if it were part of the (empty) initial baseline.
	provider.statuses = []debrid.DownloadStatus{
		{ID: "genuinely-new", Name: "Genuinely New Torrent", State: debrid.StateDownloading, Progress: 0.1},
	}
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 2 error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 || rows[0].ProviderDownloadID != "genuinely-new" {
		t.Errorf("downloads after second tick = %+v, want the new item adopted", rows)
	}
}

// TestDiscoverManual_FreshInstall_AdoptsPreExistingItemsImmediately proves
// a genuinely fresh install (an empty database, nothing tracked at all yet)
// adopts whatever's already sitting in the provider account on the very
// first tick, rather than baselining it away forever — found live: a fresh
// Proxmox install recognized the configured TorBox account but never showed
// its existing downloads, because the original version of this always took
// the conservative "established instance" branch (see
// TestDiscoverManual_EstablishedInstance_FirstRunSeedsBaselineWithoutAdopting
// for that branch, still exercised for the case it's actually meant for).
func TestDiscoverManual_FreshInstall_AdoptsPreExistingItemsImmediately(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "already-in-torbox-1", Name: "Pre-existing Torrent", State: debrid.StateCompleted, SizeBytes: 123},
	}}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 || rows[0].ProviderDownloadID != "already-in-torbox-1" {
		t.Fatalf("downloads after first tick = %+v, want the pre-existing item adopted", rows)
	}
	if rows[0].AddedVia != database.AddedViaManual {
		t.Errorf("adopted AddedVia = %q, want manual", rows[0].AddedVia)
	}

	seeded, err := db.IsDiscoveryBaselineSeeded(ctx, "faketorbox", database.KindTorrent)
	if err != nil {
		t.Fatalf("IsDiscoveryBaselineSeeded() error = %v", err)
	}
	if !seeded {
		t.Error("IsDiscoveryBaselineSeeded() = false, want true after the first tick")
	}

	baseline, err := db.DiscoveryBaseline(ctx, "faketorbox", database.KindTorrent)
	if err != nil {
		t.Fatalf("DiscoveryBaseline() error = %v", err)
	}
	if len(baseline) != 0 {
		t.Errorf("DiscoveryBaseline() = %v, want empty — a fresh install has nothing to permanently ignore", baseline)
	}
}

// TestDiscoverManual_FreshInstall_AppliesConsistentlyAcrossKindsInSameTick
// proves torrent and usenet both get their pre-existing items adopted on
// the same first tick, not just whichever kind refreshStatuses happens to
// process first. freshInstall is computed once per tick from whether the
// database had any tracked download at all before the tick started — if it
// were instead re-checked fresh inside each kind's own pass, torrent
// adopting its item first would make the database non-empty by the time
// usenet's own check ran, wrongly baselining usenet's pre-existing item
// even though both kinds are equally part of the same fresh install.
func TestDiscoverManual_FreshInstall_AppliesConsistentlyAcrossKindsInSameTick(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	torrentProvider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "pre-existing-torrent", Name: "Pre-existing Torrent", State: debrid.StateCompleted},
	}}
	usenetProvider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "pre-existing-usenet", Name: "Pre-existing Usenet", State: debrid.StateCompleted},
	}}
	im := New(db, torrentProvider, usenetProvider, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	torrentRows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads(torrent) error = %v", err)
	}
	if len(torrentRows) != 1 || torrentRows[0].ProviderDownloadID != "pre-existing-torrent" {
		t.Errorf("torrent downloads = %+v, want pre-existing-torrent adopted", torrentRows)
	}

	usenetRows, err := db.ListDownloads(ctx, database.KindUsenet)
	if err != nil {
		t.Fatalf("ListDownloads(usenet) error = %v", err)
	}
	if len(usenetRows) != 1 || usenetRows[0].ProviderDownloadID != "pre-existing-usenet" {
		t.Errorf("usenet downloads = %+v, want pre-existing-usenet adopted too, not baselined away", usenetRows)
	}
}

// insertUnrelatedDownload seeds one throwaway tracked row so
// database.HasAnyDownloads reports true — the signal discoverManual uses to
// tell a genuinely fresh install apart from an established instance seeing
// a particular provider+kind for the first time (a newly added second
// provider, or this feature itself landing on an existing instance). Always
// a different kind than whatever the test is actually exercising, so it
// doesn't show up in that kind's own ListDownloads counts.
func insertUnrelatedDownload(t *testing.T, ctx context.Context, db *database.DB) {
	t.Helper()
	if err := db.InsertDownload(ctx, &database.Download{
		ID: "unrelated-existing-row", Provider: "faketorbox", ProviderDownloadID: "unrelated-provider-id",
		Kind: database.KindUsenet, Hash: "unrelatedhash", Name: "Unrelated Pre-existing Download",
		State: database.StateReadyForImport, AddedVia: database.AddedViaArr,
	}); err != nil {
		t.Fatalf("insertUnrelatedDownload: InsertDownload() error = %v", err)
	}
}

// TestDiscoverManual_EstablishedInstance_FirstRunSeedsBaselineWithoutAdopting
// proves an instance that already has real tracked history — not a fresh
// install — still takes the conservative branch the first time it sees a
// particular provider+kind in discovery: nothing adopted, everything
// currently unmatched recorded as a permanent baseline instead. This is
// what stops this feature (or a newly added second provider) from flooding
// an established instance's Manual tab with a big pre-existing history —
// contrasted with TestDiscoverManual_FreshInstall_AdoptsPreExistingItemsImmediately,
// where the same "nothing tracked for this provider+kind yet" starting
// point instead adopts everything, because the instance itself is fresh.
func TestDiscoverManual_EstablishedInstance_FirstRunSeedsBaselineWithoutAdopting(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	insertUnrelatedDownload(t, ctx, db)

	provider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "already-in-torbox-1", Name: "Pre-existing Torrent", State: debrid.StateCompleted, SizeBytes: 123},
	}}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("torrent downloads after first tick = %d, want 0 (nothing adopted on the seeding run)", len(rows))
	}

	seeded, err := db.IsDiscoveryBaselineSeeded(ctx, "faketorbox", database.KindTorrent)
	if err != nil {
		t.Fatalf("IsDiscoveryBaselineSeeded() error = %v", err)
	}
	if !seeded {
		t.Error("IsDiscoveryBaselineSeeded() = false, want true after the first tick")
	}

	baseline, err := db.DiscoveryBaseline(ctx, "faketorbox", database.KindTorrent)
	if err != nil {
		t.Fatalf("DiscoveryBaseline() error = %v", err)
	}
	if !baseline["already-in-torbox-1"] {
		t.Error("DiscoveryBaseline() doesn't contain the pre-existing item, want it recorded")
	}
}

// TestDiscoverManual_AdoptsItemsThatAppearAfterBaselineSeeded proves an item
// present on a later tick, but absent from the baseline recorded on the
// first tick, gets adopted as a manual download — the actual "show up in
// Manual" behavior, contrasted with the pre-existing item from
// TestDiscoverManual_EstablishedInstance_FirstRunSeedsBaselineWithoutAdopting,
// which never is. Uses an established (not fresh) instance so that first
// tick actually takes the baseline-without-adopting branch this test means
// to build on.
func TestDiscoverManual_AdoptsItemsThatAppearAfterBaselineSeeded(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	insertUnrelatedDownload(t, ctx, db)

	provider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "already-in-torbox-1", Name: "Pre-existing Torrent", State: debrid.StateCompleted, SizeBytes: 123},
	}}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil { // seeds the baseline, adopts nothing
		t.Fatalf("Tick() 1 error = %v", err)
	}

	// A new item shows up alongside the pre-existing (baselined) one.
	provider.statuses = append(provider.statuses, debrid.DownloadStatus{
		ID: "newly-added-2", Name: "New Manual Torrent", Hash: "NEWHASH", State: debrid.StateDownloading, Progress: 0.25, SizeBytes: 456,
	})
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 2 error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("downloads after second tick = %d, want 1 (only the new item adopted)", len(rows))
	}
	got := rows[0]
	if got.ProviderDownloadID != "newly-added-2" {
		t.Errorf("adopted ProviderDownloadID = %q, want newly-added-2", got.ProviderDownloadID)
	}
	if got.AddedVia != database.AddedViaManual {
		t.Errorf("adopted AddedVia = %q, want manual", got.AddedVia)
	}
	if got.Hash != "newhash" {
		t.Errorf("adopted Hash = %q, want lowercased newhash", got.Hash)
	}
	if got.Name != "New Manual Torrent" || got.SizeBytes != 456 || got.Progress != 0.25 {
		t.Errorf("adopted row = %+v, want it to reflect the provider's status", got)
	}
	if got.State != database.StateDownloading {
		t.Errorf("adopted State = %q, want downloading", got.State)
	}

	// A third tick with nothing new shouldn't duplicate the now-tracked item.
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 3 error = %v", err)
	}
	rows, err = db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("downloads after third tick = %d, want still 1 (no duplicate adoption)", len(rows))
	}
}

// TestDiscoverManual_SetsSourceFromOriginalURL proves a newly discovered
// download's Source is captured immediately from the provider's
// OriginalURL, if it has one — what lets Re-add work for a discovered
// download without waiting for a later RefreshFromProvider backfill tick
// (see database.BackfillSource, which covers a row already tracked before
// the provider happened to report one). Empty OriginalURL (e.g. a
// file-upload-based usenet add) correctly leaves Source empty too.
func TestDiscoverManual_SetsSourceFromOriginalURL(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{statuses: []debrid.DownloadStatus{
		{ID: "already-tracked-1", Name: "Pre-existing", State: debrid.StateCompleted},
	}}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	// Primes discovery for this provider+kind (fresh install, so this also
	// adopts already-tracked-1 — irrelevant here, only tick 2's own
	// additions are checked below).
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 1 error = %v", err)
	}

	provider.statuses = append(provider.statuses,
		debrid.DownloadStatus{ID: "with-url", Name: "Has A Source", State: debrid.StateDownloading, OriginalURL: "magnet:?xt=urn:btih:abc123"},
		debrid.DownloadStatus{ID: "without-url", Name: "No Source", State: debrid.StateDownloading},
	)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 2 error = %v", err)
	}

	withURL, err := db.GetDownloadByProviderID(ctx, "faketorbox", "with-url")
	if err != nil {
		t.Fatalf("GetDownloadByProviderID(with-url) error = %v", err)
	}
	if withURL == nil || withURL.Source != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("with-url Source = %+v, want the OriginalURL", withURL)
	}

	withoutURL, err := db.GetDownloadByProviderID(ctx, "faketorbox", "without-url")
	if err != nil {
		t.Fatalf("GetDownloadByProviderID(without-url) error = %v", err)
	}
	if withoutURL == nil || withoutURL.Source != "" {
		t.Errorf("without-url Source = %+v, want empty", withoutURL)
	}
}

// TestDiscoverManual_SkipsRecentlyDeletedDownload proves a provider item
// that's still technically present in a listing right after being
// intentionally deleted (a real, observed race — the provider's own delete
// isn't always instantly reflected in its listing endpoints) doesn't get
// re-adopted as a fresh "discovered" ghost — see
// database.RecordDeletedDownload/RecentlyDeletedDownloads, which
// internal/api's handleDeleteDownload records into on every real delete.
func TestDiscoverManual_SkipsRecentlyDeletedDownload(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{}
	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil { // seeds the (empty) baseline
		t.Fatalf("Tick() 1 error = %v", err)
	}

	if err := db.RecordDeletedDownload(ctx, "faketorbox", database.KindTorrent, "just-deleted"); err != nil {
		t.Fatalf("RecordDeletedDownload() error = %v", err)
	}

	// The provider still reports it — simulating the provider's own
	// delete-propagation lag.
	provider.statuses = []debrid.DownloadStatus{
		{ID: "just-deleted", Name: "Should Not Be Re-adopted", State: debrid.StateCompleted},
	}
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 2 error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("downloads after tick = %+v, want none (recently-deleted item must not be re-adopted)", rows)
	}
}

// TestTick_NeverAutoFetchesManualDownloads proves an AddedViaManual download
// sitting in provider_completed is never picked up by Completed Download
// Handling's fetch step, regardless of how long it's been there — Manual
// downloads are meant to behave like TorBox's own web UI (grab files on
// demand), not get silently written to local disk the way an *arr-added
// download does. RequestDownloadLink being called at all would mean a fetch
// was attempted.
func TestTick_NeverAutoFetchesManualDownloads(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{
		files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "movie.mkv", SizeBytes: 1024}},
	}

	d := &database.Download{
		ID: "dl-manual-1", Provider: "fake", ProviderDownloadID: "provider-manual-1", Kind: database.KindTorrent,
		Hash: "manualhash", Name: "Manually Added Movie", SavePath: t.TempDir(),
		State: database.StateProviderCompleted, AddedVia: database.AddedViaManual,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if n := provider.requestedCount(); n != 0 {
		t.Errorf("RequestDownloadLink called %d times, want 0 (manual downloads are never auto-fetched)", n)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("state = %q, want it to stay provider_completed (never auto-fetched)", got.State)
	}
}

// TestTick_ToleratesNoProviderConfiguredDuringRefresh proves a
// debrid.ErrNoProvider from List() (e.g. no TorBox key set yet) doesn't fail
// the tick — it's a routine, expected state, not an error worth surfacing on
// every single tick.
func TestTick_ToleratesNoProviderConfiguredDuringRefresh(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{listErr: debrid.ErrNoProvider}
	d := &database.Download{
		ID: "dl-8", Provider: "fake", ProviderDownloadID: "provider-8", Kind: database.KindTorrent,
		Hash: "noprovider1", Name: "No Provider Yet", SavePath: t.TempDir(), State: database.StateQueued,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v, want nil even when the provider isn't configured yet", err)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	db := openTestDB(t)
	im := New(db, &fakeProvider{}, nil, t.TempDir(), time.Hour, 5) // long interval — we only care that cancel stops it

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		im.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

// TestRun_FastPollRunsIndependentlyOfBulkInterval proves runFastPoll is
// actually wired into Run(): with the bulk interval set far longer than
// fastPollInterval, an active Managed download's Status() still gets called
// on its own, faster cadence — the real-world bug this whole mechanism
// exists to fix (see fastPollInterval's doc comment).
func TestRun_FastPollRunsIndependentlyOfBulkInterval(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	active := &database.Download{
		ID: "managed", Provider: "faketorbox", ProviderDownloadID: "managed-id",
		Kind: database.KindTorrent, Name: "Managed.Release", State: database.StateDownloading,
		AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(ctx, active); err != nil {
		t.Fatalf("InsertDownload(active) error = %v", err)
	}

	provider := &fakeProvider{}
	// A bulk interval much longer than fastPollInterval — if runFastPoll
	// weren't wired in at all, statusCalls would still be 0 well after
	// fastPollInterval has elapsed.
	im := New(db, provider, nil, t.TempDir(), time.Hour, 5)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go im.Run(runCtx)

	deadline := time.After(fastPollInterval + 2*time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if provider.statusCalls.Load() > 0 {
				return // fast poll fired — success
			}
		case <-deadline:
			t.Fatalf("Status() was never called within %v of Run() starting — fast poll isn't wired in", fastPollInterval+2*time.Second)
		}
	}
}

// TestSetConfig_DownloadDirAppliesLive proves a downloadDir change from
// SetConfig takes effect on the very next Tick, with no restart.
func TestSetConfig_DownloadDirAppliesLive(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("episode bytes"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn:   cdn,
		files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: int64(len("episode bytes"))}},
	}

	d := &database.Download{
		ID: "dl-setconfig", Provider: "fake", ProviderDownloadID: "provider-setconfig", Kind: database.KindTorrent,
		Hash: "setconfig1", Name: "Some Release", Category: "tv", State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)

	newDownloadDir := t.TempDir()
	im.SetConfig(newDownloadDir, time.Minute, 5)

	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := filepath.Join(newDownloadDir, "tv", "Some Release", "episode.mkv")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected file at the new downloadDir %s, stat error = %v", want, err)
	}
}

// TestSetCategoryPaths_OverridesDownloadDir proves a category with a
// configured override directory lands there instead of under
// downloadDir/<category>, and that a category with no override still falls
// back to the downloadDir/<category> behavior.
func TestSetCategoryPaths_OverridesDownloadDir(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("movie bytes"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn:   cdn,
		files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "movie.mkv", SizeBytes: int64(len("movie bytes"))}},
	}

	d := &database.Download{
		ID: "dl-catpath", Provider: "fake", ProviderDownloadID: "provider-catpath", Kind: database.KindTorrent,
		Hash: "catpath1", Name: "Some Movie", Category: "movies", State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	downloadDir := t.TempDir()
	moviesOverride := t.TempDir()
	im := New(db, provider, nil, downloadDir, time.Minute, 5)
	im.SetCategoryPaths(map[string]string{"movies": moviesOverride})

	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := filepath.Join(moviesOverride, "Some Movie", "movie.mkv")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected file at the category override dir %s, stat error = %v", want, err)
	}
	notWant := filepath.Join(downloadDir, "movies", "Some Movie", "movie.mkv")
	if _, err := os.Stat(notWant); err == nil {
		t.Errorf("file also landed at the unused default location %s, want only the override", notWant)
	}
}

// TestTick_RespectsMaxConcurrent proves Tick actually runs up to
// SetMaxConcurrent downloads' fetches in parallel (not a coincidence of
// timing — the test blocks the CDN handler until it observes 2 requests in
// flight simultaneously before releasing any of them), and never more than
// that at once, across 4 downloads due at the same time.
func TestTick_RespectsMaxConcurrent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	var current, peak atomic.Int32
	release := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := current.Add(1)
		defer current.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		w.Write([]byte("bytes"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{
		cdn:   cdn,
		files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "file.mkv", SizeBytes: int64(len("bytes"))}},
	}

	const numDownloads = 4
	for i := 0; i < numDownloads; i++ {
		d := &database.Download{
			ID: fmt.Sprintf("dl-conc-%d", i), Provider: "fake", ProviderDownloadID: fmt.Sprintf("provider-conc-%d", i),
			Kind: database.KindTorrent, Hash: fmt.Sprintf("conchash%d", i), Name: fmt.Sprintf("Release %d", i),
			Category: "tv", State: database.StateProviderCompleted,
		}
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	im.SetMaxConcurrent(2)
	if got := im.MaxConcurrent(); got != 2 {
		t.Fatalf("MaxConcurrent() = %d, want 2", got)
	}

	done := make(chan error, 1)
	go func() { done <- im.Tick(ctx) }()

	deadline := time.After(2 * time.Second)
waitForConcurrency:
	for {
		select {
		case <-deadline:
			t.Fatal("never observed 2 concurrent in-flight fetches")
		default:
			if current.Load() >= 2 {
				break waitForConcurrency
			}
			time.Sleep(time.Millisecond)
		}
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if got := peak.Load(); got > 2 {
		t.Errorf("peak concurrent fetches = %d, want <= 2 (maxConcurrent)", got)
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("peak concurrent fetches = %d, want >= 2 (proves real parallelism happened)", got)
	}
}

// TestFetchFile_TimesOutOnSlowTransfer proves SetFetchTimeout is actually
// enforced per-request: a CDN that never finishes responding causes the
// fetch to fail (and the download to be scheduled for retry) once the
// configured timeout elapses, rather than hanging indefinitely.
func TestFetchFile_TimesOutOnSlowTransfer(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	blockForever := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockForever // never responds within the test's lifetime
	}))
	// Cleanups run LIFO: unblock the handler (registered second, so it runs
	// first) before cdn.Close (registered first, runs last) waits for that
	// same handler goroutine to return — otherwise Close blocks forever.
	t.Cleanup(cdn.Close)
	t.Cleanup(func() { close(blockForever) })

	provider := &fakeProvider{
		cdn:   cdn,
		files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "file.mkv", SizeBytes: 5}},
	}

	d := &database.Download{
		ID: "dl-timeout", Provider: "fake", ProviderDownloadID: "provider-timeout", Kind: database.KindTorrent,
		Hash: "timeouthash", Name: "Slow Release", Category: "tv", State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Minute, 5)
	im.SetFetchTimeout(50 * time.Millisecond)
	if got := im.FetchTimeout(); got != 50*time.Millisecond {
		t.Fatalf("FetchTimeout() = %v, want 50ms", got)
	}

	start := time.Now()
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Tick() took %v, want it to give up around the 50ms fetch timeout, not hang", elapsed)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("state = %q, want still provider_completed (scheduled for retry, not marked ready)", got.State)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1 (one failed attempt recorded)", got.RetryCount)
	}
}

// TestSetConfig_MaxRetriesAppliesLive proves a maxRetries change from
// SetConfig takes effect on the very next handleFailure call.
func TestSetConfig_MaxRetriesAppliesLive(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := &database.Download{
		ID: "dl-setconfig-retries", Provider: "fake", ProviderDownloadID: "provider-setconfig-retries", Kind: database.KindTorrent,
		Hash: "setconfig2", Name: "Retry Test", SavePath: t.TempDir(), State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, nil, nil, t.TempDir(), time.Minute, 5)
	im.SetConfig(t.TempDir(), time.Minute, 1) // lower maxRetries to 1: the very next failure should give up

	im.handleFailure(ctx, d, errors.New("simulated failure"))

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != database.StateError {
		t.Errorf("state = %q, want error (new maxRetries=1 should give up on the first failure)", got.State)
	}
	// RetryCount must be > 0 even in this maxRetries=1 edge case — it's how
	// database.RefreshFromProvider tells this give-up apart from a
	// provider-reported error and knows not to silently resurrect it (see
	// TestRefreshFromProvider_DoesNotResurrectImporterGaveUp).
	if got.RetryCount == 0 {
		t.Error("RetryCount = 0, want > 0 even when maxRetries=1 triggers give-up on the very first attempt")
	}
}

// TestSetConfig_ResetsTickerInterval proves a running Importer's ticker
// actually resets to a new interval immediately rather than waiting out
// whatever's left of the old one — starts with a long interval, then shrinks
// it drastically and expects a tick well within the old interval's window.
func TestSetConfig_ResetsTickerInterval(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	provider := &fakeProvider{}

	// refreshKind (called from every Tick) skips calling List entirely when
	// nothing's tracked — a row needs to exist for a tick to be observable
	// via provider.listCalls at all.
	d := &database.Download{
		ID: "dl-ticker-reset", Provider: "fake", ProviderDownloadID: "provider-ticker-reset", Kind: database.KindTorrent,
		Hash: "tickerreset1", Name: "Ticker Reset Test", State: database.StateQueued,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	im := New(db, provider, nil, t.TempDir(), time.Hour, 5) // old interval: would never fire in this test's timeout

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go im.Run(runCtx)

	im.SetConfig(t.TempDir(), 20*time.Millisecond, 5)

	deadline := time.After(2 * time.Second)
	for {
		if provider.listCalls.Load() > 0 {
			return // a tick fired — the reset took effect
		}
		select {
		case <-deadline:
			t.Fatal("ticker did not reset to the new (much shorter) interval in time")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestSetWebDownloadProvider_NilSkipsWebDLKindEntirely proves refreshKind's
// nil check applies to the web-download provider exactly like it already
// does for torrentProvider/usenetProvider — no provider set yet means Tick
// doesn't touch KindWebDL at all (nothing to List against, nothing to
// discover), rather than erroring.
func TestSetWebDownloadProvider_NilSkipsWebDLKindEntirely(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	im := New(db, &fakeProvider{}, nil, t.TempDir(), time.Minute, 5)

	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindWebDL)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("KindWebDL downloads = %d, want 0 (no web-download provider configured)", len(rows))
	}
}

// TestSetWebDownloadProvider_DiscoversManualWebDownloads proves
// SetWebDownloadProvider wires a web-download-capable provider into
// refreshStatuses' third refreshKind call — a hoster link that shows up in
// the provider's List() after the baseline is seeded gets adopted as a
// KindWebDL/AddedViaManual download, the same discovery flow already proven
// for torrent/usenet in TestDiscoverManual_AdoptsItemsThatAppearAfterBaselineSeeded.
func TestSetWebDownloadProvider_DiscoversManualWebDownloads(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	provider := &fakeProvider{}
	im := New(db, &fakeProvider{}, nil, t.TempDir(), time.Minute, 5)
	im.SetWebDownloadProvider(provider)

	if err := im.Tick(ctx); err != nil { // seeds the baseline, adopts nothing
		t.Fatalf("Tick() 1 error = %v", err)
	}

	provider.statuses = append(provider.statuses, debrid.DownloadStatus{
		ID: "webdl-1", Name: "Dragon Ball Z", Hash: "WEBHASH", State: debrid.StateCompleted, SizeBytes: 2048,
	})
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() 2 error = %v", err)
	}

	rows, err := db.ListDownloads(ctx, database.KindWebDL)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("KindWebDL downloads = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ProviderDownloadID != "webdl-1" || got.AddedVia != database.AddedViaManual {
		t.Errorf("adopted row = %+v, want provider_download_id=webdl-1 added_via=manual", got)
	}
	if got.Name != "Dragon Ball Z" || got.SizeBytes != 2048 {
		t.Errorf("adopted row = %+v, want it to reflect the provider's status", got)
	}
}
