package torbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acervinode/acervinode/internal/debrid"
)

var _ debrid.WebDownloadProvider = (*WebDownloadProvider)(nil)

func TestWebDownloadProvider_AddStatusFilesDeleteFlow(t *testing.T) {
	downloads := []map[string]any{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/webdl/createwebdownload":
			downloads = append(downloads, map[string]any{
				"id": 123.0, "hash": "webhash", "name": "Dragon Ball Z",
				"size": 2048.0, "download_state": "downloading", "progress": 0.3,
				"files": []map[string]any{{"id": 1.0, "name": "video.mkv", "size": 2048.0}},
			})
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"webdownload_id": 123, "hash": "webhash"}})
		case "/v1/api/webdl/mylist":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": downloads})
		case "/v1/api/webdl/requestdl":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "https://cdn.torbox.app/webdl/video.mkv"})
		case "/v1/api/webdl/controlwebdownload":
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

	p := NewWebDownloadProvider("test-key", WithBaseURL(server.URL))
	ctx := context.Background()

	id, err := p.AddLink(ctx, "https://mega.nz/folder/abc123", debrid.AddOptions{Name: "Dragon Ball Z"})
	if err != nil {
		t.Fatalf("AddLink() error = %v", err)
	}
	if id != "123" {
		t.Fatalf("id = %q, want 123", id)
	}

	status, err := p.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != debrid.StateDownloading || status.Hash != "webhash" {
		t.Errorf("status = %+v", status)
	}

	files, err := p.Files(ctx, id)
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "video.mkv" {
		t.Errorf("files = %+v", files)
	}

	link, err := p.RequestDownloadLink(ctx, id, files[0].ProviderFileID)
	if err != nil {
		t.Fatalf("RequestDownloadLink() error = %v", err)
	}
	if link != "https://cdn.torbox.app/webdl/video.mkv" {
		t.Errorf("link = %q", link)
	}

	if err := p.Delete(ctx, id, true); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := p.Status(ctx, id); err == nil {
		t.Error("Status() after delete: expected not-found error, got nil")
	}
}

func TestWebDownloadProvider_List(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/webdl/mylist" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{"id": 1.0, "hash": "a", "name": "First", "size": 10.0, "download_state": "cached", "progress": 1.0},
				{"id": 2.0, "hash": "b", "name": "Second", "size": 20.0, "download_state": "downloading", "progress": 0.5},
			},
		})
	}))
	defer server.Close()

	p := NewWebDownloadProvider("test-key", WithBaseURL(server.URL))
	statuses, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}
}
