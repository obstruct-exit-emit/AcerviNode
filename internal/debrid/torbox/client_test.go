package torbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestListTorrents(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/api/torrents/mylist" {
			t.Errorf("path = %s", got)
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
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"usenetdownload_id": "usenet-99", "hash": "nzbhash"},
		})
	})

	id, hash, err := client.CreateUsenetDownload(context.Background(), CreateUsenetDownloadRequest{
		Link: "https://example.com/release.nzb",
	})
	if err != nil {
		t.Fatalf("CreateUsenetDownload() error = %v", err)
	}
	if id != "usenet-99" {
		t.Errorf("id = %q, want usenet-99 (string, not numeric-formatted)", id)
	}
	if hash != "nzbhash" {
		t.Errorf("hash = %q", hash)
	}
}

func TestListUsenetDownloads(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
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
