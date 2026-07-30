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
		Source:             "magnet:?xt=urn:btih:abc123&dn=Some.Release.Name",
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
	if got.Source != d.Source {
		t.Errorf("Source = %q, want %q", got.Source, d.Source)
	}

	byHash, err := db.GetDownloadByHash(ctx, d.Hash)
	if err != nil {
		t.Fatalf("GetDownloadByHash() error = %v", err)
	}
	if byHash == nil || byHash.ID != d.ID {
		t.Errorf("GetDownloadByHash() = %+v, want id %s", byHash, d.ID)
	}
}

// TestInsertDownload_WebDLKind is a regression test for migration
// 0005_webdl_kind.sql: the downloads table's CHECK constraint had to be
// widened (via SQLite's recreate-table pattern, since CHECK can't be altered
// in place) to allow kind='webdl' alongside 'torrent'/'usenet' — this proves
// a webdl row actually inserts and round-trips cleanly, not just that the
// migration runs without erroring.
func TestInsertDownload_WebDLKind(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindWebDL)
	d.Hash = "" // a web download's hash isn't known until the provider resolves it
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload(webdl) error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got == nil || got.Kind != KindWebDL {
		t.Errorf("GetDownloadByID() = %+v, want kind=webdl", got)
	}

	rows, err := db.ListDownloads(ctx, KindWebDL)
	if err != nil {
		t.Fatalf("ListDownloads(webdl) error = %v", err)
	}
	if len(rows) != 1 || rows[0].ID != d.ID {
		t.Errorf("ListDownloads(webdl) = %+v, want only %s", rows, d.ID)
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

// TestRetryDownload proves the manual retry path resets a download that
// gave up back to a state internal/importer will pick up on its very next
// tick — the counterpart to TestUpdateDownloadRetry's automatic path.
func TestRetryDownload(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateError
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	pastRetry := time.Now().Add(-time.Hour).UTC()
	if err := db.UpdateDownloadRetry(ctx, d.ID, 5, pastRetry, "gave up: connection refused"); err != nil {
		t.Fatalf("seed UpdateDownloadRetry() error = %v", err)
	}
	if err := db.UpdateDownloadStatus(ctx, d.ID, StateError, 0, 0, nil, "gave up: connection refused"); err != nil {
		t.Fatalf("seed UpdateDownloadStatus() error = %v", err)
	}

	if err := db.RetryDownload(ctx, d.ID); err != nil {
		t.Fatalf("RetryDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != StateProviderCompleted {
		t.Errorf("State = %q, want provider_completed", got.State)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0", got.RetryCount)
	}
	if got.NextRetryAt != nil {
		t.Errorf("NextRetryAt = %v, want nil (cleared)", got.NextRetryAt)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want cleared", got.ErrorMessage)
	}

	// The whole point: it must now actually be due for retry.
	due, err := db.ListDownloadsDueForRetry(ctx, StateProviderCompleted, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListDownloadsDueForRetry() error = %v", err)
	}
	found := false
	for _, row := range due {
		if row.ID == d.ID {
			found = true
		}
	}
	if !found {
		t.Error("retried download not found in ListDownloadsDueForRetry results")
	}
}

func TestRetryDownload_NotFound(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.RetryDownload(ctx, "does-not-exist"); err == nil {
		t.Error("RetryDownload() for a nonexistent id: expected an error, got nil")
	}
}

// TestReAddDownload proves re-add points the local row at a brand new
// provider_download_id and resets everything else as if freshly added —
// used when the *original* provider-side download is gone entirely, not
// just a transient fetch failure (see TestRetryDownload).
func TestReAddDownload(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateError
	d.ProviderDownloadID = "old-provider-id"
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	if err := db.UpdateDownloadRetry(ctx, d.ID, 5, time.Now().Add(-time.Hour), "gave up: not found"); err != nil {
		t.Fatalf("seed UpdateDownloadRetry() error = %v", err)
	}
	completedAt := time.Now().UTC()
	if err := db.UpdateDownloadStatus(ctx, d.ID, StateError, 1.0, 2048, &completedAt, "gave up: not found"); err != nil {
		t.Fatalf("seed UpdateDownloadStatus() error = %v", err)
	}

	if err := db.ReAddDownload(ctx, d.ID, "new-provider-id"); err != nil {
		t.Fatalf("ReAddDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.ProviderDownloadID != "new-provider-id" {
		t.Errorf("ProviderDownloadID = %q, want new-provider-id", got.ProviderDownloadID)
	}
	if got.State != StateQueued {
		t.Errorf("State = %q, want queued", got.State)
	}
	if got.Progress != 0 || got.SizeBytes != 0 {
		t.Errorf("Progress/SizeBytes = %v/%v, want 0/0 (reset as if freshly added)", got.Progress, got.SizeBytes)
	}
	if got.RetryCount != 0 || got.NextRetryAt != nil {
		t.Errorf("RetryCount/NextRetryAt = %v/%v, want 0/nil", got.RetryCount, got.NextRetryAt)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want cleared", got.ErrorMessage)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil (not complete again yet)", got.CompletedAt)
	}
	// The local id, name, category, hash, and source must all survive —
	// only the provider-side identity and progress reset.
	if got.ID != d.ID || got.Name != d.Name || got.Category != d.Category || got.Hash != d.Hash || got.Source != d.Source {
		t.Errorf("identity fields changed: got = %+v, want matching %+v", got, d)
	}
}

func TestReAddDownload_NotFound(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.ReAddDownload(ctx, "does-not-exist", "new-id"); err == nil {
		t.Error("ReAddDownload() for a nonexistent id: expected an error, got nil")
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

// TestRefreshFromProvider_BackfillsEmptyHash proves the fix for a real bug
// found live: a torrent discovered while the provider was still indexing it
// (placeholder name, no hash yet) got permanently stuck with that
// incomplete snapshot, since nothing else ever revisited Hash/Name after
// insert. Confirmed against the user's real TorBox account — two adopted
// torrents had an empty hash locally despite TorBox's own mylist reporting
// a real one by the time it was checked.
func TestRefreshFromProvider_BackfillsEmptyHash(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.Hash = ""
	d.Name = "45____Riven_Worlds_seires___.torrent"
	d.State = StateProviderCompleted
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	rows := []*Download{d}
	statuses := []debrid.DownloadStatus{
		{
			ID:        debrid.ProviderDownloadID(d.ProviderDownloadID),
			Hash:      "5A5C00CDB722F210453928EE5B789FA727306236", // providers can report mixed case
			Name:      "2020-2022 - Riven Worlds seires (5)",
			State:     debrid.StateCompleted,
			Progress:  1,
			SizeBytes: d.SizeBytes,
		},
	}
	db.RefreshFromProvider(ctx, rows, statuses)

	wantHash := "5a5c00cdb722f210453928ee5b789fa727306236"
	if d.Hash != wantHash || d.Name != "2020-2022 - Riven Worlds seires (5)" {
		t.Errorf("in-memory row = hash:%q name:%q, want %q / the real name", d.Hash, d.Name, wantHash)
	}
	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.Hash != wantHash || got.Name != "2020-2022 - Riven Worlds seires (5)" {
		t.Errorf("persisted row = hash:%q name:%q, want %q / the real name", got.Hash, got.Name, wantHash)
	}
}

// TestRefreshFromProvider_NeverOverwritesExistingHash proves the backfill
// only ever fires for a row that genuinely has no hash yet — it must never
// second-guess or replace a hash a row already has, even if the provider's
// current value happens to differ (which shouldn't normally happen, but the
// guard itself is what the test is pinning down).
func TestRefreshFromProvider_NeverOverwritesExistingHash(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.Hash = "originalhash"
	d.Name = "Original Name"
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	rows := []*Download{d}
	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), Hash: "differenthash", Name: "Different Name", State: debrid.StateDownloading, Progress: 0.5},
	}
	db.RefreshFromProvider(ctx, rows, statuses)

	if d.Hash != "originalhash" || d.Name != "Original Name" {
		t.Errorf("hash/name = %q/%q, want left untouched since Hash was already non-empty", d.Hash, d.Name)
	}
}

// TestRefreshFromProvider_BackfillsEmptySource proves a row with no stored
// Source (e.g. a discovered download — see internal/importer.discoverManual)
// gets it backfilled from the provider's OriginalURL the moment the provider
// reports one — what lets Re-add work for a discovered download after the
// fact, not just one added directly through AcerviNode's own form.
func TestRefreshFromProvider_BackfillsEmptySource(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.Source = ""
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), Hash: d.Hash, State: debrid.StateDownloading, Progress: 0.5, OriginalURL: "magnet:?xt=urn:btih:abc123"},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.Source != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("Source = %q, want the backfilled magnet", d.Source)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.Source != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("persisted Source = %q, want the backfilled magnet", got.Source)
	}
}

