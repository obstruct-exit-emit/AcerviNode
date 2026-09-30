package sabnzbd

import (
	"net/http"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// insertUsenetRow is a small helper for the Managed-only tests below.
func insertUsenetRow(t *testing.T, db *database.DB, id, name string, via database.AddedVia, state string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: fakeProviderName, ProviderDownloadID: "p-" + id,
		Kind: database.KindUsenet, Name: name, State: state, AddedVia: via,
		SavePath: "/downloads/tv-sonarr/" + name,
	}
	if err := db.InsertDownload(t.Context(), d); err != nil {
		t.Fatalf("InsertDownload(%s) error = %v", id, err)
	}
	return d
}

// getQueue/getHistory already live in server_test.go — reused here.

// TestQueue_OmitsManualDownloads — see internal/qbittorrent's
// TestInfo_OmitsManualDownloads for the full reasoning. Sonarr's SABnzbd
// GetQueue has no category filter either, so a Manual usenet download shows up
// in *arr's queue exactly the same way.
func TestQueue_OmitsManualDownloads(t *testing.T) {
	ts, db := newTestServerWithDB(t)

	insertUsenetRow(t, db, "arr-1", "Managed.Grab", database.AddedViaArr, database.StateDownloading)
	insertUsenetRow(t, db, "man-1", "Personal.Item", database.AddedViaManual, database.StateDownloading)

	slots := getQueue(t, ts.URL).Queue.Slots
	if len(slots) != 1 {
		t.Fatalf("mode=queue returned %d slots, want only the Managed one", len(slots))
	}
	if slots[0].NzoID != "arr-1" {
		t.Errorf("slot nzo_id = %q, want arr-1", slots[0].NzoID)
	}
}

// TestHistory_OmitsManualDownloads matters more than the queue case: a Manual
// row in history reports "Failed", which is the one status Sonarr maps to
// DownloadItemStatus.Failed — triggering its failed-download handling, so it
// blocklists a release and searches again over someone's personal download.
func TestHistory_OmitsManualDownloads(t *testing.T) {
	ts, db := newTestServerWithDB(t)

	insertUsenetRow(t, db, "arr-1", "Managed.Grab", database.AddedViaArr, database.StateReadyForImport)
	insertUsenetRow(t, db, "man-1", "Personal.Item", database.AddedViaManual, database.StateError)

	slots := getHistory(t, ts.URL).History.Slots
	if len(slots) != 1 {
		t.Fatalf("mode=history returned %d slots, want only the Managed one", len(slots))
	}
	if slots[0].NzoID != "arr-1" {
		t.Errorf("slot nzo_id = %q, want arr-1", slots[0].NzoID)
	}
}

// TestDelete_RefusesManualDownload — the SABnzbd half of refusing to act on a
// row *arr should never have seen.
func TestDelete_RefusesManualDownload(t *testing.T) {
	ts, db := newTestServerWithDB(t)

	insertUsenetRow(t, db, "man-1", "Personal.Item", database.AddedViaManual, database.StateReadyForImport)

	resp, err := http.Get(ts.URL + "/api?mode=history&name=delete&value=man-1&del_files=1&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("delete error = %v", err)
	}
	resp.Body.Close()

	got, err := db.GetDownloadByID(t.Context(), "man-1")
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got == nil {
		t.Fatal("the Manual download was deleted by an *arr request; it must be refused")
	}
}
