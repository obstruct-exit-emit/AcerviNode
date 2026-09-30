package qbittorrent

import (
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
)

// TestQbtStalled pins when a torrent is reported as "stalledDL".
//
// Found on the first production burn-in: Amphibia S01E37's re-grab was a dead
// LimeTorrents torrent -- TorBox showed 0 seeds and no progress -- and this
// shim reported it to Sonarr as ordinary "downloading", so Sonarr showed it as
// healthy indefinitely. Real qBittorrent reports such a torrent as
// "stalledDL", which Sonarr and Radarr map to a Warning ("stalled with no
// connections"; confirmed against their source). A Warning is only a flag:
// it does not trigger failed-download handling, so nothing is blocklisted on
// our say-so.
//
// The rule errs toward "downloading", since a false warning is noise in the
// operator's queue: it needs a cached live status, no seeders AND no speed,
// the provider still downloading, and the grab older than the grace period.
func TestQbtStalled(t *testing.T) {
	now := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)
	dead := database.LiveStatus{Seeders: 0, Leechers: 1, DownloadSpeedBytes: 0}

	tests := []struct {
		name    string
		state   string
		added   time.Time
		live    database.LiveStatus
		hasLive bool
		want    bool
	}{
		{"the Amphibia torrent: no seeds, no speed, an hour old", database.StateDownloading, old, dead, true, true},
		{"seeders but momentarily no speed is not stalled", database.StateDownloading, old, database.LiveStatus{Seeders: 4}, true, false},
		{"speed from peers without a full seed is not stalled", database.StateDownloading, old, database.LiveStatus{Leechers: 3, DownloadSpeedBytes: 50_000}, true, false},
		{"nothing cached yet (just restarted) is unknown, not stalled", database.StateDownloading, old, database.LiveStatus{}, false, false},
		{"a fresh grab gets time to find peers", database.StateDownloading, fresh, dead, true, false},
		{"provider finished, our fetch in progress: speed is naturally 0", database.StateProviderCompleted, old, dead, true, false},
		{"queued stays queued", database.StateQueued, old, dead, true, false},
		{"ready for import is not stalled", database.StateReadyForImport, old, dead, true, false},
		{"an error stays an error", database.StateError, old, dead, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &database.Download{Kind: database.KindTorrent, State: tc.state, AddedAt: tc.added}
			if got := qbtStalled(d, tc.live, tc.hasLive, now); got != tc.want {
				t.Errorf("qbtStalled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInfo_DeadTorrentIsReportedStalled -- the same, end to end through
// /api/v2/torrents/info, with the live status filled the way internal/importer's
// poll fills it.
func TestInfo_DeadTorrentIsReportedStalled(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	insert := func(id, hash string) *database.Download {
		d := &database.Download{
			ID: id, Provider: fakeProviderName, ProviderDownloadID: "p-" + id,
			Kind: database.KindTorrent, Hash: hash, Name: id,
			State: database.StateDownloading, AddedVia: database.AddedViaArr,
			AddedAt: time.Now().UTC().Add(-time.Hour),
		}
		if err := db.InsertDownload(t.Context(), d); err != nil {
			t.Fatalf("InsertDownload(%s) error = %v", id, err)
		}
		return d
	}
	deadRow := insert("dead", "dead1")
	liveRow := insert("alive", "live1")

	// List advances each entry one step: calls 1 -> 2 is "downloading".
	provider := newFakeProvider()
	provider.entries["p-dead"] = &fakeEntry{name: "dead", size: 1024, calls: 1}
	provider.entries["p-alive"] = &fakeEntry{name: "alive", size: 1024, calls: 1, seeders: 5, speed: 120_000}
	statuses, _ := provider.List(t.Context())
	db.RefreshFromProvider(t.Context(), []*database.Download{deadRow, liveRow}, statuses, time.Now().UTC(), database.RefreshOptions{})

	got := map[string]string{}
	for _, it := range getTorrentInfo(t, client, ts.URL) {
		got[it.Hash] = it.State
	}
	if got["dead1"] != "stalledDL" {
		t.Errorf("dead torrent state = %q, want stalledDL", got["dead1"])
	}
	if got["live1"] != "downloading" {
		t.Errorf("healthy torrent state = %q, want downloading", got["live1"])
	}
}
