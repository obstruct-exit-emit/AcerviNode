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

func TestGetHosterList(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/webdl/hosters" {
			t.Errorf("path = %s", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{"name": "Mega", "domains": []string{"mega.nz"}, "status": true, "type": "hoster"},
				{"name": "YouTube", "domains": []string{"youtube.com"}, "status": true, "type": "stream"},
			},
		})
	})

	hosters, err := client.GetHosterList(context.Background())
	if err != nil {
		t.Fatalf("GetHosterList() error = %v", err)
	}
	if len(hosters) != 2 || hosters[0].Name != "Mega" || !hosters[0].Status {
		t.Errorf("hosters = %+v", hosters)
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
