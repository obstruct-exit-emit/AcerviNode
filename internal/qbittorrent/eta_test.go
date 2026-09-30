package qbittorrent

import (
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
)

// TestInfo_UnknownETAUsesQBittorrentsSentinel.
//
// qBittorrent has a specific value for "I don't know how long this will take":
// 8640000 (100 days). Sonarr's and Radarr's GetRemainingTime handle it
// explicitly — `if (torrent.Eta == 8640000) return null;`, commented
// "qBittorrent sends eta=8640000 if unknown such as queued" — and also treat
// anything above a year, or negative, as unknown.
//
// We were sending 0 whenever no ETA had been cached for a row, which is not
// "unknown" to an *arr app: it parses as zero seconds remaining, so every queued
// download claimed to be finishing immediately.
func TestInfo_UnknownETAUsesQBittorrentsSentinel(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	// A freshly-added row with nothing cached for it yet — the common case, and
	// exactly when an *arr app is most likely to be watching.
	insertRow(t, db, "arr-1", "aaa1", "Queued Grab", database.AddedViaArr, database.StateQueued)

	items := getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 {
		t.Fatalf("/info returned %d items, want 1", len(items))
	}
	if items[0].Eta != qbtUnknownETA {
		t.Errorf("eta = %d, want %d (qBittorrent's unknown sentinel, which *arr maps to null)", items[0].Eta, qbtUnknownETA)
	}
}

// TestInfo_KnownETAIsReportedAsIs — the sentinel must not swallow a real value.
func TestInfo_KnownETAIsReportedAsIs(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	d := insertRow(t, db, "arr-1", "aaa1", "Active Grab", database.AddedViaArr, database.StateDownloading)

	// Fill the live cache the way internal/importer's poll does.
	provider := newFakeProvider()
	provider.entries["p-arr-1"] = &fakeEntry{name: "Active Grab", size: 1024, calls: 1, eta: 742}
	statuses, _ := provider.List(t.Context())
	db.RefreshFromProvider(t.Context(), []*database.Download{d}, statuses, time.Now().UTC(), database.RefreshOptions{})

	items := getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 {
		t.Fatalf("/info returned %d items, want 1", len(items))
	}
	if items[0].Eta != 742 {
		t.Errorf("eta = %d, want the provider's own 742", items[0].Eta)
	}
}

// TestQbtETA covers the mapping directly, including the values *arr also treats
// as unknown so we never emit one by accident.
func TestQbtETA(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"zero means unknown, not imminent", 0, qbtUnknownETA},
		{"negative means unknown", -1, qbtUnknownETA},
		{"a real ETA passes through", 742, 742},
		{"one second passes through", 1, 1},
		{"beyond a year is unknown anyway, normalised to the sentinel", 400 * 24 * 3600, qbtUnknownETA},
		{"the sentinel itself is stable", qbtUnknownETA, qbtUnknownETA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := qbtETA(tc.in); got != tc.want {
				t.Errorf("qbtETA(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
