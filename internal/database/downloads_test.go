package database

import (
	"context"
	"fmt"
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

// TestInsertDownload_SourceFileRoundTripsViaGetSourceFile proves a usenet
// download's stored .nzb bytes/filename survive InsertDownload and can be
// fetched back via GetSourceFile — what handleReAddDownload uses to
// resubmit a file-uploaded NZB.
func TestInsertDownload_SourceFileRoundTripsViaGetSourceFile(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindUsenet)
	d.Source = "" // file-based add, not URL-based
	d.SourceFile = []byte("fake nzb file contents")
	d.SourceFileName = "release.nzb"
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	filename, data, err := db.GetSourceFile(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetSourceFile() error = %v", err)
	}
	if filename != "release.nzb" || string(data) != "fake nzb file contents" {
		t.Errorf("GetSourceFile() = %q/%q, want release.nzb/fake nzb file contents", filename, data)
	}
}

// TestInsertDownload_SourceFileNotIncludedInNormalScan proves the raw file
// bytes are deliberately excluded from the normal Download read path
// (GetDownloadByID etc.) — see Download.SourceFile's doc comment — while
// SourceFileName (cheap) still comes through normally, which is what lets
// has_source be computed without paying for the blob on every list/detail
// fetch.
func TestInsertDownload_SourceFileNotIncludedInNormalScan(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindUsenet)
	d.Source = ""
	d.SourceFile = []byte("fake nzb file contents")
	d.SourceFileName = "release.nzb"
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.SourceFileName != "release.nzb" {
		t.Errorf("SourceFileName = %q, want release.nzb", got.SourceFileName)
	}
	if got.SourceFile != nil {
		t.Errorf("SourceFile = %q, want nil (not included in the normal scan)", got.SourceFile)
	}
}

