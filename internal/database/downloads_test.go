package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/debrid"
)

func newTestDownload(kind Kind) *Download {
	return &Download{
		ID:                 uuid.NewString(),
		Provider:           "torbox",
		ProviderDownloadID: "provider-123",
		Kind:               kind,
		Hash:               "abc123",
		Name:               "Some.Release.Name",
		Category:           "tv-sonarr",
		SavePath:           "/downloads/tv-sonarr",
		SizeBytes:          1024,
		State:              "queued",
		Progress:           0,
	}
}

func TestInsertAndGetDownload(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetDownloadByID() = nil, want a row")
	}
	if got.Name != d.Name || got.Provider != d.Provider || got.Kind != KindTorrent {
		t.Errorf("GetDownloadByID() = %+v, want match for %+v", got, d)
	}

	byHash, err := db.GetDownloadByHash(ctx, d.Hash)
	if err != nil {
		t.Fatalf("GetDownloadByHash() error = %v", err)
	}
	if byHash == nil || byHash.ID != d.ID {
		t.Errorf("GetDownloadByHash() = %+v, want id %s", byHash, d.ID)
	}
}

func TestGetDownloadByID_NotFound(t *testing.T) {
	db := openTestDB(t)
	got, err := db.GetDownloadByID(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got != nil {
		t.Errorf("GetDownloadByID() = %+v, want nil", got)
	}
}

func TestListDownloads_FiltersByKind(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	torrent := newTestDownload(KindTorrent)
	usenet := newTestDownload(KindUsenet)
	usenet.Hash = "" // usenet rows have no infohash
	usenet.ProviderDownloadID = "provider-456"

	if err := db.InsertDownload(ctx, torrent); err != nil {
		t.Fatalf("InsertDownload(torrent) error = %v", err)
	}
	if err := db.InsertDownload(ctx, usenet); err != nil {
		t.Fatalf("InsertDownload(usenet) error = %v", err)
	}

	torrents, err := db.ListDownloads(ctx, KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads(torrent) error = %v", err)
	}
	if len(torrents) != 1 || torrents[0].ID != torrent.ID {
		t.Errorf("ListDownloads(torrent) = %+v, want only %s", torrents, torrent.ID)
	}

	usenetDownloads, err := db.ListDownloads(ctx, KindUsenet)
	if err != nil {
		t.Fatalf("ListDownloads(usenet) error = %v", err)
	}
	if len(usenetDownloads) != 1 || usenetDownloads[0].ID != usenet.ID {
		t.Errorf("ListDownloads(usenet) = %+v, want only %s", usenetDownloads, usenet.ID)
	}

	all, err := db.ListAllDownloads(ctx)
	if err != nil {
		t.Fatalf("ListAllDownloads() error = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListAllDownloads() = %d rows, want 2 (both kinds)", len(all))
	}
}

func TestUpdateDownloadStatus(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	completedAt := time.Now().UTC().Truncate(time.Second)
	if err := db.UpdateDownloadStatus(ctx, d.ID, "ready_for_import", 1.0, 2048, &completedAt, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != "ready_for_import" || got.Progress != 1.0 {
		t.Errorf("got state=%q progress=%v, want ready_for_import/1.0", got.State, got.Progress)
	}
	if got.SizeBytes != 2048 {
		t.Errorf("got SizeBytes=%d, want 2048 (backfilled from provider)", got.SizeBytes)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Errorf("got CompletedAt=%v, want %v", got.CompletedAt, completedAt)
	}
}

func TestUpdateDownloadStatus_UnknownID(t *testing.T) {
	db := openTestDB(t)
	err := db.UpdateDownloadStatus(context.Background(), "does-not-exist", "error", 0, 0, nil, "boom")
	if err == nil {
		t.Error("UpdateDownloadStatus() expected error for unknown id, got nil")
	}
}

func TestDeleteDownload_CascadesFiles(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	files := []*DownloadFile{{ID: uuid.NewString(), Path: "movie.mkv", SizeBytes: 42}}
	if err := db.ReplaceDownloadFiles(ctx, d.ID, files); err != nil {
		t.Fatalf("ReplaceDownloadFiles() error = %v", err)
	}

	if err := db.DeleteDownload(ctx, d.ID); err != nil {
		t.Fatalf("DeleteDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got != nil {
		t.Errorf("GetDownloadByID() after delete = %+v, want nil", got)
	}

	remaining, err := db.ListDownloadFiles(ctx, d.ID)
	if err != nil {
		t.Fatalf("ListDownloadFiles() error = %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("ListDownloadFiles() after delete = %+v, want none (cascade)", remaining)
	}
}

func TestReplaceDownloadFiles_ReplacesFullSet(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	first := []*DownloadFile{{ID: uuid.NewString(), Path: "a.mkv", SizeBytes: 1}}
	if err := db.ReplaceDownloadFiles(ctx, d.ID, first); err != nil {
		t.Fatalf("ReplaceDownloadFiles(first) error = %v", err)
	}

	second := []*DownloadFile{
		{ID: uuid.NewString(), Path: "b.mkv", SizeBytes: 2},
		{ID: uuid.NewString(), Path: "c.mkv", SizeBytes: 3},
	}
	if err := db.ReplaceDownloadFiles(ctx, d.ID, second); err != nil {
		t.Fatalf("ReplaceDownloadFiles(second) error = %v", err)
	}

	got, err := db.ListDownloadFiles(ctx, d.ID)
	if err != nil {
		t.Fatalf("ListDownloadFiles() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListDownloadFiles() = %d files, want 2 (old set replaced)", len(got))
	}
}

func TestSetDownloadFileURL(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	file := &DownloadFile{ID: uuid.NewString(), Path: "movie.mkv", SizeBytes: 42}
	if err := db.ReplaceDownloadFiles(ctx, d.ID, []*DownloadFile{file}); err != nil {
		t.Fatalf("ReplaceDownloadFiles() error = %v", err)
	}

	expires := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	if err := db.SetDownloadFileURL(ctx, file.ID, "https://cdn.example/movie.mkv", expires); err != nil {
		t.Fatalf("SetDownloadFileURL() error = %v", err)
	}

	files, err := db.ListDownloadFiles(ctx, d.ID)
	if err != nil {
		t.Fatalf("ListDownloadFiles() error = %v", err)
	}
	if len(files) != 1 || files[0].DownloadURL != "https://cdn.example/movie.mkv" {
		t.Errorf("ListDownloadFiles() = %+v, want resolved URL set", files)
	}
}

func TestUpdateDownloadRetry(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateProviderCompleted
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	nextRetry := time.Now().Add(20 * time.Second).UTC().Truncate(time.Second)
	if err := db.UpdateDownloadRetry(ctx, d.ID, 1, nextRetry, "temporary network error"); err != nil {
		t.Fatalf("UpdateDownloadRetry() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", got.RetryCount)
	}
	if got.NextRetryAt == nil || !got.NextRetryAt.Equal(nextRetry) {
		t.Errorf("NextRetryAt = %v, want %v", got.NextRetryAt, nextRetry)
	}
	if got.ErrorMessage != "temporary network error" {
		t.Errorf("ErrorMessage = %q, want temporary network error", got.ErrorMessage)
	}
	// A retry (not a give-up) must not change state.
	if got.State != StateProviderCompleted {
		t.Errorf("State = %q, want unchanged provider_completed", got.State)
	}
}

func TestListDownloadsDueForRetry(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()

	ready := newTestDownload(KindTorrent)
	ready.State = StateProviderCompleted
	ready.ProviderDownloadID = "ready"
	if err := db.InsertDownload(ctx, ready); err != nil {
		t.Fatalf("InsertDownload(ready) error = %v", err)
	}

	notYetDue := newTestDownload(KindTorrent)
	notYetDue.State = StateProviderCompleted
	notYetDue.ProviderDownloadID = "not-yet-due"
	if err := db.InsertDownload(ctx, notYetDue); err != nil {
		t.Fatalf("InsertDownload(notYetDue) error = %v", err)
	}
	if err := db.UpdateDownloadRetry(ctx, notYetDue.ID, 1, now.Add(1*time.Hour), "still backing off"); err != nil {
		t.Fatalf("UpdateDownloadRetry(notYetDue) error = %v", err)
	}

	pastDue := newTestDownload(KindTorrent)
	pastDue.State = StateProviderCompleted
	pastDue.ProviderDownloadID = "past-due"
	if err := db.InsertDownload(ctx, pastDue); err != nil {
		t.Fatalf("InsertDownload(pastDue) error = %v", err)
	}
	if err := db.UpdateDownloadRetry(ctx, pastDue.ID, 1, now.Add(-1*time.Hour), "backoff elapsed"); err != nil {
		t.Fatalf("UpdateDownloadRetry(pastDue) error = %v", err)
	}

	due, err := db.ListDownloadsDueForRetry(ctx, StateProviderCompleted, now)
	if err != nil {
		t.Fatalf("ListDownloadsDueForRetry() error = %v", err)
	}
	gotIDs := map[string]bool{}
	for _, d := range due {
		gotIDs[d.ID] = true
	}
	if !gotIDs[ready.ID] {
		t.Errorf("expected %q (no next_retry_at) to be due", ready.ID)
	}
	if !gotIDs[pastDue.ID] {
		t.Errorf("expected %q (next_retry_at in the past) to be due", pastDue.ID)
	}
	if gotIDs[notYetDue.ID] {
		t.Errorf("expected %q (next_retry_at in the future) NOT to be due", notYetDue.ID)
	}
}

func TestLocalStateFromProvider(t *testing.T) {
	tests := []struct {
		in   debrid.DownloadState
		want string
	}{
		{debrid.StateQueued, StateQueued},
		{debrid.StateDownloading, StateDownloading},
		{debrid.StateCompleted, StateProviderCompleted},
		{debrid.StateError, StateError},
		{debrid.StateUnknown, StateQueued},
		{debrid.DownloadState("something TorBox invents later"), StateQueued},
	}
	for _, tt := range tests {
		if got := LocalStateFromProvider(tt.in); got != tt.want {
			t.Errorf("LocalStateFromProvider(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRefreshFromProvider_UpdatesChangedRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateQueued
	d.Progress = 0
	d.SizeBytes = 0
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	rows := []*Download{d}
	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateCompleted, Progress: 1, SizeBytes: 999},
	}
	db.RefreshFromProvider(ctx, rows, statuses)

	// The in-memory row is updated in place...
	if d.State != StateProviderCompleted || d.Progress != 1 || d.SizeBytes != 999 {
		t.Errorf("in-memory row = state:%q progress:%v size:%v, want provider_completed/1/999", d.State, d.Progress, d.SizeBytes)
	}
	// ...and so is the persisted one.
	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != StateProviderCompleted || got.Progress != 1 || got.SizeBytes != 999 {
		t.Errorf("persisted row = state:%q progress:%v size:%v, want provider_completed/1/999", got.State, got.Progress, got.SizeBytes)
	}
}

func TestRefreshFromProvider_IgnoresRowsMissingFromStatuses(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateQueued
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	// statuses has no entry at all for d.ProviderDownloadID — e.g. the
	// provider's list hasn't indexed a freshly-added download yet.
	db.RefreshFromProvider(ctx, []*Download{d}, nil)

	if d.State != StateQueued {
		t.Errorf("state = %q, want it left unchanged (queued) when absent from statuses", d.State)
	}
}

// TestRefreshFromProvider_NeverRegressesReadyForImport is the guarantee the
// comment on RefreshFromProvider promises: once internal/importer has moved
// a row to ready_for_import, the provider's own (possibly stale) state must
// never move it back.
func TestRefreshFromProvider_NeverRegressesReadyForImport(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateReadyForImport
	d.Progress = 1
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.3},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.State != StateReadyForImport {
		t.Errorf("state = %q, want it to stay ready_for_import", d.State)
	}
}

// TestRefreshFromProvider_ToleratesUpdateFailure proves one row's update
// error (e.g. it was deleted concurrently) doesn't stop the rest of the
// batch from being processed.
func TestRefreshFromProvider_ToleratesUpdateFailure(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	good := newTestDownload(KindTorrent)
	good.State = StateQueued
	good.ProviderDownloadID = "good"
	if err := db.InsertDownload(ctx, good); err != nil {
		t.Fatalf("InsertDownload(good) error = %v", err)
	}

	// missing was never inserted, so UpdateDownloadStatus will fail with
	// ErrNotFound for it.
	missing := newTestDownload(KindTorrent)
	missing.State = StateQueued
	missing.ProviderDownloadID = "missing"

	statuses := []debrid.DownloadStatus{
		{ID: "good", State: debrid.StateCompleted, Progress: 1},
		{ID: "missing", State: debrid.StateCompleted, Progress: 1},
	}
	db.RefreshFromProvider(ctx, []*Download{missing, good}, statuses)

	if good.State != StateProviderCompleted {
		t.Errorf("good row's state = %q, want provider_completed despite missing row's update failing", good.State)
	}

	got, err := db.GetDownloadByID(ctx, good.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != StateProviderCompleted {
		t.Errorf("good row not persisted: state = %q", got.State)
	}
}
