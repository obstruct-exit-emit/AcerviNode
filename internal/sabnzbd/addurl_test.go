package sabnzbd

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// TestMain keeps addurl's own fetch off the network: only loopback hosts --
// the httptest servers these tests start -- are reachable. Older tests add
// https://example.com/... links; they now take the fallback, handing the link
// to the provider exactly as every addurl did before the fetch existed, so
// they still test what they were written against, offline.
func TestMain(m *testing.M) {
	nzbHTTPClient = &http.Client{Transport: loopbackOnly{http.DefaultTransport}}
	os.Exit(m.Run())
}

type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("test: no network beyond loopback")
	}
	return l.next.RoundTrip(r)
}

const testNZB = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file poster="p" date="1" subject="s"></file></nzb>`

// newAddURLServer is a test server with its fake provider in hand, so a test
// can see which route an add took to reach it.
func newAddURLServer(t *testing.T) (*httptest.Server, *fakeProvider, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	p := newFakeProvider()
	ts := httptest.NewServer(NewServer(testRegistry(p), db, staticAPIKey(testAPIKey)))
	t.Cleanup(ts.Close)
	return ts, p, db
}

func addURL(t *testing.T, ts *httptest.Server, link, nzbname string) map[string]any {
	t.Helper()
	form := url.Values{"mode": {"addurl"}, "apikey": {testAPIKey}, "name": {link}, "cat": {"music"}}
	if nzbname != "" {
		form.Set("nzbname", nzbname)
	}
	resp, err := http.PostForm(ts.URL+"/api", form)
	if err != nil {
		t.Fatalf("addurl error = %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode addurl reply: %v", err)
	}
	return out
}

// TestAddURL_FetchesTheNZBItself.
//
// Real SABnzbd fetches an addurl link itself, from the machine it runs on. This
// shim handed the link to the provider instead, so a debrid service's cloud
// servers were asked to fetch it -- and the link a self-hosted app sends is
// usually a LAN one, typically Prowlarr's download proxy, which they can never
// reach. Found live: CantiNode fell back to addurl with a 192.168.1.x Prowlarr
// link and AcerviNode did not answer within its 30-second timeout. Sonarr and
// Radarr upload the file (addfile), which is why they never hit it.
func TestAddURL_FetchesTheNZBItself(t *testing.T) {
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write([]byte(testNZB))
	}))
	t.Cleanup(indexer.Close)
	ts, p, _ := newAddURLServer(t)

	link := indexer.URL + "/2/download?apikey=secret&link=abc"
	out := addURL(t, ts, link, "Mike Doughty-Stellar Motel-CD-FLAC-2014-FORSAKEN")
	if out["status"] != true {
		t.Fatalf("addurl = %+v, want status:true", out)
	}
	if len(p.urlAdds) != 0 {
		t.Errorf("the link was handed to the provider (%v); it should have been fetched here", p.urlAdds)
	}
	if len(p.fileAdds) != 1 || string(p.fileAdds[0].data) != testNZB {
		t.Fatalf("fileAdds = %d, want the fetched NZB uploaded once, byte for byte", len(p.fileAdds))
	}
	if p.lastAddName != "Mike Doughty-Stellar Motel-CD-FLAC-2014-FORSAKEN" {
		t.Errorf("job name = %q, want the nzbname the client sent", p.lastAddName)
	}
}

// TestAddURL_FallsBackToTheLink -- when the fetch here fails, or what comes
// back is not an NZB, the link goes to the provider as it always did. That
// keeps working whatever worked before: a public link the provider can reach
// is still a valid add even if this machine cannot fetch it.
func TestAddURL_FallsBackToTheLink(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"the indexer refuses", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}},
		{"an error status, whatever the body looks like", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(testNZB))
		}},
		{"an HTML error page with a 200", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html><body>API limit reached</body></html>"))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			indexer := httptest.NewServer(tc.handler)
			t.Cleanup(indexer.Close)
			ts, p, _ := newAddURLServer(t)

			link := indexer.URL + "/getnzb?id=1&apikey=secret"
			if out := addURL(t, ts, link, "Some.Release"); out["status"] != true {
				t.Fatalf("addurl = %+v, want status:true", out)
			}
			if len(p.fileAdds) != 0 {
				t.Errorf("uploaded %d file(s) that were not an NZB", len(p.fileAdds))
			}
			if len(p.urlAdds) != 1 || p.urlAdds[0] != link {
				t.Errorf("urlAdds = %v, want the link handed over unchanged", p.urlAdds)
			}
		})
	}
}

// TestAddURL_NamesTheJobLikeAnUpload -- with no nzbname, real SABnzbd names
// the job after the fetched file, as it does for an upload: the
// Content-Disposition filename without .nzb.
func TestAddURL_NamesTheJobLikeAnUpload(t *testing.T) {
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="Some.Show.S01E01.1080p.WEB-GRP.nzb"`)
		w.Write([]byte(testNZB))
	}))
	t.Cleanup(indexer.Close)
	ts, p, _ := newAddURLServer(t)

	if out := addURL(t, ts, indexer.URL+"/download?id=9", ""); out["status"] != true {
		t.Fatalf("addurl = %+v, want status:true", out)
	}
	if p.lastAddName != "Some.Show.S01E01.1080p.WEB-GRP" {
		t.Errorf("job name = %q, want the file's own name without .nzb", p.lastAddName)
	}
	if len(p.fileAdds) != 1 || !strings.HasSuffix(p.fileAdds[0].filename, ".nzb") {
		t.Errorf("fileAdds = %+v, want one upload named like an .nzb", p.fileAdds)
	}
}
