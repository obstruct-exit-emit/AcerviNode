package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
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

func (k staticAPIKey) APIKey() string                            { return string(k) }
func (k staticAPIKey) DownloadDir() string                       { return "/downloads" }
func (k staticAPIKey) DeleteLocalFiles(*database.Download) error { return nil }

// fakeSettings is settingsSource with an inspectable DeleteLocalFiles — for
// tests that need to assert whether/how it was called (see
// TestHandleDelete_DeleteFilesTrueRemovesLocalFiles).
type fakeSettings struct {
	deleteLocalFilesCalls []string // download IDs, in order
	deleteLocalFilesErr   error
}

func (f *fakeSettings) APIKey() string      { return "test-api-key" }
func (f *fakeSettings) DownloadDir() string { return "/downloads" }
func (f *fakeSettings) DeleteLocalFiles(d *database.Download) error {
	f.deleteLocalFilesCalls = append(f.deleteLocalFilesCalls, d.ID)
	return f.deleteLocalFilesErr
}

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	return newTestServerWithSettings(t, staticAPIKey("test-api-key"))
}

func newTestServerWithSettings(t *testing.T, settings settingsSource) (*httptest.Server, *http.Client) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewServer(testRegistry(newFakeProvider()), db, settings)
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

// TestHandleDelete_DeleteFilesTrueRemovesLocalFiles proves deleteFiles=true
// on POST /api/v2/torrents/delete actually removes local files, not just the
// provider-side copy — before DeleteLocalFiles existed, TorBox's own
// provider.Delete implementation ignored the deleteFiles flag entirely, so
// this endpoint never touched local disk at all, even when Sonarr/Radarr
// explicitly asked it to.
func TestHandleDelete_DeleteFilesTrueRemovesLocalFiles(t *testing.T) {
	settings := &fakeSettings{}
	ts, client := newTestServerWithSettings(t, settings)
	login(t, client, ts.URL)

	addResp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     testMagnet,
		"category": "tv-sonarr",
	})
	addResp.Body.Close()

	db := ts.Config.Handler.(*Server).db
	all, err := db.ListAllDownloads(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAllDownloads() = %v, %v, want exactly one row", all, err)
	}
	d := all[0]

	delResp, err := client.PostForm(ts.URL+"/api/v2/torrents/delete", url.Values{
		"hashes":      {d.Hash},
		"deleteFiles": {"true"},
	})
	if err != nil {
		t.Fatalf("delete error = %v", err)
	}
	delResp.Body.Close()

	if len(settings.deleteLocalFilesCalls) != 1 || settings.deleteLocalFilesCalls[0] != d.ID {
		t.Errorf("DeleteLocalFiles calls = %v, want exactly [%s]", settings.deleteLocalFilesCalls, d.ID)
	}
}

// TestHandleDelete_RecordsDeletedTombstone proves a delete through the
// qBittorrent shim records the same tombstone the native API's own delete
// endpoint already did — without it, any delete through this shim (a user,
// or an *arr app's own routine "remove from download client" call) landing
// in the window before the provider's own listing catches up with its
// delete could leave a still-provider-side-present download with no local
// row protecting it from re-adoption, and internal/importer's next
// discovery tick would rediscover it as a brand-new Manual download — the
// exact "Managed download turned into Manual" symptom this closes.
func TestHandleDelete_RecordsDeletedTombstone(t *testing.T) {
	ts, client := newTestServer(t)
	login(t, client, ts.URL)

	addResp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     testMagnet,
		"category": "tv-sonarr",
	})
	addResp.Body.Close()

	db := ts.Config.Handler.(*Server).db
	all, err := db.ListAllDownloads(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAllDownloads() = %v, %v, want exactly one row", all, err)
	}
	d := all[0]

	delResp, err := client.PostForm(ts.URL+"/api/v2/torrents/delete", url.Values{"hashes": {d.Hash}})
	if err != nil {
		t.Fatalf("delete error = %v", err)
	}
	delResp.Body.Close()

	tombstoned, err := db.RecentlyDeletedDownloads(t.Context(), d.Provider, d.Kind)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if !tombstoned[d.ProviderDownloadID] {
		t.Errorf("RecentlyDeletedDownloads() = %v, want it to contain %s", tombstoned, d.ProviderDownloadID)
	}
}

