package sabnzbd

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestGetConfig_ReportsARootedCompleteDir.
//
// Sonarr's GetCategories reads misc.complete_dir and, when it is not rooted,
// falls back to mode=fullstatus's own CompleteDir. We sent "" for the first and
// omitted the second entirely, so both paths came back empty: category.FullPath
// ended up empty and so did status.OutputRootFolders — which is what Sonarr's
// remote-path-mapping health check reads.
//
// Reporting a rooted path also means Sonarr stops making the fullstatus call at
// all, since the first value already answers the question.
func TestGetConfig_ReportsARootedCompleteDir(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api?mode=get_config&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("mode=get_config error = %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Config struct {
			Misc struct {
				CompleteDir string `json:"complete_dir"`
			} `json:"misc"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode get_config: %v", err)
	}

	got := out.Config.Misc.CompleteDir
	if got == "" {
		t.Fatal("misc.complete_dir is empty; Sonarr cannot resolve the client's output root")
	}
	if got[0] != '/' {
		t.Errorf("misc.complete_dir = %q, want a rooted path so Sonarr does not fall back to fullstatus", got)
	}
	if got != testDownloadDir {
		t.Errorf("misc.complete_dir = %q, want the configured download dir %q", got, testDownloadDir)
	}
}

// TestFullStatus_ReportsCompleteDir covers the fallback itself, for the older
// path and for any client that asks this way round.
func TestFullStatus_ReportsCompleteDir(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api?mode=fullstatus&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("mode=fullstatus error = %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Status struct {
			CompleteDir string `json:"complete_dir"`
			Version     string `json:"version"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode fullstatus: %v", err)
	}

	if out.Status.CompleteDir != testDownloadDir {
		t.Errorf("status.complete_dir = %q, want %q", out.Status.CompleteDir, testDownloadDir)
	}
	// The fields that were already there must survive.
	if out.Status.Version == "" {
		t.Error("status.version went missing")
	}
}
