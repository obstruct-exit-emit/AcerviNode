package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient("test-api-key", WithBaseURL(server.URL))
}

func TestCreateTorrent_Magnet(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.URL.Path; got != "/v1/api/torrents/createtorrent" {
			t.Errorf("path = %s, want /v1/api/torrents/createtorrent", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-api-key" {
			t.Errorf("Authorization header = %q", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm() error = %v", err)
		}
		if got := r.FormValue("magnet"); got != "magnet:?xt=urn:btih:abc123" {
			t.Errorf("magnet field = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"detail":  "created",
			"data": map[string]any{
				"torrent_id": 42,
				"hash":       "abc123",
			},
		})
	})

	id, hash, err := client.CreateTorrent(context.Background(), CreateTorrentRequest{
		Magnet: "magnet:?xt=urn:btih:abc123",
	})
	if err != nil {
		t.Fatalf("CreateTorrent() error = %v", err)
	}
	if id != "42" {
		t.Errorf("id = %q, want 42", id)
	}
	if hash != "abc123" {
		t.Errorf("hash = %q, want abc123", hash)
	}
}

func TestCreateTorrent_File(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm() error = %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile() error = %v", err)
		}
		defer file.Close()
		if header.Filename != "release.torrent" {
			t.Errorf("filename = %q, want release.torrent", header.Filename)
		}

		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"torrent_id": 7, "hash": "def456"},
		})
	})

	id, hash, err := client.CreateTorrent(context.Background(), CreateTorrentRequest{
		File:     []byte("fake torrent bytes"),
		Filename: "release.torrent",
	})
	if err != nil {
		t.Fatalf("CreateTorrent() error = %v", err)
	}
	if id != "7" || hash != "def456" {
		t.Errorf("id/hash = %s/%s, want 7/def456", id, hash)
	}
}

func TestControlTorrent(t *testing.T) {
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/controltorrent" {
			t.Errorf("path = %s", got)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	})

	if err := client.ControlTorrent(context.Background(), "42", OpDelete); err != nil {
		t.Fatalf("ControlTorrent() error = %v", err)
	}
	if gotBody["torrent_id"] != "42" || gotBody["operation"] != "delete" {
		t.Errorf("request body = %+v, want torrent_id=42 operation=delete", gotBody)
	}
}

func TestRequestTorrentDownloadLink(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/requestdl" {
			t.Errorf("path = %s", got)
		}
		q := r.URL.Query()
		if q.Get("torrent_id") != "42" || q.Get("file_id") != "1" || q.Get("token") != "test-api-key" {
			t.Errorf("query = %v", q)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    "https://cdn.torbox.app/download/abc",
		})
	})

	url, err := client.RequestTorrentDownloadLink(context.Background(), "42", "1")
	if err != nil {
		t.Fatalf("RequestTorrentDownloadLink() error = %v", err)
	}
	if url != "https://cdn.torbox.app/download/abc" {
		t.Errorf("url = %q", url)
	}
}

// TestRequestTorrentZipDownloadLink pins the query shape confirmed live
// against a real TorBox account: omitting file_id and adding
// zip_link=true (undocumented, found by testing directly) returns a
// working .zip URL for the whole torrent.
func TestRequestTorrentZipDownloadLink(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/requestdl" {
			t.Errorf("path = %s", got)
		}
		q := r.URL.Query()
		if q.Get("torrent_id") != "42" || q.Get("zip_link") != "true" || q.Get("file_id") != "" {
			t.Errorf("query = %v, want torrent_id=42 zip_link=true no file_id", q)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    "https://cdn.torbox.app/zip/abc",
		})
	})

	url, err := client.RequestTorrentZipDownloadLink(context.Background(), "42")
	if err != nil {
		t.Fatalf("RequestTorrentZipDownloadLink() error = %v", err)
	}
	if url != "https://cdn.torbox.app/zip/abc" {
		t.Errorf("url = %q", url)
	}
}

