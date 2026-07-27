package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

type fakeDeleter struct {
	called      bool
	lastID      debrid.ProviderDownloadID
	deleteFiles bool
	err         error
}

func (f *fakeDeleter) Delete(_ context.Context, id debrid.ProviderDownloadID, deleteFiles bool) error {
	f.called = true
	f.lastID = id
	f.deleteFiles = deleteFiles
	return f.err
}

func newTestServer(t *testing.T, providers []ProviderStatus, torrentProvider, usenetProvider deleter) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewServer("secret", "dev", providers, db, torrentProvider, usenetProvider), db
}

func authedRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func TestHandleHealth_NoAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleVersion_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/version"))
	if rec.Code != http.StatusOK {
		t.Errorf("correct key: status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("version response body is empty")
	}
}

func TestHandleProviders_ReturnsConfigured(t *testing.T) {
	srv, _ := newTestServer(t, []ProviderStatus{
		{Name: "torbox", TorrentCapable: true, UsenetCapable: true},
	}, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "torbox") {
		t.Errorf("body = %q, want it to mention torbox", got)
	}
}

// TestHandleProviders_NilProvidersReturnsEmptyArray guards against a real
// bug found during manual testing: a nil providers slice (the case when no
// provider is configured — see cmd/acervinode's buildProviders) marshals to
// JSON `null`, which the embedded UI's `providers.length` check would throw
// on. NewServer must normalize nil to an empty slice.
func TestHandleProviders_NilProvidersReturnsEmptyArray(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want [] (not null)", got)
	}
}

func seedDownload(t *testing.T, db *database.DB, kind database.Kind, providerDownloadID string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: "dl-" + providerDownloadID, Provider: "fake", ProviderDownloadID: providerDownloadID,
		Kind: kind, Hash: "hash-" + providerDownloadID, Name: "Test Download",
		Category: "tv-sonarr", SizeBytes: 1024, State: database.StateDownloading, Progress: 0.5,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}
	return d
}

func TestHandleListDownloads(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	seedDownload(t, db, database.KindTorrent, "p1")
	seedDownload(t, db, database.KindUsenet, "p2")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestHandleListDownloads_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/downloads", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetDownload(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")
	if err := db.ReplaceDownloadFiles(context.Background(), d.ID, []*database.DownloadFile{
		{ID: "f1", Path: "movie.mkv", SizeBytes: 1024},
	}); err != nil {
		t.Fatalf("ReplaceDownloadFiles() error = %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		downloadResponse
		Files []downloadFileResponse `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != d.ID {
		t.Errorf("ID = %q, want %q", got.ID, d.ID)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "movie.mkv" {
		t.Errorf("files = %+v", got.Files)
	}
}

func TestHandleGetDownload_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/does-not-exist"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleDeleteDownload(t *testing.T) {
	torrentDeleter := &fakeDeleter{}
	srv, db := newTestServer(t, nil, torrentDeleter, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID+"?deleteFiles=true"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if !torrentDeleter.called {
		t.Error("expected torrent provider Delete to be called")
	}
	if torrentDeleter.lastID != debrid.ProviderDownloadID("p1") {
		t.Errorf("provider id = %q, want p1", torrentDeleter.lastID)
	}
	if !torrentDeleter.deleteFiles {
		t.Error("deleteFiles should have been true")
	}

	got, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got != nil {
		t.Errorf("download still present after delete: %+v", got)
	}
}

func TestHandleDeleteDownload_UsenetKindUsesUsenetProvider(t *testing.T) {
	torrentDeleter := &fakeDeleter{}
	usenetDeleter := &fakeDeleter{}
	srv, db := newTestServer(t, nil, torrentDeleter, usenetDeleter)
	d := seedDownload(t, db, database.KindUsenet, "p2")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if torrentDeleter.called {
		t.Error("torrent provider should not have been called for a usenet download")
	}
	if !usenetDeleter.called {
		t.Error("usenet provider should have been called")
	}
}

func TestHandleDeleteDownload_ProviderErrorStillCleansUpLocally(t *testing.T) {
	failing := &fakeDeleter{err: context.DeadlineExceeded}
	srv, db := newTestServer(t, nil, failing, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 even though the provider call failed", rec.Code)
	}

	got, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got != nil {
		t.Errorf("download still present after delete despite provider error: %+v", got)
	}
}
