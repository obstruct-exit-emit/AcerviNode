package torbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStatuses_CarryTorBoxUpdatedAt -- TorBox's updated_at is how fresh a
// record is, and the refresh guard orders updates by it (see
// debrid.DownloadStatus.ProviderUpdatedAt). Its bulk listing can answer a later
// request with an older record, so without it a stale answer looks current.
// Seen on production for torrents; usenet records carry the same field.
func TestStatuses_CarryTorBoxUpdatedAt(t *testing.T) {
	want := time.Date(2026, 10, 6, 2, 52, 19, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/api/torrents/mylist" && r.URL.Query().Get("id") != "":
			w.Write([]byte(`{"success":true,"data":{"id":7,"name":"t","download_state":"completed","progress":1,"updated_at":"2026-10-06T02:52:19Z"}}`))
		case r.URL.Path == "/v1/api/torrents/mylist":
			w.Write([]byte(`{"success":true,"data":[{"id":7,"name":"t","download_state":"completed","progress":1,"updated_at":"2026-10-06T02:52:19Z"}]}`))
		case r.URL.Path == "/v1/api/webdl/mylist":
			w.Write([]byte(`{"success":true,"data":[{"id":9,"name":"w","download_state":"completed","progress":1,"download_finished":true,"download_present":true,"updated_at":"2026-10-06T02:52:19Z"}]}`))
		case r.URL.Path == "/v1/api/usenet/mylist":
			w.Write([]byte(`{"success":true,"data":[{"id":8,"name":"u","download_state":"completed","progress":1,"download_finished":true,"download_present":true,"updated_at":"2026-10-06T02:52:19Z"}]}`))
		default:
			w.Write([]byte(`{"success":true,"data":[]}`))
		}
	}))
	t.Cleanup(server.Close)
	ctx := context.Background()

	check := func(name string, got *time.Time) {
		t.Helper()
		if got == nil || !got.Equal(want) {
			t.Errorf("%s: ProviderUpdatedAt = %v, want %v", name, got, want)
		}
	}

	tp := NewProvider("k", WithBaseURL(server.URL))
	list, err := tp.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("torrent List() = %v, %v", list, err)
	}
	check("torrent List", list[0].ProviderUpdatedAt)
	st, err := tp.Status(ctx, "7")
	if err != nil {
		t.Fatalf("torrent Status() error = %v", err)
	}
	check("torrent Status", st.ProviderUpdatedAt)

	up := NewUsenetProvider("k", WithBaseURL(server.URL))
	ulist, err := up.List(ctx)
	if err != nil || len(ulist) != 1 {
		t.Fatalf("usenet List() = %v, %v", ulist, err)
	}
	check("usenet List", ulist[0].ProviderUpdatedAt)

	wp := NewWebDownloadProvider("k", WithBaseURL(server.URL))
	wlist, err := wp.List(ctx)
	if err != nil || len(wlist) != 1 {
		t.Fatalf("webdl List() = %v, %v", wlist, err)
	}
	check("webdl List", wlist[0].ProviderUpdatedAt)
}
