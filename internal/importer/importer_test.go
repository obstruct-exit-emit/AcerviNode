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

	// requestedAtMu guards requestedAt: TestTick_RespectsMaxConcurrent runs
	// multiple downloads' RequestDownloadLink calls concurrently against the
	// same shared fakeProvider, which a plain unsynchronized append can't
	// survive.
	requestedAtMu sync.Mutex
	requestedAt   []string // fileIDs RequestDownloadLink was called for, in order
}

func (f *fakeProvider) Name() string { return "faketorbox" }

func (f *fakeProvider) List(_ context.Context) ([]debrid.DownloadStatus, error) {
	f.listCalls.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.statuses, nil
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

func TestDiscoverManual_FirstRunSeedsBaselineWithoutAdopting(t *testing.T) {
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
	if len(rows) != 0 {
		t.Fatalf("downloads after first tick = %d, want 0 (nothing adopted on the seeding run)", len(rows))
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
// TestDiscoverManual_FirstRunSeedsBaselineWithoutAdopting, which never is.
func TestDiscoverManual_AdoptsItemsThatAppearAfterBaselineSeeded(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

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
