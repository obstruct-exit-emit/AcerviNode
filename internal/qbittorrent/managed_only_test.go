package qbittorrent

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// insertRow is a small helper for the Managed-only tests below.
func insertRow(t *testing.T, db *database.DB, id, hash, name string, via database.AddedVia, state string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: fakeProviderName, ProviderDownloadID: "p-" + id,
		Kind: database.KindTorrent, Hash: hash, Name: name,
		State: state, AddedVia: via,
	}
	if err := db.InsertDownload(t.Context(), d); err != nil {
		t.Fatalf("InsertDownload(%s) error = %v", id, err)
	}
	return d
}

// TestInfo_OmitsManualDownloads is the wall's missing half.
//
// The shim already records AddedViaArr on everything it adds, and its own
// comments say it "only exists for *arr apps" — but it read every row of the
// kind back out, Manual included. Sonarr's and Radarr's GetItems loops read a
// torrent's category and never filter on it (confirmed against their real
// source), so a discovered Manual download — the operator's personal library —
// landed in *arr's queue. There it blocks grabs via QueueSpecification, shows
// errored rows as permanent warnings, and sits forever at "downloading" since
// a Manual download is never auto-fetched.
func TestInfo_OmitsManualDownloads(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	insertRow(t, db, "arr-1", "aaa1", "Managed Grab", database.AddedViaArr, database.StateDownloading)
	insertRow(t, db, "man-1", "bbb2", "Personal Library Item", database.AddedViaManual, database.StateProviderCompleted)

	items := getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 {
		names := make([]string, len(items))
		for i, it := range items {
			names[i] = it.Name
		}
		t.Fatalf("/info returned %d torrents (%s), want only the Managed one", len(items), strings.Join(names, ", "))
	}
	if items[0].Hash != "aaa1" {
		t.Errorf("/info hash = %q, want the Managed grab aaa1", items[0].Hash)
	}
}

// TestDelete_RefusesManualDownload closes the harm directly rather than only
// hiding it: an *arr app that somehow names a Manual download's hash must not
// be able to remove the operator's own download.
func TestDelete_RefusesManualDownload(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	insertRow(t, db, "man-1", "bbb2", "Personal", database.AddedViaManual, database.StateProviderCompleted)

	resp, err := client.PostForm(ts.URL+"/api/v2/torrents/delete", url.Values{
		"hashes":      {"bbb2"},
		"deleteFiles": {"true"},
	})
	if err != nil {
		t.Fatalf("POST /delete error = %v", err)
	}
	resp.Body.Close()

	got, err := db.GetDownloadByHash(t.Context(), "bbb2")
	if err != nil {
		t.Fatalf("GetDownloadByHash() error = %v", err)
	}
	if got == nil {
		t.Fatal("the Manual download was deleted by an *arr request; it must be refused")
	}
}

// TestProperties_NotFoundForManualDownload keeps the by-hash read paths
// consistent with the listing — a row *arr cannot see should not be one it can
// interrogate either.
func TestProperties_NotFoundForManualDownload(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	insertRow(t, db, "man-1", "bbb2", "Personal", database.AddedViaManual, database.StateProviderCompleted)

	resp, err := client.Get(ts.URL + "/api/v2/torrents/properties?hash=bbb2")
	if err != nil {
		t.Fatalf("GET /properties error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /properties for a Manual download = %d, want 404", resp.StatusCode)
	}
}

// TestSetCategory_LeavesManualDownloadAlone: "all" is a legal hashes value on
// real qBittorrent, so a post-import category step must not relabel every
// Manual download in the account.
func TestSetCategory_LeavesManualDownloadAlone(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	insertRow(t, db, "man-1", "bbb2", "Personal", database.AddedViaManual, database.StateProviderCompleted)

	resp, err := client.PostForm(ts.URL+"/api/v2/torrents/setCategory", url.Values{
		"hashes":   {"all"},
		"category": {"tv-sonarr-imported"},
	})
	if err != nil {
		t.Fatalf("POST /setCategory error = %v", err)
	}
	resp.Body.Close()

	got, err := db.GetDownloadByHash(t.Context(), "bbb2")
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByHash() = %v, %v", got, err)
	}
	if got.Category != "" {
		t.Errorf("Manual download category = %q, want it untouched by an *arr setCategory", got.Category)
	}
}