func TestListTorrents(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/mylist" {
			t.Errorf("path = %s", got)
		}
		// Regression check: TorBox's mylist is server-side cached for up to
		// 600s unless bypass_cache is set — confirmed live against a real
		// account (a just-added torrent was simply absent otherwise).
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		// Regression check: rdt-client's own TorBox client always sends this
		// alongside bypass_cache — confirmed live it makes a real difference
		// (2-4x faster per call in repeated back-to-back testing against a
		// real account), not just a payload-size effect.
		if got := r.URL.Query().Get("limit"); got != listLimit {
			t.Errorf("limit query param = %q, want %s", got, listLimit)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{
					"id": 42, "hash": "abc123", "name": "Some.Release",
					"size": 1024.0, "download_state": "downloading", "progress": 0.5,
					"files": []map[string]any{{"id": 1, "name": "movie.mkv", "size": 1024.0}},
				},
			},
		})
	})

	torrents, err := client.ListTorrents(context.Background())
	if err != nil {
		t.Fatalf("ListTorrents() error = %v", err)
	}
	if len(torrents) != 1 || torrents[0].Hash != "abc123" || torrents[0].Progress != 0.5 {
		t.Errorf("torrents = %+v", torrents)
	}
	if len(torrents[0].Files) != 1 || torrents[0].Files[0].Name != "movie.mkv" {
		t.Errorf("files = %+v", torrents[0].Files)
	}
}

// TestGetTorrent pins the id-filtered request shape (id + bypass_cache, no
// limit — that's only meaningful for a multi-item response) and proves the
// single-object response envelope decodes correctly, confirmed against
// TorBox's official SDK docs (see Client.GetTorrent's doc comment).
func TestGetTorrent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/mylist" {
			t.Errorf("path = %s", got)
		}
		if got := r.URL.Query().Get("id"); got != "42" {
			t.Errorf("id query param = %q, want 42", got)
		}
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"id": 42, "hash": "abc123", "name": "Some.Release",
				"size": 1024.0, "download_state": "downloading", "progress": 0.5,
				"seeds": 3.0, "peers": 1.0, "download_speed": 191117.0,
				"files": []map[string]any{{"id": 1, "name": "movie.mkv", "size": 1024.0}},
			},
		})
	})

	torrent, err := client.GetTorrent(context.Background(), "42")
	if err != nil {
		t.Fatalf("GetTorrent() error = %v", err)
	}
	if torrent.Hash != "abc123" || torrent.Progress != 0.5 {
		t.Errorf("torrent = %+v", torrent)
	}
	if torrent.Seeds != 3 || torrent.Peers != 1 || torrent.DownloadSpeed != 191117 {
		t.Errorf("swarm info = %+v, want seeds=3 peers=1 download_speed=191117", torrent)
	}
}

func TestCheckCachedTorrents(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/checkcached" {
			t.Errorf("path = %s", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"abc123": map[string]any{"hash": "abc123"},
			},
		})
	})

	got, err := client.CheckCachedTorrents(context.Background(), []string{"abc123", "notcached"})
	if err != nil {
		t.Fatalf("CheckCachedTorrents() error = %v", err)
	}
	if !got["abc123"] {
		t.Error("abc123 should be reported cached")
	}
	if got["notcached"] {
		t.Error("notcached should be reported not cached")
	}
}

// TestCheckCachedTorrents_SendsRepeatedHashParams guards against a real,
// live-confirmed API quirk: TorBox's docs describe the hash query param as
// "comma separated," but a comma-joined value consistently timed out against
// the real API (curl exit 28, twice in a row) — repeated hash= params is
// what actually works, confirmed to correctly return every cached hash
// requested, even for two at once. See checkCached's own doc comment.
func TestCheckCachedTorrents_SendsRepeatedHashParams(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query()["hash"]
		want := []string{"aaa", "bbb"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("hash query params = %v, want %v (repeated, not comma-joined)", got, want)
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
	})
	if _, err := client.CheckCachedTorrents(context.Background(), []string{"aaa", "bbb"}); err != nil {
		t.Fatalf("CheckCachedTorrents() error = %v", err)
	}
}

func TestCheckCachedUsenet(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/usenet/checkcached" {
			t.Errorf("path = %s", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"deadbeef": map[string]any{"hash": "deadbeef"},
			},
		})
	})

	got, err := client.CheckCachedUsenet(context.Background(), []string{"deadbeef", "notcached"})
	if err != nil {
		t.Fatalf("CheckCachedUsenet() error = %v", err)
	}
	if !got["deadbeef"] {
		t.Error("deadbeef should be reported cached")
	}
	if got["notcached"] {
		t.Error("notcached should be reported not cached")
	}
}