// TestToTorrentInfo_SplitsContentPathFromSavePath proves save_path and
// content_path are never reported as equal — Sonarr/Radarr's own GetItems
// (confirmed against their real source) refuses to import a completed
// torrent whenever the two match, treating that as a misconfiguration
// signal. AcerviNode's own d.SavePath is the real per-download content
// root (what real qBittorrent calls content_path); content_path here must
// carry that real value, with save_path synthesized as its parent purely
// so the comparison never accidentally matches.
// TestHandleSetCategory_UpdatesCategoryAndRegistersIt proves setCategory —
// what Sonarr/Radarr's MarkItemAsImported calls for a separate post-import
// category — actually changes the tracked row's category, and that the new
// name shows up in GET /api/v2/torrents/categories (real qBittorrent
// requires a category to already exist there before setCategory succeeds;
// this shim auto-registers it instead — see handleSetCategory's own doc
// comment for why).
func TestHandleSetCategory_UpdatesCategoryAndRegistersIt(t *testing.T) {
	ts, client := newTestServer(t)
	login(t, client, ts.URL)

	addResp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     testMagnet,
		"category": "tv-sonarr",
	})
	addResp.Body.Close()

	db := ts.Config.Handler.(*Server).db
	all, err := db.ListAllDownloads(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAllDownloads() = %v, %v, want exactly one row", all, err)
	}
	d := all[0]

	resp, err := client.PostForm(ts.URL+"/api/v2/torrents/setCategory", url.Values{
		"hashes": {d.Hash}, "category": {"tv-sonarr-imported"},
	})
	if err != nil {
		t.Fatalf("setCategory error = %v", err)
	}
	resp.Body.Close()

	got, err := db.GetDownloadByID(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.Category != "tv-sonarr-imported" {
		t.Errorf("Category = %q, want tv-sonarr-imported", got.Category)
	}

	catResp, err := client.Get(ts.URL + "/api/v2/torrents/categories")
	if err != nil {
		t.Fatalf("categories error = %v", err)
	}
	defer catResp.Body.Close()
	var cats map[string]categoryResponse
	if err := json.NewDecoder(catResp.Body).Decode(&cats); err != nil {
		t.Fatalf("decode categories: %v", err)
	}
	if _, ok := cats["tv-sonarr-imported"]; !ok {
		t.Errorf("categories = %v, want tv-sonarr-imported to be registered", cats)
	}
}

// TestHandleSetCategory_HashesAllAppliesToEveryTorrent proves "all" (real
// qBittorrent's own wildcard for "every tracked torrent") is honored, not
// just a literal pipe-separated hash list.
func TestHandleSetCategory_HashesAllAppliesToEveryTorrent(t *testing.T) {
	ts, client := newTestServer(t)
	login(t, client, ts.URL)

	for i := 0; i < 2; i++ {
		addResp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
			"urls": testMagnet, "category": "tv-sonarr",
		})
		addResp.Body.Close()
	}

	resp, err := client.PostForm(ts.URL+"/api/v2/torrents/setCategory", url.Values{
		"hashes": {"all"}, "category": {"movies"},
	})
	if err != nil {
		t.Fatalf("setCategory error = %v", err)
	}
	resp.Body.Close()

	db := ts.Config.Handler.(*Server).db
	all, err := db.ListAllDownloads(t.Context())
	if err != nil {
		t.Fatalf("ListAllDownloads() error = %v", err)
	}
	for _, d := range all {
		if d.Category != "movies" {
			t.Errorf("download %s Category = %q, want movies", d.ID, d.Category)
		}
	}
}