// TestRefreshFromProvider_NeverOverwritesExistingSource mirrors
// TestRefreshFromProvider_NeverOverwritesExistingHash: a row that already has
// a Source (e.g. one AcerviNode itself recorded at add time) must never have
// it replaced, even if the provider also reports an OriginalURL.
func TestRefreshFromProvider_NeverOverwritesExistingSource(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.Source = "magnet:?xt=urn:btih:original&dn=Original"
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), Hash: d.Hash, State: debrid.StateDownloading, Progress: 0.5, OriginalURL: "magnet:?xt=urn:btih:different"},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.Source != "magnet:?xt=urn:btih:original&dn=Original" {
		t.Errorf("Source = %q, want left untouched since it was already non-empty", d.Source)
	}
}

// TestRefreshFromProvider_ManagedDownloadMissingFromStatuses_NeverFlaggedByThisMechanism
// proves a Managed (AddedViaArr) row missing from the provider's listing is
// left entirely alone by handleMissingFromProvider, however many ticks it
// stays missing — internal/importer's own fetch-retry path is what catches a
// vanished Managed download instead (the fetch attempt itself fails and
// eventually gives up with a clear reason); this mechanism only ever applies
// to Manual, which has no such path — see the Manual-specific tests below.
func TestRefreshFromProvider_ManagedDownloadMissingFromStatuses_NeverFlaggedByThisMechanism(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateQueued
	d.AddedVia = AddedViaArr
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	// statuses has no entry at all for d.ProviderDownloadID, repeatedly —
	// well past missingDetectionThreshold — to prove this really is never
	// applied to a Managed row, not just debounced longer.
	for i := 0; i < missingDetectionThreshold+2; i++ {
		db.RefreshFromProvider(ctx, []*Download{d}, nil)
	}

	if d.State != StateQueued {
		t.Errorf("state = %q, want it left unchanged (queued) when absent from statuses", d.State)
	}
	if d.MissingCount != 0 {
		t.Errorf("MissingCount = %d, want 0 (never incremented for a Managed row)", d.MissingCount)
	}
}

