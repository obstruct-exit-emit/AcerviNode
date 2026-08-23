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

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "provider-123", true); err != nil {
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

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "old-one", true); err != nil {
		t.Fatalf("RecordDeletedDownload() error = %v", err)
	}
	// Backdate it past its own expiry directly — RecordDeletedDownload
	// always stamps "now", so this simulates time having passed without an
	// actual sleep. Both columns move together to stay self-consistent;
	// expires_at is what reads and pruning key on now that each tombstone
	// carries its own lifetime.
	past := time.Now().UTC().Add(-recentlyDeletedGracePeriod - time.Minute)
	if _, err := db.ExecContext(ctx,
		`UPDATE deleted_downloads SET deleted_at = ?, expires_at = ? WHERE provider_download_id = 'old-one'`,
		past, past,
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
	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "new-one", true); err != nil {
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

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "provider-123", true); err != nil {
		t.Fatalf("first RecordDeletedDownload() error = %v", err)
	}
	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "provider-123", true); err != nil {
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

// TestRecordDeletedDownload_UnconfirmedDeleteOutlivesShortGrace is the
// regression for a ghost download coming back after a *failed* provider
// delete. The short grace period assumes the provider-side delete worked,
// so the id is gone for good and only the provider's listing lag needs
// bridging. When that call fails the item is still on the account, and a
// short tombstone doesn't prevent the ghost — it just delays it.
//
// Reproduced live before this fix: two downloads were deleted while the
// account was rate-limited, both provider deletes returned 429, and both
// reappeared as Manual downloads once the five minutes lapsed — including
// one that had been Managed, which came back in the wrong tab.
func TestRecordDeletedDownload_UnconfirmedDeleteOutlivesShortGrace(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "confirmed", true); err != nil {
		t.Fatalf("RecordDeletedDownload(confirmed) error = %v", err)
	}
	if err := db.RecordDeletedDownload(ctx, "torbox", KindTorrent, "unconfirmed", false); err != nil {
		t.Fatalf("RecordDeletedDownload(unconfirmed) error = %v", err)
	}

	// Assert the lifetime the production code actually stored, rather than
	// writing one here and checking it back — the latter would pass even if
	// RecordDeletedDownload ignored providerConfirmed entirely.
	storedExpiry := func(id string) time.Time {
		t.Helper()
		var at time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT expires_at FROM deleted_downloads WHERE provider_download_id = ?`, id).Scan(&at); err != nil {
			t.Fatalf("read expires_at for %s: %v", id, err)
		}
		return at.UTC()
	}
	confirmedExpiry, unconfirmedExpiry := storedExpiry("confirmed"), storedExpiry("unconfirmed")
	shortCutoff := time.Now().UTC().Add(recentlyDeletedGracePeriod + time.Minute)
	if confirmedExpiry.After(shortCutoff) {
		t.Errorf("confirmed delete expires at %v, want within the short grace period", confirmedExpiry)
	}
	if !unconfirmedExpiry.After(shortCutoff) {
		t.Errorf("failed provider delete expires at %v, want well beyond the short grace period — the item is still on the account", unconfirmedExpiry)
	}

	// Now simulate that much time actually passing, shifting each row by the
	// same amount so whatever lifetime production chose is preserved.
	shift := recentlyDeletedGracePeriod + time.Minute
	for _, id := range []string{"confirmed", "unconfirmed"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE deleted_downloads SET deleted_at = ?, expires_at = ? WHERE provider_download_id = ?`,
			time.Now().UTC().Add(-shift), storedExpiry(id).Add(-shift), id,
		); err != nil {
			t.Fatalf("shift tombstone %s: %v", id, err)
		}
	}

	got, err := db.RecentlyDeletedDownloads(ctx, "torbox", KindTorrent)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if got["confirmed"] {
		t.Error("a confirmed delete's tombstone should expire after the short grace period")
	}
	if !got["unconfirmed"] {
		t.Error("a failed provider delete must stay tombstoned past the short grace period — otherwise the item, which is still on the account, comes back as a ghost")
	}
}
