package torbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acervinode/acervinode/internal/debrid"
)

// Compile-time interface compliance checks.
var (
	_ debrid.TorrentProvider = (*Provider)(nil)
	_ debrid.UsenetProvider  = (*UsenetProvider)(nil)
)

func TestProvider_AddStatusFilesDeleteFlow(t *testing.T) {
	torrents := []map[string]any{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/torrents/createtorrent":
			torrents = append(torrents, map[string]any{
				"id": 42.0, "hash": "abc123", "name": "Some.Release",
				"size": 2048.0, "download_state": "downloading", "progress": 0.1,
				"files": []map[string]any{{"id": 1.0, "name": "movie.mkv", "size": 2048.0}},
			})
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"torrent_id": 42, "hash": "abc123"}})
		case "/v1/api/torrents/mylist":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": torrents})
		case "/v1/api/torrents/requestdl":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "https://cdn.torbox.app/movie.mkv"})
		case "/v1/api/torrents/controltorrent":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["operation"] != OpDelete {
				t.Errorf("operation = %v, want delete", body["operation"])
			}
			torrents = nil
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	p := NewProvider("test-key", WithBaseURL(server.URL))
	ctx := context.Background()

	id, err := p.AddMagnet(ctx, "magnet:?xt=urn:btih:abc123", debrid.AddOptions{Name: "Some.Release"})
	if err != nil {
		t.Fatalf("AddMagnet() error = %v", err)
	}
	if id != "42" {
		t.Fatalf("id = %q, want 42", id)
	}

	status, err := p.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != debrid.StateDownloading || status.Hash != "abc123" {
		t.Errorf("status = %+v", status)
	}

	files, err := p.Files(ctx, id)
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "movie.mkv" {
		t.Errorf("files = %+v", files)
	}

	link, err := p.RequestDownloadLink(ctx, id, files[0].ProviderFileID)
	if err != nil {
		t.Fatalf("RequestDownloadLink() error = %v", err)
	}
	if link != "https://cdn.torbox.app/movie.mkv" {
		t.Errorf("link = %q", link)
	}

	if err := p.Delete(ctx, id, true); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := p.Status(ctx, id); err == nil {
		t.Error("Status() after delete: expected not-found error, got nil")
	}
}

func TestUsenetProvider_AddStatusFilesDeleteFlow(t *testing.T) {
	downloads := []map[string]any{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/usenet/createusenetdownload":
			downloads = append(downloads, map[string]any{
				"id": 99.0, "name": "Some.NZB.Release", "size": 4096.0,
				"download_state": "cached", "progress": 1.0,
				"files": []map[string]any{{"id": 1.0, "name": "episode.mkv", "size": 4096.0}},
			})
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"usenetdownload_id": "99", "hash": "nzbhash"}})
		case "/v1/api/usenet/mylist":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": downloads})
		case "/v1/api/usenet/requestdl":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "https://cdn.torbox.app/episode.mkv"})
		case "/v1/api/usenet/controlusenetdownload":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["operation"] != OpDelete {
				t.Errorf("operation = %v, want delete", body["operation"])
			}
			downloads = nil
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	p := NewUsenetProvider("test-key", WithBaseURL(server.URL))
	ctx := context.Background()

	id, err := p.AddNZBURL(ctx, "https://example.com/release.nzb", debrid.AddOptions{Name: "Some.NZB.Release"})
	if err != nil {
		t.Fatalf("AddNZBURL() error = %v", err)
	}
	if id != "99" {
		t.Fatalf("id = %q, want 99", id)
	}

	status, err := p.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != debrid.StateCompleted {
		t.Errorf("status = %+v, want StateCompleted", status)
	}

	files, err := p.Files(ctx, id)
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "episode.mkv" {
		t.Errorf("files = %+v", files)
	}

	link, err := p.RequestDownloadLink(ctx, id, files[0].ProviderFileID)
	if err != nil {
		t.Fatalf("RequestDownloadLink() error = %v", err)
	}
	if link != "https://cdn.torbox.app/episode.mkv" {
		t.Errorf("link = %q", link)
	}

	if err := p.Delete(ctx, id, true); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := p.Status(ctx, id); err == nil {
		t.Error("Status() after delete: expected not-found error, got nil")
	}
}
