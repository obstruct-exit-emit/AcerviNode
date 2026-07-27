package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// fakeProvider satisfies both torrentAdder and usenetAdder — every test that
// previously only exercised Delete keeps working unchanged (the extra
// methods are simply unused in those cases), and add-download tests
// configure addID/addErr/statusResp/statusErr as needed.
type fakeProvider struct {
	providerName string

	deleteCalled bool
	deleteID     debrid.ProviderDownloadID
	deleteFiles  bool
	deleteErr    error

	addID         debrid.ProviderDownloadID
	addErr        error
	addedMagnet   string
	addedURL      string
	addedFilename string
	addedFile     []byte

	statusResp debrid.DownloadStatus
	statusErr  error
}

func (f *fakeProvider) Name() string {
	if f.providerName == "" {
		return "fake"
	}
	return f.providerName
}

func (f *fakeProvider) Delete(_ context.Context, id debrid.ProviderDownloadID, deleteFiles bool) error {
	f.deleteCalled = true
	f.deleteID = id
	f.deleteFiles = deleteFiles
	return f.deleteErr
}

func (f *fakeProvider) AddMagnet(_ context.Context, magnetURI string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedMagnet = magnetURI
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddTorrentFile(_ context.Context, filename string, data []byte, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedFilename = filename
	f.addedFile = data
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddNZBURL(_ context.Context, link string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedURL = link
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddNZBFile(_ context.Context, filename string, data []byte, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedFilename = filename
	f.addedFile = data
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) Status(_ context.Context, _ debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	if f.statusErr != nil {
		return debrid.DownloadStatus{}, f.statusErr
	}
	return f.statusResp, nil
}

type fakeSettings struct {
	configured bool
	setCalls   []string
	setErr     error
	apiKey     string
	regenCalls int
	regenErr   error
	general    GeneralInfo
}

func (f *fakeSettings) TorBoxConfigured() bool { return f.configured }

func (f *fakeSettings) SetTorBoxAPIKey(_ context.Context, apiKey string) error {
	f.setCalls = append(f.setCalls, apiKey)
	if f.setErr != nil {
		return f.setErr
	}
	f.configured = true
	return nil
}

// APIKey defaults to "secret" (matching authedRequest below) so tests that
// don't care about auth can pass a zero-value *fakeSettings.
func (f *fakeSettings) APIKey() string {
	if f.apiKey == "" {
		return "secret"
	}
	return f.apiKey
}

func (f *fakeSettings) RegenerateAPIKey(_ context.Context) (string, error) {
	f.regenCalls++
	if f.regenErr != nil {
		return "", f.regenErr
	}
	f.apiKey = "regenerated-key"
	return f.apiKey, nil
}

func (f *fakeSettings) General() GeneralInfo { return f.general }

func newTestServer(t *testing.T, torrentProvider torrentAdder, usenetProvider usenetAdder, settings Settings) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if settings == nil {
		settings = &fakeSettings{}
	}
	return NewServer("dev", db, torrentProvider, usenetProvider, settings), db
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
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{configured: true})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "torbox") {
		t.Errorf("body = %q, want it to mention torbox", got)
	}
}

// TestHandleProviders_UnconfiguredReturnsEmptyArray guards against a real
// bug found during manual testing: a nil slice marshals to JSON `null`,
// which the embedded UI's `providers.length` check would throw on.
func TestHandleProviders_UnconfiguredReturnsEmptyArray(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{configured: false})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want [] (not null)", got)
	}
}

func TestHandleGetProviderSettings(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{configured: true})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]providerSettingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got["torbox"].Configured {
		t.Errorf("got = %+v, want torbox.configured = true", got)
	}
	// The actual key must never appear in the response body.
	if strings.Contains(rec.Body.String(), "api_key") {
		t.Errorf("response leaked a field named api_key: %s", rec.Body.String())
	}
}

func TestHandleSetTorBoxAPIKey(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/providers/torbox", strings.NewReader(`{"api_key":"new-torbox-key"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if len(settings.setCalls) != 1 || settings.setCalls[0] != "new-torbox-key" {
		t.Errorf("SetTorBoxAPIKey calls = %v, want one call with new-torbox-key", settings.setCalls)
	}
}

func TestHandleSetTorBoxAPIKey_RejectsEmptyKey(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/providers/torbox", strings.NewReader(`{"api_key":""}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if len(settings.setCalls) != 0 {
		t.Errorf("SetTorBoxAPIKey should not have been called for an empty key")
	}
}

