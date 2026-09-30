package database

import (
	"testing"
)

// TestListManagedDownloads_ExcludesManual is the database half of making the
// compat shims Managed-only.
//
// An *arr app must never be shown a Manual download. Sonarr's and Radarr's own
// GetItems loops read a torrent's category but never filter on it (confirmed
// against their real source), so every row a shim reports lands in their queue
// — where a Manual copy of an episode makes QueueSpecification reject the real
// grab, silently, and looking like an indexer problem.
func TestListManagedDownloads_ExcludesManual(t *testing.T) {
	ctx := t.Context()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	managed := &Download{
		ID: "m-1", Provider: "p", ProviderDownloadID: "pm-1", Kind: KindTorrent,
		Hash: "aaa", Name: "Managed Grab", State: StateDownloading, AddedVia: AddedViaArr,
	}
	manual := &Download{
		ID: "u-1", Provider: "p", ProviderDownloadID: "pu-1", Kind: KindTorrent,
		Hash: "bbb", Name: "Personal Library Item", State: StateProviderCompleted, AddedVia: AddedViaManual,
	}
	otherKind := &Download{
		ID: "m-2", Provider: "p", ProviderDownloadID: "pm-2", Kind: KindUsenet,
		Hash: "ccc", Name: "Managed Usenet", State: StateQueued, AddedVia: AddedViaArr,
	}
	for _, d := range []*Download{managed, manual, otherKind} {
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload(%s) error = %v", d.ID, err)
		}
	}

	rows, err := db.ListManagedDownloads(ctx, KindTorrent)
	if err != nil {
		t.Fatalf("ListManagedDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListManagedDownloads() returned %d rows, want 1", len(rows))
	}
	if rows[0].ID != "m-1" {
		t.Errorf("row = %q, want the Managed torrent m-1", rows[0].ID)
	}

	// The unfiltered listing still sees everything — the native API and web UI
	// are exactly where a Manual download is supposed to be visible.
	all, err := db.ListDownloads(ctx, KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListDownloads() returned %d rows, want 2 (Manual still visible natively)", len(all))
	}
}

// TestGetManagedDownloadByHash_RefusesManual covers the by-hash lookups. A
// shim that hides a Manual download from its listing but still acts on one by
// hash would leave *arr's "remove from queue" able to delete the operator's
// own download, files included.
func TestGetManagedDownloadByHash_RefusesManual(t *testing.T) {
	ctx := t.Context()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	manual := &Download{
		ID: "u-1", Provider: "p", ProviderDownloadID: "pu-1", Kind: KindTorrent,
		Hash: "deadbeef", Name: "Personal", State: StateProviderCompleted, AddedVia: AddedViaManual,
	}
	if err := db.InsertDownload(ctx, manual); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	got, err := db.GetManagedDownloadByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("GetManagedDownloadByHash() error = %v", err)
	}
	if got != nil {
		t.Errorf("GetManagedDownloadByHash() = %q, want nil for a Manual download", got.ID)
	}

	// Same row, unfiltered: still found, so this is a scoping decision rather
	// than the row having gone missing.
	if plain, err := db.GetDownloadByHash(ctx, "deadbeef"); err != nil || plain == nil {
		t.Fatalf("GetDownloadByHash() = %v, %v; want the row still findable unfiltered", plain, err)
	}
}
