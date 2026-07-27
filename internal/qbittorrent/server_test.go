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

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewServer(newFakeProvider(), db, "test-api-key")
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

	// Second /info poll: fake provider now reports completed.
	items = getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 || items[0].State != "uploading" {
		t.Fatalf("state after second poll = %+v, want uploading (mapped from provider_completed)", items)
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