func TestCheckCachedWebDownloads(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/checkcached" {
			t.Errorf("path = %s", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"cafef00d": map[string]any{"hash": "cafef00d"},
			},
		})
	})

	got, err := client.CheckCachedWebDownloads(context.Background(), []string{"cafef00d", "notcached"})
	if err != nil {
		t.Fatalf("CheckCachedWebDownloads() error = %v", err)
	}
	if !got["cafef00d"] {
		t.Error("cafef00d should be reported cached")
	}
	if got["notcached"] {
		t.Error("notcached should be reported not cached")
	}
}

func TestTorrentInfo(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/torrentinfo" {
			t.Errorf("path = %s", got)
		}
		if got := r.URL.Query().Get("hash"); got != "abc123" {
			t.Errorf("hash query param = %q, want abc123", got)
		}
		if got := r.URL.Query().Get("timeout"); got != "15" {
			t.Errorf("timeout query param = %q, want 15", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"name": "Test.Torrent", "hash": "abc123", "size": 12345,
				"seeds": 10, "peers": 3,
				"files": []map[string]any{
					{"name": "Test.Torrent/file.mkv", "size": 12000},
				},
			},
		})
	})

	got, err := client.TorrentInfo(context.Background(), "abc123", 15)
	if err != nil {
		t.Fatalf("TorrentInfo() error = %v", err)
	}
	if got.Name != "Test.Torrent" || got.Size != 12345 || got.Seeds != 10 || got.Peers != 3 {
		t.Errorf("TorrentInfo() = %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "Test.Torrent/file.mkv" || got.Files[0].Size != 12000 {
		t.Errorf("TorrentInfo() files = %+v", got.Files)
	}
}

// TestTorrentInfo_NotFoundSurfacesError guards the real, live-confirmed
// failure shape: a torrent TorBox can't find enough peers for within its own
// search window comes back a plain HTTP 500 with a real error detail, not a
// 200 with empty data — confirmed live against the real API with a
// fabricated hash.
func TestTorrentInfo_NotFoundSurfacesError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "UNKNOWN_ERROR",
			"detail":  "Could not download full metadata for the torrent.",
			"data":    map[string]any{"name": nil, "hash": "0000", "size": nil, "seeds": 0, "peers": 0, "files": nil},
		})
	})

	_, err := client.TorrentInfo(context.Background(), "0000", 0)
	if err == nil {
		t.Fatal("TorrentInfo() error = nil, want an error for a torrent TorBox couldn't find")
	}
}

func TestListQueued(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/queued/getqueued" {
			t.Errorf("path = %s", got)
		}
		if got := r.URL.Query().Get("type"); got != "torrent" {
			t.Errorf("type query param = %q, want torrent", got)
		}
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		// Regression check: rdt-client's own TorBox client always sends this
		// alongside bypass_cache — confirmed live it makes a real difference
		// (2-4x faster per call in repeated back-to-back testing against a
		// real account), not just a payload-size effect.
		if got := r.URL.Query().Get("limit"); got != listLimit {
			t.Errorf("limit query param = %q, want %s", got, listLimit)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{"id": 77, "hash": "queuedhash", "name": "Backlogged.Release"},
			},
		})
	})

	queued, err := client.ListQueued(context.Background(), "torrent")
	if err != nil {
		t.Fatalf("ListQueued() error = %v", err)
	}
	if len(queued) != 1 || queued[0].Hash != "queuedhash" || queued[0].Name != "Backlogged.Release" {
		t.Errorf("queued = %+v", queued)
	}
}

func TestCreateUsenetDownload_Link(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/usenet/createusenetdownload" {
			t.Errorf("path = %s", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm() error = %v", err)
		}
		if got := r.FormValue("link"); got != "https://example.com/release.nzb" {
			t.Errorf("link field = %q", got)
		}
		// usenetdownload_id is a JSON number in the real API — confirmed live
		// against a real account; a string here would be unrepresentative of
		// what TorBox actually returns (see CreateUsenetDownload).
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"usenetdownload_id": 99, "hash": "nzbhash"},
		})
	})

	id, hash, err := client.CreateUsenetDownload(context.Background(), CreateUsenetDownloadRequest{
		Link: "https://example.com/release.nzb",
	})
	if err != nil {
		t.Fatalf("CreateUsenetDownload() error = %v", err)
	}
	if id != "99" {
		t.Errorf("id = %q, want 99 (formatted the same way torrent ids are)", id)
	}
	if hash != "nzbhash" {
		t.Errorf("hash = %q", hash)
	}
}

