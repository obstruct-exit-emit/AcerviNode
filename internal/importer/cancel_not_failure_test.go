package importer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

func cancelTestDownload(t *testing.T, db *database.DB, id string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: "faketorbox", ProviderDownloadID: "p-" + id, Kind: database.KindTorrent,
		Hash: "h-" + id, Name: "Cancel " + id, Category: "tv", SavePath: t.TempDir() + "/Cancel " + id,
		State: database.StateProviderCompleted, SizeBytes: 20,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	return d
}

// TestTick_DeliberateCancelIsNotRecordedAsAFailure.
//
// CancelFetch is only ever called because a download is being removed. Once
// both compat shims began cancelling before an *arr delete (which is where
// mid-fetch removals actually come from), every one of those went through
// handleFailure: a retry was recorded and "process download failed, will
// retry" was logged at WARN for a download that no longer existed. Seen live on
// the first real test of that fix. A removal is not a failure, and "count the
// failures in the log" only works as a health signal if it counts failures.
//
// Checking whether the row still exists would be racy — CancelFetch unblocks
// the deleting caller before handleFailure runs, so the two race, which is
// exactly why the live run logged "will retry" against a row about to vanish.
// The cancellation carries its own reason instead.
func TestTick_DeliberateCancelIsNotRecordedAsAFailure(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	inFlight := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "20")
		w.Write(make([]byte, 5))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(inFlight)
		<-r.Context().Done() // hold the transfer open until the client gives up
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "file.bin", SizeBytes: 20}}}
	d := cancelTestDownload(t, db, "cancelled")
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	done := make(chan error, 1)
	go func() { done <- im.Tick(ctx) }()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch never started")
	}
	im.CancelFetch(d.ID)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Tick did not return after the fetch was cancelled")
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0: a deliberate cancel was recorded as a failed attempt", got.RetryCount)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("State = %q, want it left as it was", got.State)
	}
}

// TestTick_StalledFetchStillRetries is the regression this must not cause. The
// idle-stall timeout also ends a fetch with context.Canceled, so "ignore
// cancellation" would quietly stop stalls from ever retrying. A stall has to
// keep counting as a failure.
func TestTick_StalledFetchStillRetries(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "20")
		w.Write(make([]byte, 5))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // stall: headers and a few bytes, then silence
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{{ProviderFileID: "1", Path: "file.bin", SizeBytes: 20}}}
	d := cancelTestDownload(t, db, "stalled")
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)
	im.SetFetchTimeout(50 * time.Millisecond)

	if err := im.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1: a stall must still be recorded as a failed attempt", got.RetryCount)
	}
}
