package sabnzbd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

const testAPIKey = "test-api-key"

// staticAPIKey satisfies settingsSource for tests that don't need a live,
// changeable key.
type staticAPIKey string

func (k staticAPIKey) APIKey() string                            { return string(k) }
func (k staticAPIKey) DeleteLocalFiles(*database.Download) error { return nil }

// fakeSettings is settingsSource with an inspectable DeleteLocalFiles — for
// tests that need to assert whether/how it was called (see
// TestHandleDelete_DeleteFilesTrueRemovesLocalFiles).
type fakeSettings struct {
	deleteLocalFilesCalls []string // download IDs, in order
	deleteLocalFilesErr   error
}

func (f *fakeSettings) APIKey() string { return testAPIKey }
func (f *fakeSettings) DeleteLocalFiles(d *database.Download) error {
	f.deleteLocalFilesCalls = append(f.deleteLocalFilesCalls, d.ID)
	return f.deleteLocalFilesErr
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithSettings(t, staticAPIKey(testAPIKey))
}

func newTestServerWithSettings(t *testing.T, settings settingsSource) *httptest.Server {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewServer(newFakeProvider(), db, settings)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

type queueResponse struct {
	Queue struct {
		Slots []queueSlot `json:"slots"`
	} `json:"queue"`
}

type historyResponse struct {
	History struct {
		Slots []historySlot `json:"slots"`
	} `json:"history"`
}

// TestSonarrCallSequence drives the shim through the same requests Sonarr
// makes for its "Test" button, an add, repeated /queue polling through a
// real state transition, and finally seeing the item land in /history.
func TestSonarrCallSequence(t *testing.T) {
	ts := newTestServer(t)

	// mode=version with a wrong apikey must be rejected.
	resp, err := http.Get(ts.URL + "/api?mode=version&apikey=wrong")
	if err != nil {
		t.Fatalf("version (bad key) error = %v", err)
	}
	var badKey map[string]any
	json.NewDecoder(resp.Body).Decode(&badKey)
	resp.Body.Close()
	if badKey["status"] != false {
		t.Errorf("version with bad apikey = %+v, want status:false", badKey)
	}

	// mode=version — probed by *arr apps on "Test"
	resp, err = http.Get(ts.URL + "/api?mode=version&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("version error = %v", err)
	}
	var ver map[string]string
	json.NewDecoder(resp.Body).Decode(&ver)
	resp.Body.Close()
	if ver["version"] == "" {
		t.Error("version response missing version field")
	}

	// mode=get_config — populates the category dropdown, and is also what
	// Sonarr/Radarr's own TestCategory() reads on every "Test": it calls
	// category.Dir.TrimEnd('*') unconditionally on every category returned,
	// with no null check (confirmed against Sonarr's real source) — a
	// missing "dir" key crashed every single Test with an unhandled
	// NullReferenceException ("Object reference not set to an instance of
	// an object"), found live. Decoded generically here (not into a Go
	// struct) specifically so a missing key is actually caught — a struct
	// decode would silently accept it as the zero value.
	resp, err = http.Get(ts.URL + "/api?mode=get_config&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("get_config error = %v", err)
	}
	var config struct {
		Config struct {
			Categories []map[string]any `json:"categories"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		t.Fatalf("decode get_config: %v", err)
	}
	resp.Body.Close()
	if len(config.Config.Categories) == 0 {
		t.Fatal("get_config categories is empty, want at least the built-in \"*\"")
	}
	for _, cat := range config.Config.Categories {
		if _, ok := cat["dir"]; !ok {
			t.Errorf("category %v missing \"dir\" key", cat)
		}
	}

	// mode=addurl
	addResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode":    {"addurl"},
		"apikey":  {testAPIKey},
		"name":    {"https://example.com/release.nzb"},
		"cat":     {"tv-sonarr"},
		"nzbname": {"Some.NZB.Release"},
	})
	if err != nil {
		t.Fatalf("addurl error = %v", err)
	}
	var addResult map[string]any
	json.NewDecoder(addResp.Body).Decode(&addResult)
	addResp.Body.Close()
	if addResult["status"] != true {
		t.Fatalf("addurl result = %+v, want status:true", addResult)
	}
	nzoIDs, _ := addResult["nzo_ids"].([]any)
	if len(nzoIDs) != 1 {
		t.Fatalf("addurl nzo_ids = %+v, want one id", addResult["nzo_ids"])
	}
	nzoID := nzoIDs[0].(string)

	// First /queue poll: fake provider's second call reports "downloading".
	queue := getQueue(t, ts.URL)
	if len(queue.Queue.Slots) != 1 {
		t.Fatalf("queue after add = %d slots, want 1", len(queue.Queue.Slots))
	}
	slot := queue.Queue.Slots[0]
	if slot.NzoID != nzoID {
		t.Errorf("nzo_id = %q, want %q", slot.NzoID, nzoID)
	}
	if slot.Status != "Downloading" {
		t.Errorf("status after first poll = %q, want Downloading", slot.Status)
	}
	if slot.Cat != "tv-sonarr" {
		t.Errorf("cat = %q, want tv-sonarr", slot.Cat)
	}

	// Second /queue poll: fake provider now reports completed. That's
	// reported as "Moving" — real SABnzbd's own "post-processing done, now
	// placing files into their final location" phase, still just
	// DownloadItemStatus.Downloading from Sonarr's perspective (confirmed
	// against Sonarr's real source), not ready to import yet — internal/
	// importer (not wired into this shim-only test, see internal/importer's
	// own tests) is what actually fetches the files to disk and moves it to
	// history.
	queue = getQueue(t, ts.URL)
	if len(queue.Queue.Slots) != 1 || queue.Queue.Slots[0].Status != "Moving" {
		t.Fatalf("queue after provider-completion = %+v, want one Moving slot (provider_completed, not yet imported)", queue.Queue.Slots)
	}

	// history stays empty — nothing has reached ready_for_import yet.
	hist := getHistory(t, ts.URL)
	if len(hist.History.Slots) != 0 {
		t.Fatalf("history = %+v, want empty (nothing imported yet)", hist.History.Slots)
	}
}

// TestHandleDelete_RemovesFromQueueAndHistory proves name=delete works
// layered onto both mode=queue (an active download) and mode=history (a
// failed one) — matching real SABnzbd's API shape, which Sonarr/Radarr rely
// on to let a user remove a download from either list.
func TestHandleDelete_RemovesFromQueueAndHistory(t *testing.T) {
	ts := newTestServer(t)

	// Add one via addurl, delete it straight out of the queue.
	addResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"addurl"}, "apikey": {testAPIKey},
		"name": {"https://example.com/queued.nzb"}, "cat": {"tv-sonarr"},
	})
	if err != nil {
		t.Fatalf("addurl error = %v", err)
	}
	var addResult map[string]any
	json.NewDecoder(addResp.Body).Decode(&addResult)
	addResp.Body.Close()
	nzoID := addResult["nzo_ids"].([]any)[0].(string)

	if queue := getQueue(t, ts.URL); len(queue.Queue.Slots) != 1 {
		t.Fatalf("queue before delete = %d slots, want 1", len(queue.Queue.Slots))
	}

	delResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"queue"}, "name": {"delete"}, "apikey": {testAPIKey}, "value": {nzoID},
	})
	if err != nil {
		t.Fatalf("queue delete error = %v", err)
	}
	var delResult map[string]any
	json.NewDecoder(delResp.Body).Decode(&delResult)
	delResp.Body.Close()
	if delResult["status"] != true {
		t.Errorf("queue delete result = %+v, want status:true", delResult)
	}
	if queue := getQueue(t, ts.URL); len(queue.Queue.Slots) != 0 {
		t.Errorf("queue after delete = %+v, want empty", queue.Queue.Slots)
	}

	// A second download, forced straight to error state (bypassing the
	// shim, which has no path there on its own in a provider-only test), to
	// prove mode=history&name=delete works the same way for a history row.
	ctx := t.Context()
	failed := &database.Download{
		ID: "dl-failed", Provider: "fake", ProviderDownloadID: "provider-failed",
		Kind: database.KindUsenet, Name: "Failed Release", Category: "tv-sonarr",
		State: database.StateError, ErrorMessage: "simulated",
	}
	db := ts.Config.Handler.(*Server).db
	if err := db.InsertDownload(ctx, failed); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	if hist := getHistory(t, ts.URL); len(hist.History.Slots) != 1 {
		t.Fatalf("history before delete = %d slots, want 1", len(hist.History.Slots))
	}

	delResp, err = http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"history"}, "name": {"delete"}, "apikey": {testAPIKey}, "value": {"dl-failed"},
	})
	if err != nil {
		t.Fatalf("history delete error = %v", err)
	}
	json.NewDecoder(delResp.Body).Decode(&delResult)
	delResp.Body.Close()
	if delResult["status"] != true {
		t.Errorf("history delete result = %+v, want status:true", delResult)
	}
	if hist := getHistory(t, ts.URL); len(hist.History.Slots) != 0 {
		t.Errorf("history after delete = %+v, want empty", hist.History.Slots)
	}
}

// TestHandleDelete_DeleteFilesTrueRemovesLocalFiles proves del_files=1
// actually removes local files, not just the provider-side copy — before
// DeleteLocalFiles existed, TorBox's own provider.Delete implementation
// ignored the deleteFiles flag entirely (it only ever deletes the
// provider-side copy), so this endpoint never touched local disk at all,
// even when Sonarr/Radarr explicitly asked it to.
func TestHandleDelete_DeleteFilesTrueRemovesLocalFiles(t *testing.T) {
	settings := &fakeSettings{}
	ts := newTestServerWithSettings(t, settings)

	addResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"addurl"}, "apikey": {testAPIKey},
		"name": {"https://example.com/queued.nzb"}, "cat": {"tv-sonarr"},
	})
	if err != nil {
		t.Fatalf("addurl error = %v", err)
	}
	var addResult map[string]any
	json.NewDecoder(addResp.Body).Decode(&addResult)
	addResp.Body.Close()
	nzoID := addResult["nzo_ids"].([]any)[0].(string)

	delResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"queue"}, "name": {"delete"}, "apikey": {testAPIKey},
		"value": {nzoID}, "del_files": {"1"},
	})
	if err != nil {
		t.Fatalf("queue delete error = %v", err)
	}
	delResp.Body.Close()

	if len(settings.deleteLocalFilesCalls) != 1 || settings.deleteLocalFilesCalls[0] != nzoID {
		t.Errorf("DeleteLocalFiles calls = %v, want exactly [%s]", settings.deleteLocalFilesCalls, nzoID)
	}
}

// TestHandleDelete_RecordsDeletedTombstone proves a delete through the
// SABnzbd shim records the same tombstone the native API's own delete
// endpoint already did — without it, any delete through this shim (a user,
// or an *arr app's own routine "remove from download client" call) landing
// in the window before the provider's own listing catches up with its
// delete could leave a still-provider-side-present download with no local
// row protecting it from re-adoption, and internal/importer's next
// discovery tick would rediscover it as a brand-new Manual download — the
// exact "Managed download turned into
// Manual" symptom this closes.
func TestHandleDelete_RecordsDeletedTombstone(t *testing.T) {
	ts := newTestServer(t)

	addResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"addurl"}, "apikey": {testAPIKey},
		"name": {"https://example.com/queued.nzb"}, "cat": {"tv-sonarr"},
	})
	if err != nil {
		t.Fatalf("addurl error = %v", err)
	}
	var addResult map[string]any
	json.NewDecoder(addResp.Body).Decode(&addResult)
	addResp.Body.Close()
	nzoID := addResult["nzo_ids"].([]any)[0].(string)

	db := ts.Config.Handler.(*Server).db
	d, err := db.GetDownloadByID(t.Context(), nzoID)
	if err != nil || d == nil {
		t.Fatalf("GetDownloadByID(%s) = %v, %v", nzoID, d, err)
	}

	delResp, err := http.PostForm(ts.URL+"/api", url.Values{
		"mode": {"queue"}, "name": {"delete"}, "apikey": {testAPIKey}, "value": {nzoID},
	})
	if err != nil {
		t.Fatalf("queue delete error = %v", err)
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

// TestHandleHistory_ReportsBytes proves the real SABnzbd history field
// "bytes" is populated — confirmed against Sonarr's real source
// (SabnzbdHistoryItem/Sabnzbd.cs's GetHistory) that it's read directly into
// the download item's own TotalSize, unlike nzb_name/download_time (also on
// the real schema, confirmed unused by Sonarr's parsing) — found missing
// entirely during an API-parity audit.
func TestHandleHistory_ReportsBytes(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	completed := &database.Download{
		ID: "dl-completed", Provider: "fake", ProviderDownloadID: "provider-completed",
		Kind: database.KindUsenet, Name: "Completed Release", Category: "tv-sonarr",
		State: database.StateReadyForImport, SizeBytes: 292301045,
	}
	failed := &database.Download{
		ID: "dl-failed-bytes", Provider: "fake", ProviderDownloadID: "provider-failed-bytes",
		Kind: database.KindUsenet, Name: "Failed Release", Category: "tv-sonarr",
		State: database.StateError, ErrorMessage: "simulated", SizeBytes: 12345,
	}
	db := ts.Config.Handler.(*Server).db
	if err := db.InsertDownload(ctx, completed); err != nil {
		t.Fatalf("InsertDownload(completed) error = %v", err)
	}
	if err := db.InsertDownload(ctx, failed); err != nil {
		t.Fatalf("InsertDownload(failed) error = %v", err)
	}

	hist := getHistory(t, ts.URL)
	byID := make(map[string]historySlot, len(hist.History.Slots))
	for _, slot := range hist.History.Slots {
		byID[slot.NzoID] = slot
	}
	if got := byID["dl-completed"].Bytes; got != 292301045 {
		t.Errorf("completed slot Bytes = %d, want 292301045", got)
	}
	if got := byID["dl-failed-bytes"].Bytes; got != 12345 {
		t.Errorf("failed slot Bytes = %d, want 12345", got)
	}
}

func getQueue(t *testing.T, baseURL string) queueResponse {
	t.Helper()
	resp, err := http.Get(baseURL + "/api?mode=queue&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("queue error = %v", err)
	}
	defer resp.Body.Close()
	var q queueResponse
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		t.Fatalf("decode queue response: %v", err)
	}
	return q
}

func getHistory(t *testing.T, baseURL string) historyResponse {
	t.Helper()
	resp, err := http.Get(baseURL + "/api?mode=history&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("history error = %v", err)
	}
	defer resp.Body.Close()
	var h historyResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	return h
}

// TestRefreshFromProvider_BackfillsSizeEvenWhenStateAndProgressUnchanged
// mirrors internal/qbittorrent's regression test for the same real bug: an
// NZB-URL-only add starts with size_bytes=0, and refreshFromProvider's
// early-exit check only compared state and progress, never size, so a later
// poll that only changed size never got persisted.
func TestRefreshFromProvider_BackfillsSizeEvenWhenStateAndProgressUnchanged(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	d := &database.Download{
		ID: "dl-1", Provider: "faketorbox", ProviderDownloadID: "fake-usenet-1", Kind: database.KindUsenet,
		Name: "Some NZB Release", State: database.StateDownloading, Progress: 0.5, SizeBytes: 0,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	provider := newFakeProvider()
	provider.entries["fake-usenet-1"] = &fakeEntry{
		name: "Some NZB Release", size: 987654321, calls: 1, // calls=1 -> List() sees calls=2 -> "downloading"/0.5, matching d exactly
	}

	srv := &Server{provider: provider, db: db}
	srv.refreshFromProvider(ctx, []*database.Download{d})

	got, err := db.GetDownloadByID(ctx, "dl-1")
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got.SizeBytes != 987654321 {
		t.Errorf("SizeBytes = %d, want 987654321 (backfilled even though state/progress didn't change)", got.SizeBytes)
	}
}

// TestHandleQueue_ReportsTimeLeftFromProvider proves the provider's live ETA
// reaches mode=queue's timeleft field — same underlying gap as
// internal/qbittorrent's eta fix: debrid.DownloadStatus.ETASeconds was always
// available but never made it into a queue slot.
func TestHandleQueue_ReportsTimeLeftFromProvider(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	d := &database.Download{
		ID: "dl-eta", Provider: "faketorbox", ProviderDownloadID: "fake-usenet-eta", Kind: database.KindUsenet,
		Name: "ETA Test", State: database.StateDownloading, Progress: 0.5,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}

	provider := newFakeProvider()
	provider.entries["fake-usenet-eta"] = &fakeEntry{
		name: "ETA Test", size: 1024, calls: 1, eta: 754, // 754s = 0:12:34
	}

	srv := &Server{provider: provider, db: db, categories: newCategoryStore()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=queue", nil)
	srv.handleQueue(rec, req)

	var q queueResponse
	if err := json.NewDecoder(rec.Body).Decode(&q); err != nil {
		t.Fatalf("decode queue response: %v", err)
	}
	if len(q.Queue.Slots) != 1 {
		t.Fatalf("queue = %d slots, want 1", len(q.Queue.Slots))
	}
	if got := q.Queue.Slots[0].TimeLeft; got != "0:12:34" {
		t.Errorf("TimeLeft = %q, want 0:12:34 (from provider's 754s ETA)", got)
	}
}

// TestHandleQueue_ReportsAggregateSpeed proves mode=queue's top-level
// kbpersec — real SABnzbd's own aggregate-speed field, not a per-slot one
// (confirmed against SABnzbd's real API docs: no per-slot speed field
// exists there to match even if AcerviNode wanted one) — sums every active
// download's current speed.
func TestHandleQueue_ReportsAggregateSpeed(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	provider := newFakeProvider()
	for i, id := range []string{"fake-usenet-speed-1", "fake-usenet-speed-2"} {
		d := &database.Download{
			ID: id, Provider: "faketorbox", ProviderDownloadID: id, Kind: database.KindUsenet,
			Name: id, State: database.StateDownloading, Progress: 0.5,
		}
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload(%s) error = %v", id, err)
		}
		speed := int64(100 * 1024) // 100 KB/s
		if i == 1 {
			speed = 924 * 1024 // 924 KB/s -> combined 1024.00 KB/s
		}
		provider.entries[debrid.ProviderDownloadID(id)] = &fakeEntry{name: id, size: 1024, calls: 1, speed: speed}
	}

	srv := &Server{provider: provider, db: db, categories: newCategoryStore()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=queue", nil)
	srv.handleQueue(rec, req)

	var q struct {
		Queue struct {
			Slots    []queueSlot `json:"slots"`
			KBPerSec string      `json:"kbpersec"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&q); err != nil {
		t.Fatalf("decode queue response: %v", err)
	}
	if len(q.Queue.Slots) != 2 {
		t.Fatalf("slots = %d, want 2", len(q.Queue.Slots))
	}
	if q.Queue.KBPerSec != "1024.00" {
		t.Errorf("kbpersec = %q, want 1024.00 (100KB/s + 924KB/s summed)", q.Queue.KBPerSec)
	}
}

func TestFormatTimeLeft(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{0, "0:00:00"},
		{-5, "0:00:00"},
		{59, "0:00:59"},
		{60, "0:01:00"},
		{754, "0:12:34"},
		{3661, "1:01:01"},
	}
	for _, c := range cases {
		if got := formatTimeLeft(c.seconds); got != c.want {
			t.Errorf("formatTimeLeft(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

// TestSabnzbdPhaseStatus proves the three real SABnzbd post-processing
// phases TorBox's usenet service also goes through (see
// torbox.mapUsenetState's own doc comment) are reported as the same status
// strings a real SABnzbd instance would use — Sonarr/Radarr's own
// SabnzbdDownloadStatus enum already recognizes all three, confirmed
// against Sonarr's real source — rather than staying stuck at a generic
// "Downloading" for the whole verify/repair/extract sequence.
func TestSabnzbdPhaseStatus(t *testing.T) {
	cases := []struct {
		phase string
		want  string
	}{
		{"verifying", "Verifying"},
		{"repairing", "Repairing"},
		{"extracting", "Extracting"},
		{"processing", "Verifying"}, // TorBox's own generic bucket — no exact real-SABnzbd equivalent, see sabnzbdPhaseStatus's own doc comment
		{"", "Downloading"},
		{"something-unrecognized", "Downloading"},
	}
	for _, c := range cases {
		if got := sabnzbdPhaseStatus(c.phase); got != c.want {
			t.Errorf("sabnzbdPhaseStatus(%q) = %q, want %q", c.phase, got, c.want)
		}
	}
}

// TestToQueueSlot_UsesPhaseSpecificStatus proves a queue slot's status
// string reflects the actual sub-phase when one's known, and still falls
// back to "Downloading" when it isn't (a torrent-only concept has none, an
// ordinary "downloading" phase is "", etc.) — and that a StateQueued row
// never gets phase-status treatment, matching real SABnzbd's own "Queued"
// meaning nothing has started yet.
func TestToQueueSlot_UsesPhaseSpecificStatus(t *testing.T) {
	cases := []struct {
		name  string
		state string
		phase string
		want  string
	}{
		{"plain downloading, no known phase", database.StateDownloading, "", "Downloading"},
		{"verifying", database.StateDownloading, "verifying", "Verifying"},
		{"repairing", database.StateDownloading, "repairing", "Repairing"},
		{"extracting", database.StateDownloading, "extracting", "Extracting"},
		{"provider_completed always reports Moving regardless of a stray phase value — the provider's own work is already done", database.StateProviderCompleted, "verifying", "Moving"},
		{"queued stays queued regardless of a stray phase value", database.StateQueued, "verifying", "Queued"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &database.Download{ID: "dl-1", Name: "Test", State: c.state}
			slot := toQueueSlot(d, 0, c.phase)
			if slot.Status != c.want {
				t.Errorf("status = %q, want %q", slot.Status, c.want)
			}
		})
	}
}