// TestGetSourceFile_EmptyForRowWithNothingStored proves GetSourceFile
// returns a clean empty result (not an error) for a row that never had a
// file stored — a URL-based add, a torrent, a webdl, or a discovered
// download.
func TestGetSourceFile_EmptyForRowWithNothingStored(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	filename, data, err := db.GetSourceFile(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetSourceFile() error = %v", err)
	}
	if filename != "" || data != nil {
		t.Errorf("GetSourceFile() = %q/%v, want empty/nil", filename, data)
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

func TestUpdateDownloadStatus_SetsCachedAtOnceOnFirstProviderCompleted(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	before, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if before.CachedAt != nil {
		t.Fatalf("got CachedAt=%v before any provider_completed, want nil", before.CachedAt)
	}

	if err := db.UpdateDownloadStatus(ctx, d.ID, StateProviderCompleted, 1.0, 1024, nil, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}
	firstCached, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if firstCached.CachedAt == nil {
		t.Fatal("got CachedAt=nil after provider_completed, want set")
	}
	cachedAt := *firstCached.CachedAt

	// A later write, even one that changes state again, must not move
	// CachedAt — it records the *first* time this happened, not the most
	// recent.
	if err := db.UpdateDownloadStatus(ctx, d.ID, StateProviderCompleted, 1.0, 2048, nil, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}
	after, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if after.CachedAt == nil || !after.CachedAt.Equal(cachedAt) {
		t.Errorf("got CachedAt=%v after second write, want unchanged %v", after.CachedAt, cachedAt)
	}
}

// TestInsertDownload_SetsCachedAtWhenBornProviderCompleted guards a real bug
// found live: a Manual download whose very first observed status was
// already StateProviderCompleted (TorBox's common instant-cache case — the
// content was already cached the moment it was added, so there's no
// "queued"/"downloading" phase at all) showed "Cached —" in the web UI
// despite sitting at 100% progress since the moment it was added.
// UpdateDownloadStatus's own CachedAt logic only fires on a state
// *transition*, which never happens for a row that's born already in that
// state rather than moving into it later.
func TestInsertDownload_SetsCachedAtWhenBornProviderCompleted(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateProviderCompleted
	d.Progress = 1.0
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.CachedAt == nil {
		t.Error("got CachedAt=nil for a row inserted already provider_completed, want set")
	}
}

// TestInsertDownload_LeavesCachedAtNilForNonCompletedState is the ordinary
// case (still queued/downloading) — makes sure the fix above didn't start
// stamping cached_at unconditionally on every insert.
func TestInsertDownload_LeavesCachedAtNilForNonCompletedState(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent) // State: "queued", per newTestDownload
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.CachedAt != nil {
		t.Errorf("got CachedAt=%v for a row inserted as queued, want nil", got.CachedAt)
	}
}

// TestBackfillCachedAtMigration_SQLFixesAStuckRow proves migration 0010's
// exact statement correctly backfills a row that predates the
// InsertDownload fix above — one that's sitting in provider_completed with
// cached_at still NULL, the exact condition 0009's own doc comment wrongly
// assumed a "future refresh" would always eventually correct. Simulated via
// a raw INSERT (bypassing InsertDownload, which now prevents new rows from
// ever reaching this condition) rather than fighting the migration runner's
// once-only semantics — what's actually being verified is the UPDATE
// statement's own correctness, not when it happens to run.
func TestBackfillCachedAtMigration_SQLFixesAStuckRow(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	addedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	_, err := db.ExecContext(ctx, `
		INSERT INTO downloads (
			id, provider, provider_download_id, kind, hash, name,
			size_bytes, state, progress, added_at, updated_at, added_via
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "torbox", "provider-stuck", "torrent", "abc123", "Stuck Row",
		1024, StateProviderCompleted, 1.0, addedAt, addedAt, AddedViaManual,
	)
	if err != nil {
		t.Fatalf("seed raw stuck row: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE downloads SET cached_at = added_at WHERE state = 'provider_completed' AND cached_at IS NULL`,
	); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	got, err := db.GetDownloadByID(ctx, id)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.CachedAt == nil || !got.CachedAt.Equal(addedAt) {
		t.Errorf("CachedAt = %v, want %v (backfilled from added_at)", got.CachedAt, addedAt)
	}
}

func TestUpdateDownloadCategory(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.Category = "tv-sonarr"
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	if err := db.UpdateDownloadCategory(ctx, d.ID, "tv-sonarr-imported"); err != nil {
		t.Fatalf("UpdateDownloadCategory() error = %v", err)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.Category != "tv-sonarr-imported" {
		t.Errorf("Category = %q, want tv-sonarr-imported", got.Category)
	}
}

func TestUpdateDownloadCategory_UnknownID(t *testing.T) {
	db := openTestDB(t)
	err := db.UpdateDownloadCategory(context.Background(), "does-not-exist", "tv")
	if err == nil {
		t.Error("UpdateDownloadCategory() expected error for unknown id, got nil")
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
	// Route through provider_completed first so CachedAt gets set on the old
	// provider-side download, proving ReAddDownload actually clears it below
	// rather than it just having never been set.
	if err := db.UpdateDownloadStatus(ctx, d.ID, StateProviderCompleted, 1.0, 2048, nil, ""); err != nil {
		t.Fatalf("seed UpdateDownloadStatus(provider_completed) error = %v", err)
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
	if got.CachedAt != nil {
		t.Errorf("CachedAt = %v, want nil (new provider-side download hasn't been observed cached yet)", got.CachedAt)
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

// TestListActiveManagedDownloads proves the fast-poll query's scoping:
// only an AddedViaArr (Managed) download currently queued/downloading
// qualifies — not a Manual download in the same states, not a Managed
// download that's already moved past those states (provider_completed,
// ready_for_import, error), and not a different kind.
func TestListActiveManagedDownloads(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	managedQueued := newTestDownload(KindTorrent)
	managedQueued.ProviderDownloadID = "managed-queued"
	managedQueued.AddedVia = AddedViaArr
	managedQueued.State = StateQueued
	if err := db.InsertDownload(ctx, managedQueued); err != nil {
		t.Fatalf("InsertDownload(managedQueued) error = %v", err)
	}

	managedDownloading := newTestDownload(KindTorrent)
	managedDownloading.ProviderDownloadID = "managed-downloading"
	managedDownloading.AddedVia = AddedViaArr
	managedDownloading.State = StateDownloading
	if err := db.InsertDownload(ctx, managedDownloading); err != nil {
		t.Fatalf("InsertDownload(managedDownloading) error = %v", err)
	}

	managedCompleted := newTestDownload(KindTorrent)
	managedCompleted.ProviderDownloadID = "managed-completed"
	managedCompleted.AddedVia = AddedViaArr
	managedCompleted.State = StateProviderCompleted
	if err := db.InsertDownload(ctx, managedCompleted); err != nil {
		t.Fatalf("InsertDownload(managedCompleted) error = %v", err)
	}

	manualDownloading := newTestDownload(KindTorrent)
	manualDownloading.ProviderDownloadID = "manual-downloading"
	manualDownloading.AddedVia = AddedViaManual
	manualDownloading.State = StateDownloading
	if err := db.InsertDownload(ctx, manualDownloading); err != nil {
		t.Fatalf("InsertDownload(manualDownloading) error = %v", err)
	}

	managedOtherKind := newTestDownload(KindUsenet)
	managedOtherKind.ProviderDownloadID = "managed-usenet-downloading"
	managedOtherKind.AddedVia = AddedViaArr
	managedOtherKind.State = StateDownloading
	if err := db.InsertDownload(ctx, managedOtherKind); err != nil {
		t.Fatalf("InsertDownload(managedOtherKind) error = %v", err)
	}

	active, err := db.ListActiveManagedDownloads(ctx, KindTorrent)
	if err != nil {
		t.Fatalf("ListActiveManagedDownloads() error = %v", err)
	}
	gotIDs := map[string]bool{}
	for _, d := range active {
		gotIDs[d.ID] = true
	}
	if !gotIDs[managedQueued.ID] {
		t.Errorf("expected %q (managed, queued) to be active", managedQueued.ID)
	}
	if !gotIDs[managedDownloading.ID] {
		t.Errorf("expected %q (managed, downloading) to be active", managedDownloading.ID)
	}
	if gotIDs[managedCompleted.ID] {
		t.Errorf("expected %q (managed, already provider_completed) NOT to be active", managedCompleted.ID)
	}
	if gotIDs[manualDownloading.ID] {
		t.Errorf("expected %q (manual) NOT to be active", manualDownloading.ID)
	}
	if gotIDs[managedOtherKind.ID] {
		t.Errorf("expected %q (wrong kind) NOT to be active", managedOtherKind.ID)
	}
	if len(active) != 2 {
		t.Errorf("active = %+v, want exactly 2 rows", active)
	}
}

// TestListDownloadsEligibleForCleanup proves the cleanup query's scoping:
// only a Managed (arr) download in ready_for_import older than the cutoff
// qualifies — not one that's too recent, not a Manual download in the
// analogous provider_completed "available" state, and not a Managed
// download still in provider_completed/error (never reached
// ready_for_import at all).
func TestListDownloadsEligibleForCleanup(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	cutoff := now.Add(-7 * 24 * time.Hour)

	old := newTestDownload(KindTorrent)
	old.ProviderDownloadID = "old"
	old.AddedVia = AddedViaArr
	old.State = StateReadyForImport
	oldCompletedAt := now.Add(-10 * 24 * time.Hour)
	old.CompletedAt = &oldCompletedAt
	if err := db.InsertDownload(ctx, old); err != nil {
		t.Fatalf("InsertDownload(old) error = %v", err)
	}
	if err := db.UpdateDownloadStatus(ctx, old.ID, StateReadyForImport, 1, old.SizeBytes, &oldCompletedAt, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus(old) error = %v", err)
	}

	recent := newTestDownload(KindTorrent)
	recent.ProviderDownloadID = "recent"
	recent.AddedVia = AddedViaArr
	recent.State = StateReadyForImport
	if err := db.InsertDownload(ctx, recent); err != nil {
		t.Fatalf("InsertDownload(recent) error = %v", err)
	}
	recentCompletedAt := now.Add(-1 * time.Hour)
	if err := db.UpdateDownloadStatus(ctx, recent.ID, StateReadyForImport, 1, recent.SizeBytes, &recentCompletedAt, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus(recent) error = %v", err)
	}

	oldManual := newTestDownload(KindTorrent)
	oldManual.ProviderDownloadID = "old-manual"
	oldManual.AddedVia = AddedViaManual
	oldManual.State = StateProviderCompleted
	if err := db.InsertDownload(ctx, oldManual); err != nil {
		t.Fatalf("InsertDownload(oldManual) error = %v", err)
	}

	oldNotYetImported := newTestDownload(KindTorrent)
	oldNotYetImported.ProviderDownloadID = "old-not-yet-imported"
	oldNotYetImported.AddedVia = AddedViaArr
	oldNotYetImported.State = StateProviderCompleted
	if err := db.InsertDownload(ctx, oldNotYetImported); err != nil {
		t.Fatalf("InsertDownload(oldNotYetImported) error = %v", err)
	}

	eligible, err := db.ListDownloadsEligibleForCleanup(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListDownloadsEligibleForCleanup() error = %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != old.ID {
		t.Errorf("eligible = %+v, want exactly [old]", eligible)
	}
}

func TestCountDownloadsByState(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	torrentErr1 := newTestDownload(KindTorrent)
	torrentErr1.ProviderDownloadID = "torrent-err-1"
	torrentErr1.State = StateError
	if err := db.InsertDownload(ctx, torrentErr1); err != nil {
		t.Fatalf("InsertDownload(torrentErr1) error = %v", err)
	}

	torrentErr2 := newTestDownload(KindTorrent)
	torrentErr2.ProviderDownloadID = "torrent-err-2"
	torrentErr2.State = StateError
	if err := db.InsertDownload(ctx, torrentErr2); err != nil {
		t.Fatalf("InsertDownload(torrentErr2) error = %v", err)
	}

	usenetErr := newTestDownload(KindUsenet)
	usenetErr.ProviderDownloadID = "usenet-err"
	usenetErr.State = StateError
	if err := db.InsertDownload(ctx, usenetErr); err != nil {
		t.Fatalf("InsertDownload(usenetErr) error = %v", err)
	}

	torrentQueued := newTestDownload(KindTorrent)
	torrentQueued.ProviderDownloadID = "torrent-queued"
	torrentQueued.State = StateQueued
	if err := db.InsertDownload(ctx, torrentQueued); err != nil {
		t.Fatalf("InsertDownload(torrentQueued) error = %v", err)
	}

	counts, err := db.CountDownloadsByState(ctx, StateError)
	if err != nil {
		t.Fatalf("CountDownloadsByState() error = %v", err)
	}
	if counts[KindTorrent] != 2 {
		t.Errorf("counts[KindTorrent] = %d, want 2", counts[KindTorrent])
	}
	if counts[KindUsenet] != 1 {
		t.Errorf("counts[KindUsenet] = %d, want 1", counts[KindUsenet])
	}
	if _, ok := counts[KindWebDL]; ok {
		t.Errorf("counts[KindWebDL] = %d, want kind absent entirely (no error rows)", counts[KindWebDL])
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
	db.RefreshFromProvider(ctx, rows, statuses, time.Now())

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
	if got.CachedAt == nil {
		t.Error("got CachedAt=nil after refreshing into provider_completed, want set")
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
	db.RefreshFromProvider(ctx, rows, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, rows, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
		db.RefreshFromProvider(ctx, []*Download{d}, nil, time.Now())
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

	db.RefreshFromProvider(ctx, []*Download{d}, nil, time.Now())

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
		db.RefreshFromProvider(ctx, []*Download{d}, nil, time.Now())
		if d.State == StateError {
			t.Fatalf("state = error after miss %d, want it to stay provider_completed until threshold %d", i, missingDetectionThreshold)
		}
	}

	db.RefreshFromProvider(ctx, []*Download{d}, nil, time.Now())

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

	db.RefreshFromProvider(ctx, []*Download{d}, nil, time.Now())
	if d.MissingCount != 1 {
		t.Fatalf("MissingCount after one miss = %d, want 1", d.MissingCount)
	}

	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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

	db.RefreshFromProvider(ctx, []*Download{d}, nil, time.Now())

	if d.MissingCount != 0 {
		t.Errorf("MissingCount = %d, want 0 (not incremented for an already-errored row)", d.MissingCount)
	}
	if d.ErrorMessage != "stalled (no seeds)" {
		t.Errorf("ErrorMessage = %q, want the original reason untouched", d.ErrorMessage)
	}
}

// seedManualDownloads inserts n AddedViaManual, StateProviderCompleted rows
// with distinct provider_download_ids ("mass-vanish-0", "mass-vanish-1", …)
// — a helper for the mass-vanish circuit-breaker tests below, which need
// several rows at once rather than newTestDownload's single hardcoded id.
func seedManualDownloads(t *testing.T, db *DB, n int) []*Download {
	t.Helper()
	ctx := context.Background()
	out := make([]*Download, n)
	for i := 0; i < n; i++ {
		d := newTestDownload(KindTorrent)
		d.ProviderDownloadID = fmt.Sprintf("mass-vanish-%d", i)
		d.State = StateProviderCompleted
		d.AddedVia = AddedViaManual
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}
		out[i] = d
	}
	return out
}

// TestRefreshFromProvider_MassVanish_CircuitBreakerSkipsDetection proves a
// provider listing that comes back empty while several Manual downloads are
// tracked (more than half of massVanishMinTracked+ rows missing at once) is
// treated as suspicious rather than as proof every one of them vanished —
// missing_count never advances and nothing gets flagged error, no matter how
// many passes see the same empty listing. See isSuspectedMassVanish.
func TestRefreshFromProvider_MassVanish_CircuitBreakerSkipsDetection(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	downloads := seedManualDownloads(t, db, 4)

	// Run well past missingDetectionThreshold — if the circuit breaker
	// weren't working, every row would be StateError by now.
	for i := 0; i < missingDetectionThreshold+2; i++ {
		db.RefreshFromProvider(ctx, downloads, nil, time.Now())
	}

	for _, d := range downloads {
		if d.MissingCount != 0 {
			t.Errorf("download %s MissingCount = %d, want 0 (circuit breaker should have suppressed detection)", d.ID, d.MissingCount)
		}
		if d.State != StateProviderCompleted {
			t.Errorf("download %s state = %q, want provider_completed (never flagged)", d.ID, d.State)
		}
	}
}

// TestRefreshFromProvider_BelowMinTracked_CircuitBreakerDoesNotApply proves
// the circuit breaker doesn't suppress detection for an account with only a
// couple of Manual downloads — massVanishMinTracked exists so a small
// account isn't permanently exempt from vanish-detection just because a
// small absolute count looks like a large fraction.
func TestRefreshFromProvider_BelowMinTracked_CircuitBreakerDoesNotApply(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	downloads := seedManualDownloads(t, db, massVanishMinTracked-1)

	for i := 0; i < missingDetectionThreshold; i++ {
		db.RefreshFromProvider(ctx, downloads, nil, time.Now())
	}

	for _, d := range downloads {
		if d.State != StateError {
			t.Errorf("download %s state = %q, want error (below massVanishMinTracked, circuit breaker should not apply)", d.ID, d.State)
		}
	}
}

// TestRefreshFromProvider_PartialVanish_BelowFraction_CircuitBreakerDoesNotApply
// proves a normal, low-fraction case (most tracked downloads still present,
// one genuinely gone) is detected as usual — the circuit breaker only
// engages when the missing fraction crosses massVanishFraction, not for any
// single item disappearing among many that are still fine.
func TestRefreshFromProvider_PartialVanish_BelowFraction_CircuitBreakerDoesNotApply(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	downloads := seedManualDownloads(t, db, 4)
	vanished := downloads[0]
	statuses := make([]debrid.DownloadStatus, 0, 3)
	for _, d := range downloads[1:] {
		statuses = append(statuses, debrid.DownloadStatus{
			ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateCompleted, Progress: 1,
		})
	}

	for i := 0; i < missingDetectionThreshold; i++ {
		db.RefreshFromProvider(ctx, downloads, statuses, time.Now())
	}

	if vanished.State != StateError {
		t.Errorf("vanished download state = %q, want error (1 of 4 missing is well below massVanishFraction)", vanished.State)
	}
	for _, d := range downloads[1:] {
		if d.State == StateError {
			t.Errorf("found download %s incorrectly flagged error", d.ID)
		}
	}
}

// TestRefreshFromProvider_MassVanish_FoundRowsStillUpdateNormally proves the
// circuit breaker only suppresses handleMissingFromProvider — a row that IS
// present in the same suspicious-looking listing still gets its state
// refreshed normally, not frozen along with the missing ones.
func TestRefreshFromProvider_MassVanish_FoundRowsStillUpdateNormally(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	// 1 found out of 4 total: the other 3 missing is 75%, above
	// massVanishFraction, so this pass is still "suspicious" overall.
	downloads := seedManualDownloads(t, db, 4)
	found := downloads[0]
	statuses := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(found.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.75},
	}

	db.RefreshFromProvider(ctx, downloads, statuses, time.Now())

	if found.State != StateDownloading || found.Progress != 0.75 {
		t.Errorf("found download = state:%q progress:%v, want it updated normally despite the suspicious pass", found.State, found.Progress)
	}
	for _, d := range downloads[1:] {
		if d.MissingCount != 0 || d.State == StateError {
			t.Errorf("missing download %s = MissingCount:%d state:%q, want untouched by the circuit breaker", d.ID, d.MissingCount, d.State)
		}
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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

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
	db.RefreshFromProvider(ctx, []*Download{missing, good}, statuses, time.Now())

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

// TestRefreshFromProvider_StaleFetchDoesNotOvewriteFresherUpdate proves the
// real, live-observed race this guards against: multiple independent
// pollers (a compat shim's own reactive refresh, internal/importer's bulk
// tick, its fast per-download poll) can all be mid-flight against the
// provider for the same download at once, and a slower request that
// started earlier can finish — and try to write — after a faster one that
// started later. Found live: a real torrent's progress reported via
// GET /api/v2/torrents/info stuck at 13.9% while the same download's own
// database row (and TorBox's own API, queried directly) had already
// reached 50%+. Simulated here by calling RefreshFromProvider twice out of
// chronological order: a "fresher" 90% update with a later fetchedAt first,
// then a "stale" 50% update with an earlier fetchedAt second — the second
// call must not regress progress backward.
func TestRefreshFromProvider_StaleFetchDoesNotOvewriteFresherUpdate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	d.Progress = 0.1
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	now := time.Now()
	fresherFetchedAt := now
	staleFetchedAt := now.Add(-5 * time.Second)

	fresh := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.9},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, fresh, fresherFetchedAt)
	if d.Progress != 0.9 {
		t.Fatalf("progress after fresh update = %v, want 0.9", d.Progress)
	}

	stale := []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5},
	}
	db.RefreshFromProvider(ctx, []*Download{d}, stale, staleFetchedAt)
	if d.Progress != 0.9 {
		t.Errorf("progress after stale update = %v, want unchanged at 0.9 (stale write must be rejected)", d.Progress)
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.Progress != 0.9 {
		t.Errorf("persisted progress = %v, want 0.9 (stale write must not have reached the database either)", got.Progress)
	}
}

// TestRefreshFromProvider_LaterFetchAfterStaleStillApplies proves the guard
// only blocks genuinely out-of-order writes — a normal, chronologically
// later update still applies fine even after an earlier one was rejected
// for being stale, so a real download's progress keeps moving forward once
// pollers catch back up.
func TestRefreshFromProvider_LaterFetchAfterStaleStillApplies(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	d.Progress = 0.1
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	now := time.Now()
	db.RefreshFromProvider(ctx, []*Download{d}, []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.9},
	}, now)
	db.RefreshFromProvider(ctx, []*Download{d}, []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5},
	}, now.Add(-5*time.Second)) // stale, rejected

	db.RefreshFromProvider(ctx, []*Download{d}, []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateCompleted, Progress: 1.0},
	}, now.Add(10*time.Second)) // genuinely later, must apply

	if d.Progress != 1.0 || d.State != StateProviderCompleted {
		t.Errorf("state = %q progress = %v, want provider_completed/1.0 (a genuinely later update must still apply)", d.State, d.Progress)
	}
}

