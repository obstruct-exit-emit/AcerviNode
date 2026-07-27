package importer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// fakeProvider is a minimal fileResolver backed by an httptest.Server
// standing in for a debrid CDN — real HTTP round trips, no network.
type fakeProvider struct {
	cdn         *httptest.Server
	files       []debrid.DownloadFile
	failLinks   map[string]bool // fileID -> force RequestDownloadLink to fail
	requestedAt []string        // fileIDs RequestDownloadLink was called for, in order
}

func (f *fakeProvider) Files(_ context.Context, _ debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	return f.files, nil
}

func (f *fakeProvider) RequestDownloadLink(_ context.Context, _ debrid.ProviderDownloadID, fileID string) (string, error) {
	f.requestedAt = append(f.requestedAt, fileID)
	if f.failLinks[fileID] {
		return "", errors.New("provider: link resolution failed")
	}
	return f.cdn.URL + "/" + fileID, nil
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

	im := New(db, provider, nil, t.TempDir())
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

	im := New(db, nil, provider, downloadDir)
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

	im := New(db, provider, nil, t.TempDir())
	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(provider.requestedAt) != 0 {
		t.Errorf("RequestDownloadLink called for already-present file: %v", provider.requestedAt)
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

	im := New(db, provider, nil, t.TempDir())
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

	im := New(db, provider, nil, t.TempDir())
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

func TestRun_StopsOnContextCancel(t *testing.T) {
	db := openTestDB(t)
	im := New(db, &fakeProvider{}, nil, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		im.Run(ctx, time.Hour) // long interval — we only care that cancel stops it
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}
