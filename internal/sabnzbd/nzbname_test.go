package sabnzbd

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// postAddFile uploads an NZB the way Sonarr and Radarr do: mode=addfile with
// the file in the "name" part, and — unless nzbName is given — no nzbname
// field at all. Confirmed against both apps' SabnzbdProxy.DownloadNzb, which
// sends only the file upload, the category and the priority.
func postAddFile(t *testing.T, baseURL, filename, nzbName string) map[string]any {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("mode", "addfile")
	w.WriteField("apikey", testAPIKey)
	w.WriteField("cat", "tv-sonarr")
	if nzbName != "" {
		w.WriteField("nzbname", nzbName)
	}
	part, err := w.CreateFormFile("name", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	part.Write([]byte(`<?xml version="1.0"?><nzb><file subject="x"></file></nzb>`))
	w.Close()

	resp, err := http.Post(baseURL+"/api", w.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("addfile error = %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func serverWith(t *testing.T, provider *fakeProvider) (*httptest.Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ts := httptest.NewServer(NewServer(testRegistry(provider), db, staticAPIKey(testAPIKey)))
	t.Cleanup(ts.Close)
	return ts, db
}

// TestAddFile_WithoutNZBNameNamesTheJobFromTheFile.
//
// Sonarr and Radarr never send nzbname, so the job name this shim used was
// always empty for them. Real SABnzbd names a job after its file, minus the
// extension, in exactly this case — and TorBox named an uploaded
// "acervinode-test.nzb" exactly "acervinode-test" when observed live. Telling
// the provider that name explicitly makes what we record and what it later
// reports identical by construction, which is what name-matching needs.
func TestAddFile_WithoutNZBNameNamesTheJobFromTheFile(t *testing.T) {
	provider := newFakeProvider()
	ts, db := serverWith(t, provider)

	out := postAddFile(t, ts.URL, "Some.Show.S01E01.1080p.WEB.nzb", "")
	if out["status"] != true {
		t.Fatalf("addfile = %+v, want status:true", out)
	}
	const want = "Some.Show.S01E01.1080p.WEB"
	if provider.lastAddName != want {
		t.Errorf("provider was asked to name the job %q, want %q", provider.lastAddName, want)
	}
	rows, err := db.ListManagedDownloads(t.Context(), database.KindUsenet)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListManagedDownloads() = %d rows, %v", len(rows), err)
	}
	if rows[0].Name != want {
		t.Errorf("row name = %q, want %q", rows[0].Name, want)
	}
}

// TestAddFile_FailedAddIsReconcilableByName is the one that mattered.
//
// When an add fails after the request went out, the shim records a pending
// *arr add so discovery can recognise the item as Managed if the provider took
// it anyway. Usenet has no hash, so the name is the *only* thing that can match
// — and with nzbname never sent, it was recorded empty. The reconciliation for
// "a Managed download turned into a Manual one" has therefore never been able
// to fire for a Sonarr or Radarr usenet grab.
func TestAddFile_FailedAddIsReconcilableByName(t *testing.T) {
	provider := newFakeProvider()
	provider.addErr = errors.New("provider: reply lost")
	ts, db := serverWith(t, provider)

	out := postAddFile(t, ts.URL, "Some.Show.S01E01.1080p.WEB.nzb", "")
	if out["status"] != false {
		t.Fatalf("addfile = %+v, want status:false for a failed add", out)
	}

	// The same claim discovery makes when it finds the item untracked.
	claimed, err := db.ClaimPendingArrAdd(t.Context(), fakeProviderName, database.KindUsenet, "", "Some.Show.S01E01.1080p.WEB")
	if err != nil {
		t.Fatalf("ClaimPendingArrAdd() error = %v", err)
	}
	if claimed == nil {
		t.Fatal("the failed add could not be claimed by the name the provider will report — it was recorded without one")
	}
	if claimed.Category != "tv-sonarr" {
		t.Errorf("claimed category = %q, want tv-sonarr", claimed.Category)
	}
}

// TestAddFile_NZBNameStillWins — a client that does send nzbname keeps it.
func TestAddFile_NZBNameStillWins(t *testing.T) {
	provider := newFakeProvider()
	ts, _ := serverWith(t, provider)

	postAddFile(t, ts.URL, "ugly-indexer-id-8841.nzb", "Nice.Release.Name")
	if provider.lastAddName != "Nice.Release.Name" {
		t.Errorf("provider was asked to name the job %q, want the supplied nzbname", provider.lastAddName)
	}
}

func TestNZBJobName(t *testing.T) {
	tests := []struct{ nzbName, filename, want string }{
		{"", "Some.Release.nzb", "Some.Release"},
		{"", "Some.Release.NZB", "Some.Release"},
		{"", "Some.Release", "Some.Release"},
		{"", "folder/sub/Some.Release.nzb", "Some.Release"},
		{"", `C:\uploads\Some.Release.nzb`, "Some.Release"},
		{"  Given Name  ", "whatever.nzb", "Given Name"},
		{"", "", ""},
		{"", ".nzb", ""},
		{"", "  Spaced.Release.nzb  ", "Spaced.Release"},
	}
	for _, tc := range tests {
		if got := nzbJobName(tc.nzbName, tc.filename); got != tc.want {
			t.Errorf("nzbJobName(%q, %q) = %q, want %q", tc.nzbName, tc.filename, got, tc.want)
		}
	}
}