// TestRefreshFromProvider_CachesLiveStatus proves the fast-moving fields
// (ETA, torrent swarm info, usenet phase) that are deliberately never
// persisted to the downloads table are still readable afterward via
// LiveStatus — internal/api's own native API/UI reads exactly this cache
// rather than making its own synchronous provider call per request.
func TestRefreshFromProvider_CachesLiveStatus(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	if _, ok := db.LiveStatus(d.ID); ok {
		t.Error("LiveStatus() ok = true before any refresh, want false")
	}

	statuses := []debrid.DownloadStatus{{
		ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5,
		ETASeconds: 754, Seeders: 3, Leechers: 1, DownloadSpeedBytes: 191117,
		Airlocked: true,
	}}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())

	live, ok := db.LiveStatus(d.ID)
	if !ok {
		t.Fatal("LiveStatus() ok = false after a refresh, want true")
	}
	if live.ETASeconds != 754 || live.Seeders != 3 || live.Leechers != 1 || live.DownloadSpeedBytes != 191117 {
		t.Errorf("LiveStatus() = %+v, want ETASeconds=754 Seeders=3 Leechers=1 DownloadSpeedBytes=191117", live)
	}
	if !live.Airlocked {
		t.Error("LiveStatus() Airlocked = false, want true — provider-side AirLock must survive the cache round-trip")
	}
}

