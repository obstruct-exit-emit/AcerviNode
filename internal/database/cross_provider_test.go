package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

func seedForProvider(t *testing.T, db *DB, provider string, n int) []*Download {
	t.Helper()
	ctx := context.Background()
	out := make([]*Download, n)
	for i := 0; i < n; i++ {
		d := newTestDownload(KindTorrent)
		d.ProviderDownloadID = fmt.Sprintf("%s-item-%d", provider, i)
		d.Provider = provider
		d.State = StateProviderCompleted
		d.AddedVia = AddedViaManual
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}
		out[i] = d
	}
	return out
}

// TestRefreshFromProvider_DoesNotFlagAnotherProvidersRows proves a refresh
// pass judges only the rows belonging to the provider it polled.
//
// This was a real, live bug. The importer listed rows by kind alone, so
// polling AllDebrid handed it every TorBox torrent row too — absent from
// AllDebrid's listing by construction, and duly flagged "no longer found in
// the provider's account" within three ticks. Healthy downloads on an
// account that was never asked about them.
//
// The ratios matter: five AllDebrid rows against two TorBox rows puts the
// wrongly-missing fraction at 28%, below the mass-vanish guard's 50%
// threshold. Above it the guard masked the bug, which is why it survived
// until a two-provider instance happened to sit under the line.
func TestRefreshFromProvider_DoesNotFlagAnotherProvidersRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	all := seedForProvider(t, db, "alldebrid", 5)
	tb := seedForProvider(t, db, "torbox", 2)
	rows := append(append([]*Download{}, all...), tb...)

	// AllDebrid's listing: only its own items.
	var statuses []debrid.DownloadStatus
	for _, d := range all {
		statuses = append(statuses, debrid.DownloadStatus{
			ID: debrid.ProviderDownloadID(d.ProviderDownloadID), State: debrid.StateCompleted, Progress: 1,
		})
	}

	start := time.Now()
	for i := 0; i < missingDetectionThreshold; i++ {
		db.RefreshFromProvider(ctx, rows, statuses, start.Add(time.Duration(i)*time.Second),
			RefreshOptions{DetectMissing: true, Provider: "alldebrid", Kind: Kind("torrent")})
	}

	for _, d := range tb {
		if d.MissingCount != 0 {
			t.Errorf("torbox row MissingCount = %d, want 0 — an alldebrid poll must not touch it", d.MissingCount)
		}
		if d.State == StateError {
			t.Errorf("torbox row flagged %q by an alldebrid poll — a provider that never held it", d.ErrorMessage)
		}
	}
}