// TestRequestUsenetZipDownloadLink pins the query shape RequestUsenetZipDownloadLink
// sends — mirrors the torrent side's confirmed-live shape, but this specific
// usenet call is NOT itself confirmed live (see the method's doc comment).
func TestRequestUsenetZipDownloadLink(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/usenet/requestdl" {
			t.Errorf("path = %s", got)
		}
		q := r.URL.Query()
		if q.Get("usenet_id") != "99" || q.Get("zip_link") != "true" || q.Get("file_id") != "" {
			t.Errorf("query = %v, want usenet_id=99 zip_link=true no file_id", q)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    "https://cdn.torbox.app/zip/nzb",
		})
	})

	url, err := client.RequestUsenetZipDownloadLink(context.Background(), "99")
	if err != nil {
		t.Fatalf("RequestUsenetZipDownloadLink() error = %v", err)
	}
	if url != "https://cdn.torbox.app/zip/nzb" {
		t.Errorf("url = %q", url)
	}
}

func TestListUsenetDownloads(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		// Regression check: rdt-client's own TorBox client always sends this
		// alongside bypass_cache — confirmed live it makes a real difference
		// (2-4x faster per call in repeated back-to-back testing against a
		// real account), not just a payload-size effect.
		if got := r.URL.Query().Get("limit"); got != listLimit {
			t.Errorf("limit query param = %q, want %s", got, listLimit)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{"id": 1, "name": "Some.NZB.Release", "download_state": "downloading", "progress": 0.25},
			},
		})
	})

	downloads, err := client.ListUsenetDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListUsenetDownloads() error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].Name != "Some.NZB.Release" {
		t.Errorf("downloads = %+v", downloads)
	}
}

// TestGetUsenetDownload is GetTorrent's usenet counterpart — same id-filtered
// request shape, single-object response envelope.
func TestGetUsenetDownload(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/usenet/mylist" {
			t.Errorf("path = %s", got)
		}
		if got := r.URL.Query().Get("id"); got != "99" {
			t.Errorf("id query param = %q, want 99", got)
		}
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"id": 99, "name": "Some.NZB.Release", "download_state": "downloading", "progress": 0.25},
		})
	})

	download, err := client.GetUsenetDownload(context.Background(), "99")
	if err != nil {
		t.Fatalf("GetUsenetDownload() error = %v", err)
	}
	if download.Name != "Some.NZB.Release" {
		t.Errorf("download = %+v", download)
	}
}

// TestCreateWebDownload pins createwebdownload's request shape — confirmed
// live against the real API: application/x-www-form-urlencoded, not
// multipart (this endpoint is link-only, no file upload option), link is the
// only required field. webdownload_id comes back as a JSON number — confirmed
// live against a real account (a raw API call against a real web download
// returned {"webdownload_id": 1462379, ...}), the same mismatch against the
// SDK's own docs (which claim it's a string) that usenetdownload_id turned
// out to have.
func TestCreateWebDownload(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/createwebdownload" {
			t.Errorf("path = %s", got)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.FormValue("link"); got != "https://mega.nz/folder/abc123" {
			t.Errorf("link field = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"webdownload_id": 123, "hash": "webhash"},
		})
	})

	id, hash, err := client.CreateWebDownload(context.Background(), CreateWebDownloadRequest{
		Link: "https://mega.nz/folder/abc123",
	})
	if err != nil {
		t.Fatalf("CreateWebDownload() error = %v", err)
	}
	if id != "123" {
		t.Errorf("id = %q, want 123", id)
	}
	if hash != "webhash" {
		t.Errorf("hash = %q, want webhash", hash)
	}
}

func TestControlWebDownload(t *testing.T) {
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/controlwebdownload" {
			t.Errorf("path = %s", got)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	})

	if err := client.ControlWebDownload(context.Background(), "123", OpDelete); err != nil {
		t.Fatalf("ControlWebDownload() error = %v", err)
	}
	if gotBody["webdl_id"] != "123" || gotBody["operation"] != "delete" {
		t.Errorf("request body = %+v, want webdl_id=123 operation=delete", gotBody)
	}
}

func TestRequestWebDownloadLink(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/requestdl" {
			t.Errorf("path = %s", got)
		}
		q := r.URL.Query()
		if q.Get("web_id") != "123" || q.Get("file_id") != "1" || q.Get("token") != "test-api-key" {
			t.Errorf("query = %v", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "https://cdn.torbox.app/webdl/file"})
	})

	url, err := client.RequestWebDownloadLink(context.Background(), "123", "1")
	if err != nil {
		t.Fatalf("RequestWebDownloadLink() error = %v", err)
	}
	if url != "https://cdn.torbox.app/webdl/file" {
		t.Errorf("url = %q", url)
	}
}