// TestRefreshFromProvider_StaleFetchDoesNotCacheStaleLiveStatus proves the
// same ordering guard that protects persisted writes also protects the
// LiveStatus cache — a stale response must not overwrite a fresher one's
// live snapshot either, even though LiveStatus itself is never persisted.
func TestRefreshFromProvider_StaleFetchDoesNotCacheStaleLiveStatus(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	now := time.Now()
	db.RefreshFromProvider(ctx, []*Download{d}, []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5, DownloadSpeedBytes: 900000},
	}, now)
	db.RefreshFromProvider(ctx, []*Download{d}, []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, Progress: 0.5, DownloadSpeedBytes: 100},
	}, now.Add(-5*time.Second)) // stale, rejected

	live, ok := db.LiveStatus(d.ID)
	if !ok {
		t.Fatal("LiveStatus() ok = false, want true")
	}
	if live.DownloadSpeedBytes != 900000 {
		t.Errorf("DownloadSpeedBytes = %d, want unchanged at 900000 (stale live snapshot must be rejected)", live.DownloadSpeedBytes)
	}
}

// TestInsertOrClaimForArr_ClaimsExistingManualRow is the regression for an
// *arr-requested download showing up in the web UI's Manual tab: whenever a
// row for the provider id already existed (TorBox deduping by content, or
// the importer's discovery pass adopting a just-added item first), the
// shim's plain insert tripped the UNIQUE constraint, the *arr add failed,
// and the only surviving row stayed Manual — so it was never auto-fetched.
func TestInsertOrClaimForArr_ClaimsExistingManualRow(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	discovered := newTestDownload(KindTorrent)
	discovered.AddedVia = AddedViaManual
	discovered.Category = ""
	discovered.SavePath = ""
	discovered.MissingCount = 2
	if err := db.InsertDownload(ctx, discovered); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	if err := db.UpdateMissingCount(ctx, discovered.ID, 2); err != nil {
		t.Fatalf("UpdateMissingCount() error = %v", err)
	}

	// Same provider id, as the shim would build it for an *arr add.
	arrAdd := newTestDownload(KindTorrent)
	arrAdd.AddedVia = AddedViaArr

	got, err := db.InsertOrClaimForArr(ctx, arrAdd)
	if err != nil {
		t.Fatalf("InsertOrClaimForArr() error = %v", err)
	}
	if got.ID != discovered.ID {
		t.Errorf("claimed row id = %q, want the existing row %q", got.ID, discovered.ID)
	}

	stored, err := db.GetDownloadByID(ctx, discovered.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if stored.AddedVia != AddedViaArr {
		t.Errorf("added_via = %q, want %q — an *arr add must claim the row", stored.AddedVia, AddedViaArr)
	}
	if stored.Category != arrAdd.Category {
		t.Errorf("category = %q, want %q", stored.Category, arrAdd.Category)
	}
	if stored.SavePath != arrAdd.SavePath {
		t.Errorf("save_path = %q, want %q", stored.SavePath, arrAdd.SavePath)
	}
	if stored.MissingCount != 0 {
		t.Errorf("missing_count = %d, want 0 (Manual-only bookkeeping)", stored.MissingCount)
	}

	// The claim must promote in place, never leave a second row behind.
	all, err := db.ListDownloads(ctx, KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(all) != 1 {
		t.Errorf("got %d rows, want 1 — claiming must not duplicate", len(all))
	}
}

func TestInsertOrClaimForArr_InsertsWhenUntracked(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindUsenet)
	d.AddedVia = AddedViaArr

	got, err := db.InsertOrClaimForArr(ctx, d)
	if err != nil {
		t.Fatalf("InsertOrClaimForArr() error = %v", err)
	}
	if got.ID != d.ID {
		t.Errorf("returned id = %q, want the inserted row %q", got.ID, d.ID)
	}
	stored, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if stored == nil || stored.AddedVia != AddedViaArr {
		t.Errorf("stored row = %+v, want an AddedViaArr row", stored)
	}
}

// A re-add that omits category/save_path must not blank out what the row
// already had — an empty save_path in particular silently breaks *arr's
// import step.
func TestInsertOrClaimForArr_KeepsExistingCategoryAndSavePathWhenBlank(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	existing := newTestDownload(KindTorrent)
	existing.AddedVia = AddedViaArr
	if err := db.InsertDownload(ctx, existing); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	blank := newTestDownload(KindTorrent)
	blank.Category = ""
	blank.SavePath = ""
	if _, err := db.InsertOrClaimForArr(ctx, blank); err != nil {
		t.Fatalf("InsertOrClaimForArr() error = %v", err)
	}

	stored, err := db.GetDownloadByID(ctx, existing.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if stored.Category != existing.Category {
		t.Errorf("category = %q, want it preserved as %q", stored.Category, existing.Category)
	}
	if stored.SavePath != existing.SavePath {
		t.Errorf("save_path = %q, want it preserved as %q", stored.SavePath, existing.SavePath)
	}
}

// The qBittorrent shim is keyed on infohash, so a claimed row missing one
// would be invisible to Sonarr/Radarr — but an existing hash is already the
// provider's own answer for this provider id and must not be replaced.
func TestInsertOrClaimForArr_FillsMissingHashButKeepsAnExistingOne(t *testing.T) {
	ctx := context.Background()

	t.Run("fills a missing hash", func(t *testing.T) {
		db := openTestDB(t)
		existing := newTestDownload(KindTorrent)
		existing.AddedVia = AddedViaManual
		existing.Hash = ""
		if err := db.InsertDownload(ctx, existing); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}

		arrAdd := newTestDownload(KindTorrent)
		arrAdd.Hash = "freshhash"
		if _, err := db.InsertOrClaimForArr(ctx, arrAdd); err != nil {
			t.Fatalf("InsertOrClaimForArr() error = %v", err)
		}

		stored, err := db.GetDownloadByID(ctx, existing.ID)
		if err != nil {
			t.Fatalf("GetDownloadByID() error = %v", err)
		}
		if stored.Hash != "freshhash" {
			t.Errorf("hash = %q, want it filled in as %q", stored.Hash, "freshhash")
		}
	})

	t.Run("keeps an existing hash", func(t *testing.T) {
		db := openTestDB(t)
		existing := newTestDownload(KindTorrent)
		existing.AddedVia = AddedViaManual
		existing.Hash = "originalhash"
		if err := db.InsertDownload(ctx, existing); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}

		arrAdd := newTestDownload(KindTorrent)
		arrAdd.Hash = "differenthash"
		if _, err := db.InsertOrClaimForArr(ctx, arrAdd); err != nil {
			t.Fatalf("InsertOrClaimForArr() error = %v", err)
		}

		stored, err := db.GetDownloadByID(ctx, existing.ID)
		if err != nil {
			t.Fatalf("GetDownloadByID() error = %v", err)
		}
		if stored.Hash != "originalhash" {
			t.Errorf("hash = %q, want it preserved as %q", stored.Hash, "originalhash")
		}
	})
}

