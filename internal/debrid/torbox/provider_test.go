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

// TestMapDownloadState proves TorBox's real download_state vocabulary maps
// correctly, especially that anything unmatched — including a stalled/
// no-seeds torrent, and TorBox's own documented "Error" state — is treated
// as an error rather than silently folded into "still downloading" forever.
// Ported from decypharr's own production mapping (github.com/sirrobot01/
// decypharr, pkg/debrid/providers/torbox/torbox.go's getTorboxStatus) as the
// reference for TorBox's actual vocabulary, since TorBox's docs don't
// publish an exhaustive list; "Error" itself is independently confirmed by
// TorBox's help center ("Download Statuses").
func TestMapDownloadState(t *testing.T) {
	cases := []struct {
		raw  string
		want debrid.DownloadState
	}{
		{"", debrid.StateUnknown},
		{"downloading", debrid.StateDownloading},
		{"metaDL", debrid.StateDownloading},
		{"checkingResumeData", debrid.StateDownloading},
		{"paused", debrid.StateDownloading},
		{"queuedDL", debrid.StateDownloading},
		{"incomplete", debrid.StateDownloading}, // TorBox v8.4.3's stalled-seeders state
		{"stalled (no seeds)", debrid.StateError},
		{"completed", debrid.StateCompleted},
		{"cached", debrid.StateCompleted},
		{"uploading", debrid.StateCompleted},
		{"downloaded", debrid.StateCompleted},
		{"Error", debrid.StateError},
		{"error", debrid.StateError},
		{"some-unrecognized-future-state", debrid.StateError},
	}
	for _, c := range cases {
		if got := mapDownloadState(c.raw); got != c.want {
			t.Errorf("mapDownloadState(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

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
		case "/v1/api/queued/getqueued":
			// Status()/List() check this too now — nothing queued in this
			// test, so an empty list, matching a real account with nothing
			// backlogged.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{}})
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

// TestProvider_ListMergesQueuedDownloads proves a torrent that's still in
// TorBox's pre-processing queue (per queued/getqueued) — and so absent from
// mylist entirely — shows up as queued rather than being invisible, and that
// one already present in mylist isn't duplicated.
func TestProvider_ListMergesQueuedDownloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/torrents/mylist":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{"id": 1.0, "hash": "already-listed", "name": "In Mylist", "size": 10.0, "download_state": "downloading", "progress": 0.2},
				},
			})
		case "/v1/api/queued/getqueued":
			if got := r.URL.Query().Get("type"); got != "torrent" {
				t.Errorf("type query param = %q, want torrent", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{"id": 1.0, "hash": "already-listed", "name": "In Mylist"},
					{"id": 2.0, "hash": "backlogged", "name": "Backlogged Release"},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	p := NewProvider("test-key", WithBaseURL(server.URL))
	statuses, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2 (queued entry #1 deduped against mylist, #2 merged in)", statuses)
	}

	byHash := make(map[string]debrid.DownloadStatus, len(statuses))
	for _, s := range statuses {
		byHash[s.Hash] = s
	}
	if got := byHash["already-listed"]; got.State != debrid.StateDownloading {
		t.Errorf("already-listed state = %q, want the mylist value to win, not the queued one", got.State)
	}
	backlogged, ok := byHash["backlogged"]
	if !ok {
		t.Fatal("backlogged (queued-only) torrent missing from List() results")
	}
	if backlogged.State != debrid.StateQueued {
		t.Errorf("backlogged state = %q, want queued", backlogged.State)
	}
}

// TestProvider_StatusFindsQueuedDownload proves Status() falls back to
// queued/getqueued instead of reporting "not found" for a torrent TorBox has
// accepted but not yet started processing.
func TestProvider_StatusFindsQueuedDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/torrents/mylist":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{}})
		case "/v1/api/queued/getqueued":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    []map[string]any{{"id": 5.0, "hash": "backlogged", "name": "Backlogged Release"}},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	p := NewProvider("test-key", WithBaseURL(server.URL))
	status, err := p.Status(context.Background(), "5")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != debrid.StateQueued || status.Hash != "backlogged" {
		t.Errorf("status = %+v", status)
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
			// usenetdownload_id is a JSON number in the real API, matching
			// mylist's own numeric "id" above — confirmed live (see
			// CreateUsenetDownload).
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"usenetdownload_id": 99, "hash": "nzbhash"}})
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
		case "/v1/api/queued/getqueued":
			// Status()/List() check this too now — nothing queued in this
			// test, so an empty list, matching a real account with nothing
			// backlogged.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{}})
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
