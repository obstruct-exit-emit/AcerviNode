package qbittorrent

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// postMultipart mirrors how *arr apps actually call torrents/add: a real
// multipart/form-data POST, not application/x-www-form-urlencoded.
func postMultipart(t *testing.T, client *http.Client, targetURL string, fields map[string]string) *http.Response {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, targetURL, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

const testMagnet = "magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=Some.Release.Name"

// staticAPIKey satisfies settingsSource for tests that don't need a live,
// changeable key or download dir.
type staticAPIKey string

func (k staticAPIKey) APIKey() string      { return string(k) }
func (k staticAPIKey) DownloadDir() string { return "/downloads" }

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewServer(newFakeProvider(), db, staticAPIKey("test-api-key"))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	return ts, &http.Client{Jar: jar}
}

// TestSonarrCallSequence drives the shim through the same requests Sonarr
// makes for its "Test" button, an add, repeated /info polling through a real
// state transition, properties/files lookup, and delete.
func TestSonarrCallSequence(t *testing.T) {
	ts, client := newTestServer(t)

	// /api/v2/torrents/info without a session must be rejected.
	resp, err := client.Get(ts.URL + "/api/v2/torrents/info")
	if err != nil {
		t.Fatalf("GET /info (unauthenticated) error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unauthenticated /info status = %d, want 403", resp.StatusCode)
	}

	// auth/login
	loginResp, err := client.PostForm(ts.URL+"/api/v2/auth/login", url.Values{
		"username": {"admin"},
		"password": {"test-api-key"},
	})
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	body := readBody(t, loginResp)
	if loginResp.StatusCode != http.StatusOK || body != "Ok." {
		t.Fatalf("login status=%d body=%q, want 200 Ok.", loginResp.StatusCode, body)
	}

	// app/webapiVersion — probed by *arr apps on "Test"
	verResp, err := client.Get(ts.URL + "/api/v2/app/webapiVersion")
	if err != nil {
		t.Fatalf("webapiVersion error = %v", err)
	}
	if v := readBody(t, verResp); v == "" {
		t.Error("webapiVersion returned empty body")
	}

	// app/preferences — Sonarr's QBittorrentProxyV2.GetConfig, called by
	// TestConnection, the actual first step of "Test." Missing this endpoint
	// entirely (a 404) was a real bug this exact test failed to catch,
	// because it wasn't in this sequence at all — found live.
	prefsResp, err := client.Get(ts.URL + "/api/v2/app/preferences")
	if err != nil {
		t.Fatalf("preferences error = %v", err)
	}
	if prefsResp.StatusCode != http.StatusOK {
		t.Fatalf("preferences status = %d, want 200", prefsResp.StatusCode)
	}
	var prefs preferencesResponse
	if err := json.NewDecoder(prefsResp.Body).Decode(&prefs); err != nil {
		t.Fatalf("decode preferences: %v", err)
	}
	prefsResp.Body.Close()
	if prefs.SavePath != "/downloads" {
		t.Errorf("preferences save_path = %q, want /downloads", prefs.SavePath)
	}

	// torrents/add
	addResp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     testMagnet,
		"category": "tv-sonarr",
	})
	if b := readBody(t, addResp); addResp.StatusCode != http.StatusOK || b != "Ok." {
		t.Fatalf("add status=%d body=%q, want 200 Ok.", addResp.StatusCode, b)
	}

	wantHash := "abcdef0123456789abcdef0123456789abcdef01"

	// First /info poll: the fake provider's second Status/List call (the
	// first happened synchronously during add) reports "downloading".
	items := getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 {
		t.Fatalf("info after add = %d items, want 1", len(items))
	}
	if items[0].Hash != wantHash {
		t.Errorf("hash = %q, want %q", items[0].Hash, wantHash)
	}
	if items[0].State != "downloading" {
		t.Errorf("state after first poll = %q, want downloading", items[0].State)
	}
	if items[0].Category != "tv-sonarr" {
		t.Errorf("category = %q, want tv-sonarr", items[0].Category)
	}

	// Second /info poll: fake provider now reports completed, but that only
	// maps to local "provider_completed" — still "downloading" to Sonarr,
	// since internal/importer hasn't fetched the files to disk yet (that's
	// exercised separately in internal/importer's own tests).
	items = getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 || items[0].State != "downloading" {
		t.Fatalf("state after second poll = %+v, want downloading (provider_completed, not yet imported)", items)
	}

	// properties
	propResp, err := client.Get(ts.URL + "/api/v2/torrents/properties?hash=" + wantHash)
	if err != nil {
		t.Fatalf("properties error = %v", err)
	}
	var props torrentProperties
	if err := json.NewDecoder(propResp.Body).Decode(&props); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	propResp.Body.Close()
	if props.Name != "Some.Release.Name" {
		t.Errorf("properties name = %q, want Some.Release.Name", props.Name)
	}

	// files
	filesResp, err := client.Get(ts.URL + "/api/v2/torrents/files?hash=" + wantHash)
	if err != nil {
		t.Fatalf("files error = %v", err)
	}
	var files []torrentFileInfo
	if err := json.NewDecoder(filesResp.Body).Decode(&files); err != nil {
		t.Fatalf("decode files: %v", err)
	}
	filesResp.Body.Close()
	if len(files) != 1 || files[0].Name != "movie.mkv" {
		t.Fatalf("files = %+v, want one movie.mkv entry", files)
	}

	// delete
	delResp, err := client.PostForm(ts.URL+"/api/v2/torrents/delete", url.Values{
		"hashes":      {wantHash},
		"deleteFiles": {"true"},
	})
	if err != nil {
		t.Fatalf("delete error = %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 200", delResp.StatusCode)
	}

	items = getTorrentInfo(t, client, ts.URL)
	if len(items) != 0 {
		t.Errorf("info after delete = %+v, want empty", items)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	ts, client := newTestServer(t)
	resp, err := client.PostForm(ts.URL+"/api/v2/auth/login", url.Values{
		"username": {"admin"},
		"password": {"wrong-key"},
	})
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || body != "Fails." {
		t.Errorf("status=%d body=%q, want 200 Fails.", resp.StatusCode, body)
	}
}