// TestIsSuspectedMassVanish_IgnoresAlreadyErroredRows is the regression for
// the guard jamming on permanently. handleMissingFromProvider skips a row
// already in StateError, so counting those rows here meant the fraction
// could never recover: a row flagged error is absent from every future
// listing forever, so once enough had errored, missing-detection was
// disabled for that kind permanently and the warning fired every tick.
func TestIsSuspectedMassVanish_IgnoresAlreadyErroredRows(t *testing.T) {
	errored := func(id string) *Download {
		d := newTestDownload(KindUsenet)
		d.ProviderDownloadID = id
		d.AddedVia = AddedViaManual
		d.State = StateError
		return d
	}
	healthy := func(id string) *Download {
		d := newTestDownload(KindUsenet)
		d.ProviderDownloadID = id
		d.AddedVia = AddedViaManual
		d.State = StateDownloading
		return d
	}

	// The real shape observed live: three long-dead rows from a rotated API
	// key plus one healthy download that the listing does return.
	rows := []*Download{errored("dead-1"), errored("dead-2"), errored("dead-3"), healthy("alive")}
	byID := map[string]debrid.DownloadStatus{"alive": {ID: "alive"}}
	if isSuspectedMassVanish(rows, byID) {
		t.Error("mass-vanish suspected when the only non-errored row was present — the guard would stay jammed forever")
	}

	// A genuine mass-vanish must still trip: healthy rows, listing empty.
	live := []*Download{healthy("a"), healthy("b"), healthy("c")}
	if !isSuspectedMassVanish(live, map[string]debrid.DownloadStatus{}) {
		t.Error("mass-vanish not suspected when every tracked row vanished at once")
	}
}

// A deleted download must not leave its refresh-ordering record and cached
// live status behind — nothing else ever removes from that map, so without
// cleanup it grows for the lifetime of the process.
func TestDeleteDownload_ForgetsRefreshState(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	statuses := []debrid.DownloadStatus{{
		ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateDownloading, ETASeconds: 42,
	}}
	db.RefreshFromProvider(ctx, []*Download{d}, statuses, time.Now())
	if _, ok := db.LiveStatus(d.ID); !ok {
		t.Fatal("LiveStatus() ok = false after a refresh, want true")
	}

	if err := db.DeleteDownload(ctx, d.ID); err != nil {
		t.Fatalf("DeleteDownload() error = %v", err)
	}
	if _, ok := db.LiveStatus(d.ID); ok {
		t.Error("LiveStatus() still cached after the download was deleted — refreshState leaks")
	}
}