// TestHandleSetShareLimits_TopPrio_SetForceStart_AreAcceptedNoOps proves
// these three real qBittorrent endpoints — called by Sonarr/Radarr only when
// specific optional client settings are enabled (seed limits, "First"
// priority, "Force Start" initial state — confirmed against their real
// source) — return success rather than 404ing, even though AcerviNode has no
// seeding/priority-queue/paused-state concept to actually apply them to.
func TestHandleSetShareLimits_TopPrio_SetForceStart_AreAcceptedNoOps(t *testing.T) {
	ts, client := newTestServer(t)
	login(t, client, ts.URL)

	cases := []struct {
		path   string
		values url.Values
	}{
		{"/api/v2/torrents/setShareLimits", url.Values{"hashes": {"all"}, "ratioLimit": {"-2"}, "seedingTimeLimit": {"-2"}, "inactiveSeedingTimeLimit": {"-2"}}},
		{"/api/v2/torrents/topPrio", url.Values{"hashes": {"all"}}},
		{"/api/v2/torrents/setForceStart", url.Values{"hashes": {"all"}, "value": {"true"}}},
	}
	for _, c := range cases {
		resp, err := client.PostForm(ts.URL+c.path, c.values)
		if err != nil {
			t.Fatalf("%s error = %v", c.path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestToTorrentInfo_SplitsContentPathFromSavePath(t *testing.T) {
	d := &database.Download{SavePath: "/downloads/tv-sonarr/Some.Release.Name"}
	info := toTorrentInfo(d, liveTorrentInfo{}, 0, false)

	if info.ContentPath != "/downloads/tv-sonarr/Some.Release.Name" {
		t.Errorf("content_path = %q, want the real save path unchanged", info.ContentPath)
	}
	if info.SavePath != "/downloads/tv-sonarr" {
		t.Errorf("save_path = %q, want the parent directory", info.SavePath)
	}
	if info.SavePath == info.ContentPath {
		t.Error("save_path and content_path must never be equal — Sonarr/Radarr treat that as a path error and refuse to import")
	}
}

// TestToTorrentInfo_EmptySavePathStaysEmpty proves a download with no
// resolved save path yet (still downloading, nothing fetched) reports both
// fields empty rather than filepath.Dir("")'s "." — a nonsensical value that
// only matters here because Sonarr's own path check is gated on the
// download already being Completed, a state a row with no SavePath should
// never actually reach (see internal/importer.processDownload, which always
// persists SavePath before marking ready_for_import).
func TestToTorrentInfo_EmptySavePathStaysEmpty(t *testing.T) {
	d := &database.Download{SavePath: ""}
	info := toTorrentInfo(d, liveTorrentInfo{}, 0, false)

	if info.SavePath != "" || info.ContentPath != "" {
		t.Errorf("save_path = %q, content_path = %q, want both empty", info.SavePath, info.ContentPath)
	}
}

// TestToTorrentInfo_ReportsSwarmInfo proves num_seeds/num_leechs/dlspeed —
// real qBittorrent's own field names — pass through from the provider's
// live status, found live to be entirely missing before this.
func TestToTorrentInfo_ReportsSwarmInfo(t *testing.T) {
	d := &database.Download{}
	info := toTorrentInfo(d, liveTorrentInfo{Seeders: 3, Leechers: 1, DownloadSpeedBytes: 191117}, 0, false)

	if info.NumSeeds != 3 {
		t.Errorf("num_seeds = %d, want 3", info.NumSeeds)
	}
	if info.NumLeechs != 1 {
		t.Errorf("num_leechs = %d, want 1", info.NumLeechs)
	}
	if info.DlSpeed != 191117 {
		t.Errorf("dlspeed = %d, want 191117", info.DlSpeed)
	}
}

// TestQbtState_ReadyForImportReportsPausedUP proves the fix for a real
// inefficiency found live: a completed Managed torrent used to report
// "uploading," which — confirmed against Sonarr/Radarr's real source —
// never satisfies CanMoveFiles/CanBeRemoved, so every torrent import fell
// back to copy-only (doubling disk usage) and was never auto-removed even
// with "Remove completed downloads" enabled. Only "pausedUP"/"stoppedUP"
// (still mapped to DownloadItemStatus.Completed, confirmed against the
// same source — this doesn't change how "done" is reported) unlock either.
func TestQbtState_ReadyForImportReportsPausedUP(t *testing.T) {
	if got := qbtState(database.StateReadyForImport); got != "pausedUP" {
		t.Errorf("qbtState(ready_for_import) = %q, want pausedUP", got)
	}
}

// TestToTorrentInfo_ReportsZeroRatioAlwaysSatisfyingSeedLimit proves the
// other half of the same fix: Sonarr/Radarr's HasReachedSeedLimit
// (confirmed against their real source) also gates CanMoveFiles/
// CanBeRemoved, requiring ratio_limit >= 0 && ratio_limit - ratio <= 0.001.
// AcerviNode never seeds a torrent locally at all, so reporting 0/0 is the
// honest answer and satisfies this unconditionally, regardless of a user's
// own configured seed-ratio settings in Sonarr/Radarr.
func TestToTorrentInfo_ReportsZeroRatioAlwaysSatisfyingSeedLimit(t *testing.T) {
	d := &database.Download{}
	info := toTorrentInfo(d, liveTorrentInfo{}, 0, false)

	if info.Ratio != 0 {
		t.Errorf("ratio = %v, want 0", info.Ratio)
	}
	if info.RatioLimit != 0 {
		t.Errorf("ratio_limit = %v, want 0", info.RatioLimit)
	}
}

// TestToTorrentInfo_SubstitutesFetchProgressWhileProviderCompleted proves
// the fix for a real UX gap: an *arr app's "Fetching" phase (provider_
// completed reported as "downloading" — see qbtState) previously showed
// progress frozen at whatever d.Progress last was (usually 1.0, since the
// provider itself is already done) for however long internal/importer's
// own local file transfer actually took. EffectiveProgress substitutes a
// live fetch progress in for that field instead, while every other state
// keeps reporting d.Progress unchanged.
func TestToTorrentInfo_SubstitutesFetchProgressWhileProviderCompleted(t *testing.T) {
	d := &database.Download{State: database.StateProviderCompleted, Progress: 1.0}
	info := toTorrentInfo(d, liveTorrentInfo{}, 0.42, true)
	if info.Progress != 0.42 {
		t.Errorf("Progress = %v, want 0.42 (live fetch progress substituted in)", info.Progress)
	}

	// No fetch progress currently tracked — falls back to d.Progress unchanged.
	info = toTorrentInfo(d, liveTorrentInfo{}, 0, false)
	if info.Progress != 1.0 {
		t.Errorf("Progress = %v, want 1.0 (d.Progress, no fetch progress tracked yet)", info.Progress)
	}

	// A different state never substitutes, even with a fetch progress value in hand.
	downloading := &database.Download{State: database.StateDownloading, Progress: 0.6}
	info = toTorrentInfo(downloading, liveTorrentInfo{}, 0.9, true)
	if info.Progress != 0.6 {
		t.Errorf("Progress = %v, want 0.6 (d.Progress, StateDownloading never substitutes)", info.Progress)
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

// login authenticates client against baseURL's /api/v2/auth/login the same
// way TestSonarrCallSequence does inline — every settingsSource fake in this
// package's tests uses "test-api-key" as its APIKey(), so this is the one
// password every test server accepts.
func login(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	resp, err := client.PostForm(baseURL+"/api/v2/auth/login", url.Values{
		"username": {"admin"},
		"password": {"test-api-key"},
	})
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || body != "Ok." {
		t.Fatalf("login status=%d body=%q, want 200 Ok.", resp.StatusCode, body)
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

	srv := &Server{registry: testRegistry(provider), db: db}
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

	srv := &Server{registry: testRegistry(provider), db: db}
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

// TestHandleAdd_ClaimsAnExistingManualRow is the end-to-end regression for
// an *arr-requested torrent ending up in the web UI's Manual tab. A row for
// the provider id the add returns can already exist — TorBox dedupes by
// content (handing back the torrent_id it already has for a hash), and the
// importer's discovery pass can adopt a just-added item moments before the
// shim inserts. The shim's plain insert then hit the (provider,
// provider_download_id) UNIQUE constraint, so the add failed outright and
// the surviving row stayed Manual: never auto-fetched to local disk, so
// Sonarr/Radarr's import step never found it.
func TestHandleAdd_ClaimsAnExistingManualRow(t *testing.T) {
	ctx := context.Background()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Already tracked as Manual under the very id the fake provider hands
	// back for the first add.
	existing := &database.Download{
		ID:                 "existing-manual-row",
		Provider:           "faketorbox",
		ProviderDownloadID: "fake-1",
		Kind:               database.KindTorrent,
		Hash:               "abcdef0123456789abcdef0123456789abcdef01",
		Name:               "Some.Release.Name",
		State:              "queued",
		AddedVia:           database.AddedViaManual,
	}
	if err := db.InsertDownload(ctx, existing); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	ts := httptest.NewServer(NewServer(testRegistry(newFakeProvider()), db, staticAPIKey("test-api-key")))
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	client := &http.Client{Jar: jar}

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
		t.Fatalf("add status=%d body=%q, want 200 Ok. — the add must not fail on an already-tracked row",
			addResp.StatusCode, b)
	}

	stored, err := db.GetDownloadByID(ctx, existing.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if stored.AddedVia != database.AddedViaArr {
		t.Errorf("added_via = %q, want %q — an *arr add must claim the row into Managed",
			stored.AddedVia, database.AddedViaArr)
	}
	if stored.Category != "tv-sonarr" {
		t.Errorf("category = %q, want the category the *arr app asked for", stored.Category)
	}

	rows, err := db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1 — claiming must promote in place, not duplicate", len(rows))
	}
}

// TestTorrentFor_ResolvesTheDownloadsOwnProvider covers the shim resolving
// per download rather than holding one provider. A row can name a provider
// this shim can't reach — several configured, or the account swapped after
// the row was created — and its provider_download_id means nothing there.
func TestTorrentFor_ResolvesTheDownloadsOwnProvider(t *testing.T) {
	srv := &Server{registry: testRegistry(newFakeProvider())}

	owned := &database.Download{ID: "a", Provider: fakeProviderName}
	if srv.torrentFor(owned) == nil {
		t.Error("torrentFor() = nil for this provider's own download")
	}

	// An older row predating the column, or a fake that never set it, falls
	// back to the default rather than being locked out.
	unattributed := &database.Download{ID: "b"}
	if srv.torrentFor(unattributed) == nil {
		t.Error("torrentFor() = nil for a row with no provider recorded")
	}

	foreign := &database.Download{ID: "c", Provider: "some-other-provider"}
	if srv.torrentFor(foreign) != nil {
		t.Error("torrentFor() returned a provider for a download belonging to a different one")
	}
}

// fakeProviderName is the name fakes register under, matching what
// fakeProvider.Name reports so a download attributed to it resolves.
const fakeProviderName = "faketorbox"

// testRegistry wraps a fake in the registry the shim now resolves through.
func testRegistry(p *fakeProvider) *debrid.Registry {
	return testRegistryNamed(fakeProviderName, p)
}

func testRegistryNamed(name string, p *fakeProvider) *debrid.Registry {
	r := debrid.NewRegistry()
	var d *debrid.DynamicTorrentProvider
	if p != nil {
		d = debrid.NewDynamicTorrentProvider(name)
		d.Set(p)
		// Reuse disabled: these tests drive a fake whose state advances on
		// each call and poll it twice back to back to observe a transition.
		// Real *arr polling is orders of magnitude slower than the cache's
		// TTL, so this restores what they were written against without
		// weakening what they assert — see debrid.ListCache.TTL.
		d.SetListCacheTTL(-1)
	}
	r.Register(name, d, nil, nil)
	return r
}

// TestRefreshFromProvider_GroupsRowsByProvider covers the shim listing each
// account once, about its own rows only. Two providers can legitimately
// issue the same id, so the live-status map is keyed by provider as well —
// merging on id alone would report one account's numbers for another's
// download.
func TestRefreshFromProvider_GroupsRowsByProvider(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	// Same provider-side id on both accounts, deliberately.
	const sharedID = "shared-1"
	alpha := newFakeProvider()
	alpha.entries[sharedID] = &fakeEntry{name: "Alpha Release", size: 100, eta: 11, calls: 1}
	beta := newFakeProvider()
	beta.entries[sharedID] = &fakeEntry{name: "Beta Release", size: 200, eta: 22, calls: 1}

	registry := debrid.NewRegistry()
	for name, f := range map[string]*fakeProvider{"alpha": alpha, "beta": beta} {
		d := debrid.NewDynamicTorrentProvider(name)
		d.Set(f)
		d.SetListCacheTTL(-1)
		registry.Register(name, d, nil, nil)
	}
	srv := &Server{registry: registry, db: db}

	rows := []*database.Download{}
	for _, name := range []string{"alpha", "beta"} {
		d := &database.Download{
			ID: "dl-" + name, Provider: name, ProviderDownloadID: sharedID,
			Kind: database.KindTorrent, Hash: "h-" + name, Name: name,
			State: database.StateDownloading,
		}
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload(%s) error = %v", name, err)
		}
		rows = append(rows, d)
	}

	live := srv.refreshFromProvider(ctx, rows)

	got := live[liveKey{provider: "alpha", id: sharedID}]
	if got.ETASeconds != 11 {
		t.Errorf("alpha ETA = %d, want 11 — its own account's number", got.ETASeconds)
	}
	if got := live[liveKey{provider: "beta", id: sharedID}]; got.ETASeconds != 22 {
		t.Errorf("beta ETA = %d, want 22 — its own account's number", got.ETASeconds)
	}
}

// TestRefreshFromProvider_NeverFlagsDownloadsAsVanished is the shim half of
// the fix. This refresh runs on every *arr poll, with no rate-limit backoff
// and no view of whether the provider has been answering reliably, so a
// listing that comes back short here must not be allowed to conclude that a
// download is gone. That decision belongs to internal/importer's bulk pass.
func TestRefreshFromProvider_NeverFlagsDownloadsAsVanished(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	d := &database.Download{
		ID: "dl-1", Provider: fakeProviderName, ProviderDownloadID: "gone-1",
		Kind: database.KindTorrent, Hash: "h1", Name: "Still There",
		State: database.StateProviderCompleted, AddedVia: database.AddedViaManual,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	// The provider knows nothing about it — an empty listing, exactly what a
	// degraded provider returns.
	srv := &Server{registry: testRegistry(newFakeProvider()), db: db}
	for i := 0; i < 5; i++ {
		srv.refreshFromProvider(ctx, []*database.Download{d})
	}

	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.MissingCount != 0 {
		t.Errorf("missing_count = %d after 5 shim refreshes, want 0", got.MissingCount)
	}
	if got.State == database.StateError {
		t.Errorf("download was flagged %q by a shim refresh, want it untouched", got.State)
	}
}