func getTorrentInfo(t *testing.T, client *http.Client, baseURL string) []torrentInfo {
	t.Helper()
	resp, err := client.Get(baseURL + "/api/v2/torrents/info")
	if err != nil {
		t.Fatalf("GET /info error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /info status = %d, want 200", resp.StatusCode)
	}
	var items []torrentInfo
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatalf("decode /info response: %v", err)
	}
	return items
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// TestRefreshFromProvider_BackfillsSizeEvenWhenStateAndProgressUnchanged is a
// regression test: a magnet-only add starts with size_bytes=0 (magnet URIs
// don't carry size), and a real bug let it stay 0 forever once state and
// progress settled — refreshFromProvider's early-exit check only looked at
// those two fields, never size, so a later poll that only changed size never
// wrote it. Found manually testing against a real TorBox account.
func TestRefreshFromProvider_BackfillsSizeEvenWhenStateAndProgressUnchanged(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	d := &database.Download{
		ID: "dl-1", Provider: "faketorbox", ProviderDownloadID: "fake-1", Kind: database.KindTorrent,
		Hash: "abc123", Name: "Some Release", State: database.StateDownloading, Progress: 0.5, SizeBytes: 0,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	provider := newFakeProvider()
	provider.entries["fake-1"] = &fakeEntry{
		name: "Some Release", size: 276445467, calls: 1, // calls=1 -> List() sees calls=2 -> "downloading"/0.5, matching d's current state/progress exactly
	}

	srv := &Server{provider: provider, db: db}
	srv.refreshFromProvider(ctx, []*database.Download{d})

	got, err := db.GetDownloadByID(ctx, "dl-1")
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.SizeBytes != 276445467 {
		t.Errorf("SizeBytes = %d, want 276445467 (backfilled even though state/progress didn't change)", got.SizeBytes)
	}
}

// TestHandleInfo_ReportsETAFromProvider proves the provider's live ETA
// actually reaches /api/v2/torrents/info's eta field. debrid.DownloadStatus
// has carried ETASeconds since TorBox's provider started populating it, but
// it was silently dropped in database.RefreshFromProvider (which has no ETA
// column to persist it to) and never made it into torrentInfo — Sonarr's
// queue view showed no ETA for any active download even though TorBox
// genuinely reports one. Fixed by reading it fresh from the same List() call
// on every /info poll instead of trying to persist a fast-changing value.
func TestHandleInfo_ReportsETAFromProvider(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	d := &database.Download{
		ID: "dl-eta", Provider: "faketorbox", ProviderDownloadID: "fake-eta", Kind: database.KindTorrent,
		Hash: "etahash", Name: "ETA Test", State: database.StateDownloading, Progress: 0.5,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	provider := newFakeProvider()
	provider.entries["fake-eta"] = &fakeEntry{
		name: "ETA Test", size: 1024, calls: 1, eta: 123,
	}

	srv := &Server{provider: provider, db: db}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/torrents/info", nil)
	srv.handleInfo(rec, req)

	var items []torrentInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("decode info response: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("info = %d items, want 1", len(items))
	}
	if items[0].Eta != 123 {
		t.Errorf("Eta = %d, want 123 (from provider)", items[0].Eta)
	}
}

// TestHandleAdd_AcceptsPlainUrlencodedMagnetOnlyPost proves a magnet-only
// add works as a plain application/x-www-form-urlencoded POST (no file
// part), not just multipart/form-data — confirmed against real
// qBittorrent's own request parser (src/base/http/requestparser.cpp) that
// it accepts both for this exact endpoint. LibriNode sends exactly this
// shape; treating ParseMultipartForm's http.ErrNotMultipart as a hard
// failure rejected every one of these with a 400 "Unsupported Media Type,"
// found live.
func TestHandleAdd_AcceptsPlainUrlencodedMagnetOnlyPost(t *testing.T) {
	ts, client := newTestServer(t)

	loginResp, err := client.PostForm(ts.URL+"/api/v2/auth/login", url.Values{
		"username": {"admin"},
		"password": {"test-api-key"},
	})
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	loginResp.Body.Close()

	addResp, err := client.PostForm(ts.URL+"/api/v2/torrents/add", url.Values{
		"urls":     {testMagnet},
		"category": {"tv-sonarr"},
	})
	if err != nil {
		t.Fatalf("add error = %v", err)
	}
	if b := readBody(t, addResp); addResp.StatusCode != http.StatusOK || b != "Ok." {
		t.Fatalf("add status=%d body=%q, want 200 Ok.", addResp.StatusCode, b)
	}

	items := getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 {
		t.Fatalf("info after urlencoded add = %d items, want 1", len(items))
	}
	if items[0].Category != "tv-sonarr" {
		t.Errorf("category = %q, want tv-sonarr", items[0].Category)
	}
}
