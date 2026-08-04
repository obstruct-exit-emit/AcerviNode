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
	_ debrid.AccountProvider = (*Provider)(nil)
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
// TestMagnetFromHash proves a torrent's OriginalURL is always reconstructed
// from its hash alone — confirmed live that TorBox itself doesn't reliably
// record an original magnet/original_url for a torrent (a real magnet-added
// torrent's mylist entry had both fields null) — and that an empty hash
// (e.g. a torrent still mid-indexing) produces "" rather than a bogus,
// hash-less magnet.
func TestMagnetFromHash(t *testing.T) {
	if got := magnetFromHash("abc123"); got != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("magnetFromHash(abc123) = %q", got)
	}
	if got := magnetFromHash(""); got != "" {
		t.Errorf("magnetFromHash(\"\") = %q, want empty", got)
	}
}

// TestTorrentToStatus_PopulatesOriginalURL proves the reconstructed magnet
// flows through into debrid.DownloadStatus.OriginalURL, what
// database.RefreshFromProvider/internal/importer.discoverManual backfill
// Source from.
func TestTorrentToStatus_PopulatesOriginalURL(t *testing.T) {
	status := torrentToStatus(Torrent{ID: 42, Hash: "abc123"})
	if status.OriginalURL != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("OriginalURL = %q", status.OriginalURL)
	}
}

// TestTorrentToStatus_PopulatesSwarmInfo proves seeds/peers/download_speed
// pass through — found live to be entirely missing anywhere in AcerviNode's
// own data model while watching a real, genuinely uncached torrent download
// (TorBox's own instant-cache path never exercises this at all).
func TestTorrentToStatus_PopulatesSwarmInfo(t *testing.T) {
	status := torrentToStatus(Torrent{ID: 1, Seeds: 3, Peers: 1, DownloadSpeed: 191117})
	if status.Seeders != 3 {
		t.Errorf("Seeders = %d, want 3", status.Seeders)
	}
	if status.Leechers != 1 {
		t.Errorf("Leechers = %d, want 1", status.Leechers)
	}
	if status.DownloadSpeedBytes != 191117 {
		t.Errorf("DownloadSpeedBytes = %d, want 191117", status.DownloadSpeedBytes)
	}
}

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