// TestRequestWebDownloadZipDownloadLink pins the query shape mirroring the
// torrent/usenet zip_link trick — the parameter's existence is confirmed via
// TorBox's real OpenAPI spec, but this exact call is NOT confirmed live (see
// RequestWebDownloadZipDownloadLink's doc comment).
func TestRequestWebDownloadZipDownloadLink(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("web_id") != "123" || q.Get("zip_link") != "true" || q.Get("file_id") != "" {
			t.Errorf("query = %v, want web_id=123 zip_link=true no file_id", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "https://cdn.torbox.app/webdl/zip"})
	})

	url, err := client.RequestWebDownloadZipDownloadLink(context.Background(), "123")
	if err != nil {
		t.Fatalf("RequestWebDownloadZipDownloadLink() error = %v", err)
	}
	if url != "https://cdn.torbox.app/webdl/zip" {
		t.Errorf("url = %q", url)
	}
}

func TestListWebDownloads(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/mylist" {
			t.Errorf("path = %s", got)
		}
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		// Regression check: rdt-client's own TorBox client always sends this
		// alongside bypass_cache — confirmed live it makes a real difference
		// (2-4x faster per call in repeated back-to-back testing against a
		// real account), not just a payload-size effect.
		if got := r.URL.Query().Get("limit"); got != listLimit {
			t.Errorf("limit query param = %q, want %s", got, listLimit)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{
					"id": 123, "hash": "webhash", "name": "Dragon Ball Z",
					"size": 2048.0, "download_state": "cached", "progress": 1.0,
					// A legitimate file id of 0 — confirmed live against a real
					// Mega folder download, and formatID handles it fine.
					"files": []map[string]any{{"id": 0, "name": "video.mkv", "size": 2048.0}},
				},
			},
		})
	})

	downloads, err := client.ListWebDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListWebDownloads() error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].Name != "Dragon Ball Z" {
		t.Errorf("downloads = %+v", downloads)
	}
	if len(downloads[0].Files) != 1 || downloads[0].Files[0].ID != 0 {
		t.Errorf("files = %+v", downloads[0].Files)
	}
}

// TestGetWebDownload is GetTorrent's web-download counterpart — same
// id-filtered request shape, single-object response envelope.
func TestGetWebDownload(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/mylist" {
			t.Errorf("path = %s", got)
		}
		if got := r.URL.Query().Get("id"); got != "123" {
			t.Errorf("id query param = %q, want 123", got)
		}
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache query param = %q, want true", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"id": 123, "hash": "webhash", "name": "Dragon Ball Z", "download_state": "cached", "progress": 1.0},
		})
	})

	download, err := client.GetWebDownload(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetWebDownload() error = %v", err)
	}
	if download.Name != "Dragon Ball Z" {
		t.Errorf("download = %+v", download)
	}
}

func TestGetUserData(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/user/me" {
			t.Errorf("path = %s", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"plan": 2, "is_subscribed": true,
				"premium_expires_at":     "2027-01-01T00:00:00Z",
				"total_bytes_downloaded": 1099511627776.0,
			},
		})
	})

	data, err := client.GetUserData(context.Background())
	if err != nil {
		t.Fatalf("GetUserData() error = %v", err)
	}
	if data.Plan != 2 || !data.IsSubscribed {
		t.Errorf("data = %+v", data)
	}
	if data.TotalBytesDownloaded != 1099511627776.0 {
		t.Errorf("TotalBytesDownloaded = %v", data.TotalBytesDownloaded)
	}
}

// TestDo_RateLimitUnwrapsToDebridErrRateLimited proves a real 429 response
// is recognizable via errors.Is(err, debrid.ErrRateLimited) even through the
// fmt.Errorf("...: %w", err) wrapping every provider adapter method applies
// on top of what Client.do returns — see APIError.Unwrap, and
// internal/importer's use of this to back off polling specifically for a
// rate limit.
func TestDo_RateLimitUnwrapsToDebridErrRateLimited(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{"detail": "rate limit exceeded"})
	})

	_, _, err := client.CreateTorrent(context.Background(), CreateTorrentRequest{Magnet: "magnet:?xt=x"})
	if err == nil {
		t.Fatal("expected error for 429 response")
	}
	wrapped := fmt.Errorf("torbox: create: %w", err)
	if !errors.Is(wrapped, debrid.ErrRateLimited) {
		t.Errorf("errors.Is(err, debrid.ErrRateLimited) = false, want true for a 429 (even wrapped)")
	}
}

