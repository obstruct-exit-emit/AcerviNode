package database

import (
	"context"
	"testing"
	"time"
)

// TestHasAnyDownloads proves the empty-database and non-empty cases both
// report correctly — internal/importer's discoverManual relies on this to
// tell a genuinely fresh install apart from an established instance seeing
// a provider+kind for the first time (see its own doc comment).
func TestHasAnyDownloads(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	got, err := db.HasAnyDownloads(ctx)
	if err != nil {
		t.Fatalf("HasAnyDownloads() error = %v", err)
	}
	if got {
		t.Error("HasAnyDownloads() = true on an empty database, want false")
	}

	if err := db.InsertDownload(ctx, &Download{
		ID: "dl-1", Provider: "torbox", ProviderDownloadID: "provider-1", Kind: KindTorrent,
		Hash: "abc123", Name: "Something", State: StateQueued, AddedVia: AddedViaManual,
	}); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	got, err = db.HasAnyDownloads(ctx)
	if err != nil {
		t.Fatalf("HasAnyDownloads() error = %v", err)
	}
	if !got {
		t.Error("HasAnyDownloads() = false after inserting a download, want true")
	}
}

// TestRecordDeletedDownload_RecentlyDeletedDownloadsRoundTrips proves a
// tombstoned download shows up in RecentlyDeletedDownloads for the same
// provider+kind, and only that provider+kind — see discoverManual, which
// uses this to avoid re-adopting a just-deleted item as a fresh discovery.
func TestRecordDeletedDownload_RecentlyDeletedDownloadsRoundTrips(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "provider-123"); err != nil {
		t.Fatalf("RecordDeletedDownload() error = %v", err)
	}

	got, err := db.RecentlyDeletedDownloads(ctx, "torbox", KindTorrent)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if !got["provider-123"] {
		t.Errorf("RecentlyDeletedDownloads() = %v, want it to contain provider-123", got)
	}

	// A different kind (or provider) must not see it.
	usenetResult, err := db.RecentlyDeletedDownloads(ctx, "torbox", KindUsenet)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads(usenet) error = %v", err)
	}
	if usenetResult["provider-123"] {
		t.Error("RecentlyDeletedDownloads(usenet) should not see a torrent-kind tombstone")
	}
}

// TestRecordDeletedDownload_PrunesOldTombstones proves a tombstone older
// than recentlyDeletedGracePeriod is excluded from
// RecentlyDeletedDownloads — the grace period is meant to be a short
// bridge over a provider's own delete-propagation delay, not a permanent
// exclusion list.
func TestRecordDeletedDownload_PrunesOldTombstones(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "old-one"); err != nil {
		t.Fatalf("RecordDeletedDownload() error = %v", err)
	}
	// Backdate it past the grace period directly — RecordDeletedDownload
	// itself always stamps "now", so this simulates time having passed
	// without needing an actual sleep.
	if _, err := db.ExecContext(ctx,
		`UPDATE deleted_downloads SET deleted_at = ? WHERE provider_download_id = 'old-one'`,
		time.Now().UTC().Add(-recentlyDeletedGracePeriod-time.Minute),
	); err != nil {
		t.Fatalf("backdate tombstone: %v", err)
	}

	got, err := db.RecentlyDeletedDownloads(ctx, "torbox", KindTorrent)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if got["old-one"] {
		t.Error("RecentlyDeletedDownloads() should not include a tombstone older than the grace period")
	}

	// Recording a new tombstone opportunistically prunes the old one from
	// the table entirely (not just filtering it out of the read).
	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "new-one"); err != nil {
		t.Fatalf("RecordDeletedDownload() error = %v", err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_downloads WHERE provider_download_id = 'old-one'`).Scan(&remaining); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if remaining != 0 {
		t.Error("old tombstone should have been pruned from the table, not just excluded from reads")
	}
}

// TestRecordDeletedDownload_OverwritesPreviousTombstone proves deleting the
// same provider_download_id twice (unusual, but shouldn't error) just
// refreshes the timestamp rather than failing on the primary key.
func TestRecordDeletedDownload_OverwritesPreviousTombstone(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "provider-123"); err != nil {
		t.Fatalf("first RecordDeletedDownload() error = %v", err)
	}
	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "provider-123"); err != nil {
		t.Fatalf("second RecordDeletedDownload() error = %v", err)
	}

	got, err := db.RecentlyDeletedDownloads(ctx, "torbox", KindTorrent)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if !got["provider-123"] {
		t.Error("RecentlyDeletedDownloads() should still contain provider-123 after a second tombstone")
	}
}