func TestHandleSetTorBoxAPIKey_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/providers/torbox", strings.NewReader(`{"api_key":"x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetGeneralSettings(t *testing.T) {
	settings := &fakeSettings{general: GeneralInfo{
		APIKey: "secret", Port: 7846, DataDir: "./data", DownloadDir: "./downloads",
		LogLevel: "info", ImportIntervalSeconds: 10, ImportMaxRetries: 5,
	}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/general"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got GeneralInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != settings.general {
		t.Errorf("got = %+v, want %+v", got, settings.general)
	}
}

func TestHandleGetGeneralSettings_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings/general", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRegenerateAPIKey(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/api-key/regenerate"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if settings.regenCalls != 1 {
		t.Errorf("RegenerateAPIKey calls = %d, want 1", settings.regenCalls)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["api_key"] != "regenerated-key" {
		t.Errorf("api_key = %q, want regenerated-key", got["api_key"])
	}
}

func TestHandleRegenerateAPIKey_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/api-key/regenerate", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
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

func TestHandleGetDownload_ExposesRetryInfo(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	nextRetry := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	if err := db.UpdateDownloadRetry(context.Background(), d.ID, 2, nextRetry, "connection reset"); err != nil {
		t.Fatalf("UpdateDownloadRetry() error = %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", got.RetryCount)
	}
	if got.NextRetryAt == nil {
		t.Fatal("NextRetryAt is nil, want it set")
	}
	if got.ErrorMessage != "connection reset" {
		t.Errorf("ErrorMessage = %q, want connection reset", got.ErrorMessage)
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
	torrentDeleter := &fakeProvider{}
	srv, db := newTestServer(t, torrentDeleter, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID+"?deleteFiles=true"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if !torrentDeleter.deleteCalled {
		t.Error("expected torrent provider Delete to be called")
	}
	if torrentDeleter.deleteID != debrid.ProviderDownloadID("p1") {
		t.Errorf("provider id = %q, want p1", torrentDeleter.deleteID)
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
	torrentDeleter := &fakeProvider{}
	usenetDeleter := &fakeProvider{}
	srv, db := newTestServer(t, torrentDeleter, usenetDeleter, nil)
	d := seedDownload(t, db, database.KindUsenet, "p2")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if torrentDeleter.deleteCalled {
		t.Error("torrent provider should not have been called for a usenet download")
	}
	if !usenetDeleter.deleteCalled {
		t.Error("usenet provider should have been called")
	}
}

func TestHandleDeleteDownload_ProviderErrorStillCleansUpLocally(t *testing.T) {
	failing := &fakeProvider{deleteErr: context.DeadlineExceeded}
	srv, db := newTestServer(t, failing, nil, nil)
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

// multipartRequest builds a POST request against target with the given form
// fields, and optionally one file part — mirrors how a real browser's
// FormData (or *arr apps' own add calls) would submit it.
func multipartRequest(t *testing.T, target string, fields map[string]string, fileField, filename string, fileData []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if fileData != nil {
		part, err := writer.CreateFormFile(fileField, filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(fileData); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, target, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

const testMagnet = "magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=Some.Release.Name"

func TestHandleAddTorrent_Magnet(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "999", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet, "category": "movies"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedMagnet != testMagnet {
		t.Errorf("AddMagnet called with %q, want %q", provider.addedMagnet, testMagnet)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Protocol != "torrent" {
		t.Errorf("protocol = %q, want torrent", got.Protocol)
	}
	if got.Name != "Some.Release.Name" {
		t.Errorf("name = %q, want the magnet's dn (fallback, since provider status wasn't available yet)", got.Name)
	}
	if got.Hash != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("hash = %q, want the magnet's infohash, lowercased", got.Hash)
	}
	if got.Category != "movies" {
		t.Errorf("category = %q, want movies", got.Category)
	}

	d, err := db.GetDownloadByID(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if d == nil {
		t.Fatal("download was not persisted")
	}
	if d.ProviderDownloadID != "999" || d.Provider != "torbox" {
		t.Errorf("provider_download_id/provider = %q/%q, want 999/torbox", d.ProviderDownloadID, d.Provider)
	}
}

// TestHandleAddTorrent_DedupedByProviderReturnsExisting proves adding a
// magnet whose provider dedupes to an already-tracked torrent_id (e.g.
// TorBox recognizing an already-cached hash and handing back the same ID as
// an earlier add) doesn't 500 on the (provider, provider_download_id) UNIQUE
// constraint — it returns the existing row with 200 instead of creating a
// duplicate. Found via a real TorBox add during manual verification.
func TestHandleAddTorrent_DedupedByProviderReturnsExisting(t *testing.T) {
	provider := &fakeProvider{addID: "999", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, provider, nil, nil)

	first := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, first)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first add status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var firstResp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	second := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, second)
	if rec.Code != http.StatusOK {
		t.Fatalf("second add status = %d, want 200 (already tracked), body=%s", rec.Code, rec.Body.String())
	}
	var secondResp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if secondResp.ID != firstResp.ID {
		t.Errorf("second add ID = %q, want the same as the first add (%q)", secondResp.ID, firstResp.ID)
	}

	rows, err := db.ListAllDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListAllDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("len(rows) = %d, want 1 (no duplicate row created)", len(rows))
	}
}

func TestHandleAddTorrent_UsesProviderStatusWhenAvailable(t *testing.T) {
	provider := &fakeProvider{
		addID: "999",
		statusResp: debrid.DownloadStatus{
			Name: "Real Name From Provider", Hash: "REALHASH", SizeBytes: 12345,
			Progress: 0.5, State: debrid.StateDownloading,
		},
	}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Real Name From Provider" {
		t.Errorf("name = %q, want the provider's real name, not the magnet-derived fallback", got.Name)
	}
	if got.Hash != "realhash" {
		t.Errorf("hash = %q, want the provider's hash (lowercased)", got.Hash)
	}
	if got.State != database.StateDownloading {
		t.Errorf("state = %q, want downloading", got.State)
	}
}

func TestHandleAddTorrent_File(t *testing.T) {
	provider := &fakeProvider{addID: "999", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"category": "tv"}, "file", "release.torrent", []byte("fake torrent bytes"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedFilename != "release.torrent" || string(provider.addedFile) != "fake torrent bytes" {
		t.Errorf("AddTorrentFile called with filename=%q data=%q", provider.addedFilename, provider.addedFile)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "release.torrent" {
		t.Errorf("name = %q, want the uploaded filename (fallback)", got.Name)
	}
}

func TestHandleAddTorrent_RequiresMagnetOrFile(t *testing.T) {
	srv, _ := newTestServer(t, &fakeProvider{}, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"category": "tv"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAddTorrent_NoProviderConfigured(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddTorrent_ProviderReturnsErrNoProvider(t *testing.T) {
	provider := &fakeProvider{addErr: debrid.ErrNoProvider}
	srv, _ := newTestServer(t, provider, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddTorrent_ProviderError(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: rate limited")}
	srv, _ := newTestServer(t, provider, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleAddTorrent_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, &fakeProvider{}, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAddUsenet_URL(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "nzb-1", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, nil, provider, nil)

	const nzbURL = "https://example.com/release.nzb"
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": nzbURL, "category": "movies"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedURL != nzbURL {
		t.Errorf("AddNZBURL called with %q, want %q", provider.addedURL, nzbURL)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Protocol != "usenet" {
		t.Errorf("protocol = %q, want usenet", got.Protocol)
	}
	if got.Name != nzbURL {
		t.Errorf("name = %q, want the URL (fallback, since provider status wasn't available yet)", got.Name)
	}

	d, err := db.GetDownloadByID(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if d == nil || d.ProviderDownloadID != "nzb-1" || d.Kind != database.KindUsenet {
		t.Errorf("persisted row = %+v, want provider_download_id=nzb-1 kind=usenet", d)
	}
}

func TestHandleAddUsenet_File(t *testing.T) {
	provider := &fakeProvider{addID: "nzb-2", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServer(t, nil, provider, nil)

	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"category": "tv"}, "file", "release.nzb", []byte("fake nzb bytes"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedFilename != "release.nzb" || string(provider.addedFile) != "fake nzb bytes" {
		t.Errorf("AddNZBFile called with filename=%q data=%q", provider.addedFilename, provider.addedFile)
	}
}

func TestHandleAddUsenet_RequiresURLOrFile(t *testing.T) {
	srv, _ := newTestServer(t, nil, &fakeProvider{}, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"category": "tv"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAddUsenet_NoProviderConfigured(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": "https://example.com/x.nzb"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddUsenet_ProviderError(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: rate limited")}
	srv, _ := newTestServer(t, nil, provider, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": "https://example.com/x.nzb"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleAddUsenet_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, &fakeProvider{}, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": "https://example.com/x.nzb"}, "", "", nil)
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