// TestRefreshFromProvider_ManualDownloadMissing_SingleMissDoesNotFlag proves
// a single absence doesn't immediately flag a Manual download as gone — see
// missingDetectionThreshold's doc comment for why a debounce exists at all.
func TestRefreshFromProvider_ManualDownloadMissing_SingleMissDoesNotFlag(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateProviderCompleted
	d.AddedVia = AddedViaManual
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	db.RefreshFromProvider(ctx, []*Download{d}, nil)

	if d.State != StateProviderCompleted {
		t.Errorf("state = %q, want it left unchanged after one miss", d.State)
	}
	if d.MissingCount != 1 {
		t.Errorf("MissingCount = %d, want 1", d.MissingCount)
	}
}

// TestRefreshFromProvider_ManualDownloadVanishes_FlaggedAfterThreshold proves
// a Manual download absent from missingDetectionThreshold consecutive
// successful listings is flagged StateError with a clear reason — the actual
// "proactively detect a vanished Manual download" behavior — and not before.
func TestRefreshFromProvider_ManualDownloadVanishes_FlaggedAfterThreshold(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateProviderCompleted
	d.AddedVia = AddedViaManual
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	for i := 1; i < missingDetectionThreshold; i++ {
		db.RefreshFromProvider(ctx, []*Download{d}, nil)
		if d.State == StateError {
			t.Fatalf("state = error after miss %d, want it to stay provider_completed until threshold %d", i, missingDetectionThreshold)
		}
	}

	db.RefreshFromProvider(ctx, []*Download{d}, nil)

	if d.State != StateError {
		t.Fatalf("state = %q after %d misses, want error", d.State, missingDetectionThreshold)
	}
	if d.ErrorMessage != missingDownloadErrorMessage {
		t.Errorf("ErrorMessage = %q, want %q", d.ErrorMessage, missingDownloadErrorMessage)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != StateError || got.ErrorMessage != missingDownloadErrorMessage {
		t.Errorf("persisted row = state:%q error:%q, want error/%q", got.State, got.ErrorMessage, missingDownloadErrorMessage)
	}
}

// TestRefreshFromProvider_ManualDownloadReappears_ResetsMissingCount proves a
// Manual download that reappears in a listing before crossing the threshold
// has its miss counter reset back to 0, rather than accumulating misses
// across gaps (e.g. missed, seen, missed, seen — never actually flagged).
func TestRefreshFromProvider_ManualDownloadReappears_ResetsMissingCount(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	d.AddedVia = AddedViaManual
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	db.RefreshFromProvider(ctx, []*Download{d}, nil)
	if d.MissingCount != 1 {
		t.Fatalf("MissingCount after one miss = %d, want 1", d.MissingCount)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.MissingCount != 0 {
		t.Errorf("MissingCount after reappearing = %d, want 0", d.MissingCount)
	}
	if d.State != StateDownloading {
		t.Errorf("state = %q, want downloading (updated normally once found again)", d.State)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.MissingCount != 0 {
		t.Errorf("persisted MissingCount = %d, want 0", got.MissingCount)
	}
}

// TestRefreshFromProvider_VanishedManualDownload_SelfHealsIfProviderReportsItAgain
// proves flagging a Manual download StateError this way isn't sticky — the
// same "not sticky" guarantee TestRefreshFromProvider_ProviderErrorCanRecover
// already proves for a provider-reported error, since this path deliberately
// never touches RetryCount either (see handleMissingFromProvider).
func TestRefreshFromProvider_VanishedManualDownload_SelfHealsIfProviderReportsItAgain(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateError
	d.ErrorMessage = missingDownloadErrorMessage
	d.AddedVia = AddedViaManual
	d.RetryCount = 0
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateCompleted, Progress: 1},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.State != StateProviderCompleted {
		t.Errorf("state = %q, want provider_completed (self-healed once the provider reported it again)", d.State)
	}
}

// TestRefreshFromProvider_AlreadyErroredManualDownload_NotDoubleFlagged proves
// a Manual download already in StateError for some other reason (e.g. a
// provider-reported error) doesn't have its MissingCount incremented or its
// ErrorMessage overwritten just because it also happens to be missing from a
// listing — see handleMissingFromProvider's early return.
func TestRefreshFromProvider_AlreadyErroredManualDownload_NotDoubleFlagged(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateError
	d.ErrorMessage = "stalled (no seeds)"
	d.AddedVia = AddedViaManual
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	db.RefreshFromProvider(ctx, []*Download{d}, nil)

	if d.MissingCount != 0 {
		t.Errorf("MissingCount = %d, want 0 (not incremented for an already-errored row)", d.MissingCount)
	}
	if d.ErrorMessage != "stalled (no seeds)" {
		t.Errorf("ErrorMessage = %q, want the original reason untouched", d.ErrorMessage)
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

// TestRefreshFromProvider_SurfacesProviderErrorState proves a provider
// reporting a genuine error (e.g. TorBox's own "Error" state, or a stalled/
// no-seeds torrent, both mapped to debrid.StateError before this reaches
// here — see torbox.mapDownloadState) actually lands the row in the local
// error state with the provider's raw reason as error_message, rather than
// the state transition being silently dropped.
func TestRefreshFromProvider_SurfacesProviderErrorState(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	d.Progress = 0.4
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateError, RawState: "stalled (no seeds)"},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.State != StateError {
		t.Errorf("state = %q, want error", d.State)
	}
	if d.ErrorMessage != "stalled (no seeds)" {
		t.Errorf("ErrorMessage = %q, want the provider's raw state", d.ErrorMessage)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != StateError || got.ErrorMessage != "stalled (no seeds)" {
		t.Errorf("persisted row = state:%q error:%q, want error/\"stalled (no seeds)\"", got.State, got.ErrorMessage)
	}
}

// TestRefreshFromProvider_ProviderErrorCanRecover proves a StateError the
// provider itself reported (RetryCount == 0) isn't sticky — if the provider
// later reports the download progressing again (e.g. a stalled torrent found
// a seed), the row un-errors automatically. Contrast with
// TestRefreshFromProvider_DoesNotResurrectImporterGaveUp below.
func TestRefreshFromProvider_ProviderErrorCanRecover(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateError
	d.ErrorMessage = "stalled (no seeds)"
	d.RetryCount = 0
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.6},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.State != StateDownloading {
		t.Errorf("state = %q, want downloading (provider-side error recovered)", d.State)
	}
}

// TestRefreshFromProvider_DoesNotResurrectImporterGaveUp proves a download
// internal/importer gave up on after exhausting its own fetch retries
// (RetryCount > 0 — a local, sticky decision distinct from a provider-
// reported error) is NOT silently reset back to provider_completed just
// because the provider still reports its unchanged old state on a later
// poll — only an explicit manual retry/re-add should revive it.
func TestRefreshFromProvider_DoesNotResurrectImporterGaveUp(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateError
	d.ErrorMessage = "fetch file: connection reset"
	d.RetryCount = 5 // importer's own exhaustion path always persists this > 0
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	// Provider still cheerfully reports "cached" — as it would for a fetch
	// failure that had nothing to do with the provider itself.
	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateCompleted, Progress: 1, SizeBytes: d.SizeBytes},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses)

	if d.State != StateError {
		t.Errorf("state = %q, want it to stay error (importer's give-up is sticky)", d.State)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.State != StateError {
		t.Errorf("persisted state = %q, want it to stay error", got.State)
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