// TestDo_NonRateLimitStatusDoesNotUnwrapToRateLimited proves an ordinary
// failure (e.g. a 401) is NOT mistaken for a rate limit — Unwrap only ever
// resolves to debrid.ErrRateLimited for a genuine 429.
func TestDo_NonRateLimitStatusDoesNotUnwrapToRateLimited(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"detail": "invalid api key"})
	})

	_, _, err := client.CreateTorrent(context.Background(), CreateTorrentRequest{Magnet: "magnet:?xt=x"})
	if errors.Is(err, debrid.ErrRateLimited) {
		t.Error("errors.Is(err, debrid.ErrRateLimited) = true for a 401, want false")
	}
}

func TestDo_NonSuccessHTTPStatus(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"detail": "invalid api key"})
	})

	_, _, err := client.CreateTorrent(context.Background(), CreateTorrentRequest{Magnet: "magnet:?xt=x"})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Detail, "invalid api key") {
		t.Errorf("Detail = %q", apiErr.Detail)
	}
}

// TestNewClient_DefaultsToDefaultRequestTimeout proves NewClient starts with
// defaultRequestTimeout when WithRequestTimeout isn't given — matches the
// value config.Config.ProviderRequestTimeoutSeconds defaults to.
func TestNewClient_DefaultsToDefaultRequestTimeout(t *testing.T) {
	client := NewClient("test-api-key")
	if client.requestTimeout != defaultRequestTimeout {
		t.Errorf("requestTimeout = %v, want %v", client.requestTimeout, defaultRequestTimeout)
	}
}

// TestRequestTimeout_BoundsEveryCall proves WithRequestTimeout is actually
// enforced — a server that never responds at all causes the call to fail
// (rather than hang) once the configured timeout elapses. Every do* method
// (doGet/doPostJSON/doPostForm/doMultipart) funnels through the same do(),
// so this one path covers all of them.
func TestRequestTimeout_BoundsEveryCall(t *testing.T) {
	blockForever := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockForever // never responds within the test's lifetime
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(blockForever) })

	client := NewClient("test-api-key", WithBaseURL(server.URL), WithRequestTimeout(50*time.Millisecond))

	start := time.Now()
	err := client.doGet(context.Background(), "/some/path", nil, &struct{}{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("doGet() error = nil, want a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("doGet() took %v, want it to give up around the 50ms request timeout, not hang", elapsed)
	}
}

func TestDo_SuccessFalseEnvelope(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "detail": "torrent already exists"})
	})

	_, _, err := client.CreateTorrent(context.Background(), CreateTorrentRequest{Magnet: "magnet:?xt=x"})
	if err == nil {
		t.Fatal("expected error for success:false envelope")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Detail != "torrent already exists" {
		t.Errorf("Detail = %q", apiErr.Detail)
	}
}

// TestSearchBudget_StaysUnderTheRequestDeadline proves TorBox is asked to
// give up before we do. Without the headroom both sides race to the same
// limit and ours usually wins, replacing TorBox's own explanation ("Could
// not download full metadata for the torrent within the alloted timeout")
// with a bare "context deadline exceeded" — measured live, where a cold
// hash took TorBox ~33s against our 30s default, making this the ordinary
// path for any torrent TorBox hasn't seen rather than an edge case.
func TestSearchBudget_StaysUnderTheRequestDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    int
	}{
		{"default 30s leaves headroom", 30 * time.Second, 25},
		{"generous timeout scales with it", 90 * time.Second, 85},
		{"exactly the headroom defers to TorBox", 5 * time.Second, 0},
		{"tighter than the headroom defers to TorBox", 2 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient("k", WithRequestTimeout(tc.timeout))
			if got := c.searchBudget(); got != tc.want {
				t.Errorf("searchBudget() = %d, want %d", got, tc.want)
			}
			if got := c.searchBudget(); got > 0 && time.Duration(got)*time.Second >= tc.timeout {
				t.Errorf("searchBudget() = %ds, must stay under the %s request deadline", got, tc.timeout)
			}
		})
	}
}