// TestMapUsenetState proves usenet's own state mapping doesn't need to
// whitelist every "Direct Unpack: <phase>" TorBox's real usenet
// post-processing might report — any state with active=true and
// progress>0 is treated as still downloading, so a phase this test has
// never heard of (verifying/repairing/extracting/anything else) is still
// correctly bucketed as in-progress rather than falling through to
// StateError the way mapDownloadState's exact-string whitelist would. See
// mapUsenetState's own doc comment for the real bug (Viren070/AIOStreams
// #903) this design avoids repeating.
func TestMapUsenetState(t *testing.T) {
	cases := []struct {
		name string
		d    UsenetDownload
		want debrid.DownloadState
	}{
		{
			name: "plain downloading",
			d:    UsenetDownload{DownloadState: "downloading", Active: true, Progress: 0.4},
			want: debrid.StateDownloading,
		},
		{
			name: "verifying — a phase never explicitly listed anywhere in this code",
			d:    UsenetDownload{DownloadState: "Direct Unpack: Verifying", Active: true, Progress: 1.0},
			want: debrid.StateDownloading,
		},
		{
			name: "repairing — same, never explicitly listed",
			d:    UsenetDownload{DownloadState: "Direct Unpack: Repairing", Active: true, Progress: 1.0},
			want: debrid.StateDownloading,
		},
		{
			name: "extracting — same, never explicitly listed",
			d:    UsenetDownload{DownloadState: "Direct Unpack: Extracting", Active: true, Progress: 1.0},
			want: debrid.StateDownloading,
		},
		{
			name: "a hypothetical future phase this code has truly never seen",
			d:    UsenetDownload{DownloadState: "Direct Unpack: Something Brand New", Active: true, Progress: 1.0},
			want: debrid.StateDownloading,
		},
		{
			name: "processing — a real, live-confirmed TorBox state, not just a hypothetical one: a real 6.8GB usenet download sat here for several minutes mid-transfer",
			d:    UsenetDownload{DownloadState: "processing", DownloadFinished: true, DownloadPresent: false, Active: true, Progress: 1.0},
			want: debrid.StateDownloading,
		},
		{
			name: "finished and present — the ordinary completion signal",
			d:    UsenetDownload{DownloadState: "cached", DownloadFinished: true, DownloadPresent: true, Progress: 1.0},
			want: debrid.StateCompleted,
		},
		{
			name: "Direct Unpack: Completed arriving ahead of download_present — the real AIOStreams #903 case",
			d:    UsenetDownload{DownloadState: "Direct Unpack: Completed", DownloadFinished: true, DownloadPresent: false, Active: true, Progress: 1.0},
			want: debrid.StateCompleted,
		},
		{
			name: "download_finished alone, without download_present or the Direct Unpack completion string, is not done yet",
			d:    UsenetDownload{DownloadState: "Direct Unpack: Verifying", DownloadFinished: true, DownloadPresent: false, Active: true, Progress: 1.0},
			want: debrid.StateDownloading,
		},
		{
			name: "a failure state",
			d:    UsenetDownload{DownloadState: "Failed", Active: false},
			want: debrid.StateError,
		},
		{
			name: "an invalid state",
			d:    UsenetDownload{DownloadState: "Invalid NZB", Active: false},
			want: debrid.StateError,
		},
		{
			name: "a real, live-confirmed repair failure — the user supplied an NZB with too few repair blocks specifically to test this",
			d:    UsenetDownload{DownloadState: "failed (Repair failed, not enough repair blocks (165 short))", DownloadFinished: true, DownloadPresent: false, Active: false, Progress: 1},
			want: debrid.StateError,
		},
		{
			name: "empty state",
			d:    UsenetDownload{DownloadState: ""},
			want: debrid.StateUnknown,
		},
		{
			name: "genuinely queued: not active, no progress, no recognized keyword",
			d:    UsenetDownload{DownloadState: "queued", Active: false, Progress: 0},
			want: debrid.StateQueued,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapUsenetState(c.d); got != c.want {
				t.Errorf("mapUsenetState(%+v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

// TestUsenetPhase proves the substring match is robust to guessed wording —
// see usenetPhase's own doc comment for why this isn't an exact-string
// match — and that a state with no recognizable phase reports "" rather
// than guessing wrong.
func TestUsenetPhase(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"Direct Unpack: Verifying", "verifying"},
		{"Direct Unpack: Repairing", "repairing"},
		{"Direct Unpack: Extracting", "extracting"},
		{"Direct Unpack: Unpacking", "extracting"}, // guessed wording, same bucket
		{"Direct Unpack: Completed", ""},
		{"processing", "processing"}, // confirmed live: a real 6.8GB usenet download sat here for several minutes
		{"downloading", ""},
		{"cached", ""},
		{"Failed", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := usenetPhase(c.raw); got != c.want {
			t.Errorf("usenetPhase(%q) = %q, want %q", c.raw, got, c.want)
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
			if wantID := r.URL.Query().Get("id"); wantID != "" {
				// TorBox's real mylist returns a single object (not a list)
				// when filtered by id — see Client.GetTorrent.
				for _, t := range torrents {
					if formatID(t["id"].(float64)) == wantID {
						json.NewEncoder(w).Encode(map[string]any{"success": true, "data": t})
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": nil})
				return
			}
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

// TestProvider_Account proves GetUserData's plan/usage fields map onto
// debrid.AccountStatus correctly, including planName's numeric-tier mapping
// (2 = Pro, confirmed live against the real account).
func TestProvider_Account(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/user/me" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"plan": 2, "is_subscribed": true,
				"premium_expires_at":     "2027-01-01T00:00:00Z",
				"total_bytes_downloaded": 1024.0,
				"cooldown_until":         "2026-08-04T04:29:02Z",
			},
		})
	}))
	defer server.Close()

	p := NewProvider("test-key", WithBaseURL(server.URL))
	status, err := p.Account(context.Background())
	if err != nil {
		t.Fatalf("Account() error = %v", err)
	}
	if status.PlanName != "Pro" {
		t.Errorf("PlanName = %q, want Pro", status.PlanName)
	}
	if !status.IsSubscribed || status.PremiumExpiresAt != "2027-01-01T00:00:00Z" || status.TotalBytesDownloaded != 1024 {
		t.Errorf("status = %+v", status)
	}
	// CooldownUntil — a real, undocumented field found live correlating
	// with every TorBox listing endpoint silently going empty — see its own
	// doc comment on torbox.UserData.
	if status.CooldownUntil != "2026-08-04T04:29:02Z" {
		t.Errorf("CooldownUntil = %q, want passed through unchanged", status.CooldownUntil)
	}
}

func TestPlanName(t *testing.T) {
	cases := []struct {
		plan float64
		want string
	}{
		{0, "Free"}, {1, "Essential"}, {2, "Pro"}, {3, "Standard"}, {99, "Unknown"},
	}
	for _, c := range cases {
		if got := planName(c.plan); got != c.want {
			t.Errorf("planName(%v) = %q, want %q", c.plan, got, c.want)
		}
	}
}

// TestUsenetToStatus_PassesThroughOriginalURL proves a usenet download's
// original_url (confirmed live: populated for a URL-based add, null for a
// file-upload-based one) passes through unchanged into
// debrid.DownloadStatus.OriginalURL.
func TestUsenetToStatus_PassesThroughOriginalURL(t *testing.T) {
	status := usenetToStatus(UsenetDownload{ID: 1, OriginalURL: "https://example.com/release.nzb"})
	if status.OriginalURL != "https://example.com/release.nzb" {
		t.Errorf("OriginalURL = %q", status.OriginalURL)
	}

	// A file-upload-based add has no original_url — confirmed live.
	fileBased := usenetToStatus(UsenetDownload{ID: 2})
	if fileBased.OriginalURL != "" {
		t.Errorf("OriginalURL = %q, want empty for a file-upload-based add", fileBased.OriginalURL)
	}
}

// TestUsenetToStatus_PassesThroughDownloadSpeed proves download_speed —
// real, documented on TorBox's own SDK schema but unmodeled until
// internal/sabnzbd's aggregate kbpersec needed it — passes through.
func TestUsenetToStatus_PassesThroughDownloadSpeed(t *testing.T) {
	status := usenetToStatus(UsenetDownload{ID: 1, DownloadSpeed: 191117})
	if status.DownloadSpeedBytes != 191117 {
		t.Errorf("DownloadSpeedBytes = %d, want 191117", status.DownloadSpeedBytes)
	}
}

// TestUsenetToStatus_FailedRepairDoesNotReportPhaseRepairing proves the fix
// for a real bug found live testing a real, user-supplied NZB with too few
// repair blocks: TorBox's own raw failure string ("failed (Repair failed,
// not enough repair blocks (165 short))") contains "repair", which
// usenetPhase alone would happily match — usenetToStatus must not call it
// for a state that isn't actually StateDownloading, or the UI would show a
// failed download as still "Repairing".
func TestUsenetToStatus_FailedRepairDoesNotReportPhaseRepairing(t *testing.T) {
	status := usenetToStatus(UsenetDownload{
		ID:               1,
		DownloadState:    "failed (Repair failed, not enough repair blocks (165 short))",
		DownloadFinished: true,
		Progress:         1,
	})
	if status.State != debrid.StateError {
		t.Fatalf("State = %q, want error", status.State)
	}
	if status.Phase != "" {
		t.Errorf("Phase = %q, want empty for a failed (not actively repairing) download", status.Phase)
	}
	// RawState — what feeds errorMessage/fail_message — must still carry the
	// full detail, since that's what actually needs to reach the user/*arr.
	if status.RawState != "failed (Repair failed, not enough repair blocks (165 short))" {
		t.Errorf("RawState = %q, want the full raw reason preserved", status.RawState)
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
				"download_finished": true, "download_present": true, "active": true,
				"files": []map[string]any{{"id": 1.0, "name": "episode.mkv", "size": 4096.0}},
			})
			// usenetdownload_id is a JSON number in the real API, matching
			// mylist's own numeric "id" above — confirmed live (see
			// CreateUsenetDownload).
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"usenetdownload_id": 99, "hash": "nzbhash"}})
		case "/v1/api/usenet/mylist":
			if wantID := r.URL.Query().Get("id"); wantID != "" {
				// TorBox's real mylist returns a single object (not a list)
				// when filtered by id — see Client.GetUsenetDownload.
				for _, d := range downloads {
					if formatID(d["id"].(float64)) == wantID {
						json.NewEncoder(w).Encode(map[string]any{"success": true, "data": d})
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": nil})
				return
			}
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

var _ debrid.TorrentInfoProvider = (*Provider)(nil)

func TestProvider_TorrentInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/torrents/torrentinfo" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"name": "Preview.Me", "hash": "beefcafe", "size": 999.0,
				"seeds": 5, "peers": 2,
				"files": []map[string]any{{"name": "Preview.Me/a.mkv", "size": 900.0}},
			},
		})
	}))
	defer server.Close()

	p := NewProvider("test-key", WithBaseURL(server.URL))
	info, err := p.TorrentInfo(context.Background(), "beefcafe")
	if err != nil {
		t.Fatalf("TorrentInfo() error = %v", err)
	}
	if info.Name != "Preview.Me" || info.SizeBytes != 999 || info.Seeds != 5 || info.Peers != 2 {
		t.Errorf("info = %+v", info)
	}
	if len(info.Files) != 1 || info.Files[0].Path != "Preview.Me/a.mkv" || info.Files[0].SizeBytes != 900 {
		t.Errorf("info.Files = %+v", info.Files)
	}
}

func TestUsenetProvider_CheckCached(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/usenet/checkcached" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"md5hash": map[string]any{"hash": "md5hash"}},
		})
	}))
	defer server.Close()

	p := NewUsenetProvider("test-key", WithBaseURL(server.URL))
	result, err := p.CheckCached(context.Background(), []string{"md5hash", "othermd5"})
	if err != nil {
		t.Fatalf("CheckCached() error = %v", err)
	}
	if !result["md5hash"] || result["othermd5"] {
		t.Errorf("result = %+v", result)
	}
}
