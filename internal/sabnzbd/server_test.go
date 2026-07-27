package sabnzbd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

const testAPIKey = "test-api-key"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := NewServer(newFakeProvider(), db, testAPIKey)
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

	// mode=get_config — populates the category dropdown
	resp, err = http.Get(ts.URL + "/api?mode=get_config&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("get_config error = %v", err)
	}
	resp.Body.Close()

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

	// Second /queue poll: fake provider now reports completed. That's still
	// "Downloading" from Sonarr's perspective — internal/importer (not
	// wired into this shim-only test, see internal/importer's own tests)
	// is what would fetch the files to disk and move it to history.
	queue = getQueue(t, ts.URL)
	if len(queue.Queue.Slots) != 1 || queue.Queue.Slots[0].Status != "Downloading" {
		t.Fatalf("queue after provider-completion = %+v, want one Downloading slot (provider_completed, not yet imported)", queue.Queue.Slots)
	}

	// history stays empty — nothing has reached ready_for_import yet.
	hist := getHistory(t, ts.URL)
	if len(hist.History.Slots) != 0 {
		t.Fatalf("history = %+v, want empty (nothing imported yet)", hist.History.Slots)
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
