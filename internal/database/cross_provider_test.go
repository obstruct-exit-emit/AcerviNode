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

// TestListDownloadsByProvider_ScopesToOneProvider covers the query added to
// fix cross-provider missing-detection. It was exercised only indirectly
// through the importer, so a regression in the SQL itself — a dropped WHERE
// clause, say — would surface as downloads being wrongly flagged gone rather
// than as a failing test here.
func TestListDownloadsByProvider_ScopesToOneProvider(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	seedForProvider(t, db, "alldebrid", 3)
	seedForProvider(t, db, "torbox", 2)

	all, err := db.ListDownloadsByProvider(ctx, "alldebrid", KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloadsByProvider() error = %v", err)
	}
	if len(all) != 3 {
		t.Errorf("alldebrid rows = %d, want 3", len(all))
	}
	for _, d := range all {
		if d.Provider != "alldebrid" {
			t.Errorf("returned a %q row for an alldebrid query", d.Provider)
		}
	}

	tb, err := db.ListDownloadsByProvider(ctx, "torbox", KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloadsByProvider() error = %v", err)
	}
	if len(tb) != 2 {
		t.Errorf("torbox rows = %d, want 2", len(tb))
	}

	// Kind still narrows as well as provider — a usenet query must not pick
	// up this provider's torrents.
	none, err := db.ListDownloadsByProvider(ctx, "torbox", KindUsenet)
	if err != nil {
		t.Fatalf("ListDownloadsByProvider() error = %v", err)
	}
	if len(none) != 0 {
		t.Errorf("torbox usenet rows = %d, want 0", len(none))
	}

	// An unknown provider is empty, not everything.
	unknown, err := db.ListDownloadsByProvider(ctx, "not-configured", KindTorrent)
	if err != nil {
		t.Fatalf("ListDownloadsByProvider() error = %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown provider returned %d rows, want 0", len(unknown))
	}
}

// TestListStuckDownloads_OnlyInFlightAndOnlyStale backs the stuck-download
// watchdog, which auto-errors what it returns. Untested until now, and it
// defaults to disabled — so the first time anyone switches it on would have
// been the first time this query ran in anger.
func TestListStuckDownloads_OnlyInFlightAndOnlyStale(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now()

	n := 0
	mk := func(state string, updated time.Time) string {
		d := newTestDownload(KindTorrent)
		// Unique per row: (provider, provider_download_id) is the identity
		// pair and carries a UNIQUE constraint, so the fixture's fixed id
		// would collide on the second insert.
		n++
		d.ProviderDownloadID = fmt.Sprintf("row-%d", n)
		d.State = state
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE downloads SET updated_at = ? WHERE id = ?`, updated, d.ID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		return d.ID
	}

	staleQueued := mk(StateQueued, old)
	staleDownloading := mk(StateDownloading, old)
	freshDownloading := mk(StateDownloading, recent)
	// Terminal and post-provider states are none of the watchdog's business:
	// auto-erroring a download whose files are already on disk would undo
	// completed work.
	staleReady := mk(StateReadyForImport, old)
	staleCompleted := mk(StateProviderCompleted, old)
	staleErrored := mk(StateError, old)

	got, err := db.ListStuckDownloads(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListStuckDownloads() error = %v", err)
	}
	ids := map[string]bool{}
	for _, d := range got {
		ids[d.ID] = true
	}

	for _, want := range []string{staleQueued, staleDownloading} {
		if !ids[want] {
			t.Errorf("stale in-flight download %s not returned", want)
		}
	}
	for name, id := range map[string]string{
		"recently updated":   freshDownloading,
		"ready_for_import":   staleReady,
		"provider_completed": staleCompleted,
		"already errored":    staleErrored,
	} {
		if ids[id] {
			t.Errorf("%s download was returned as stuck; the watchdog would auto-error it", name)
		}
	}
}

// TestListErroredDownloadsEligibleForCleanup_OnlyStaleErrors backs error-state
// cleanup, which deletes what it returns — local files, the provider-side
// download and the row. Also untested until now, and also disabled by default.
func TestListErroredDownloadsEligibleForCleanup_OnlyStaleErrors(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	n := 0
	mk := func(state string, updated time.Time) string {
		d := newTestDownload(KindTorrent)
		// Unique per row: (provider, provider_download_id) is the identity
		// pair and carries a UNIQUE constraint, so the fixture's fixed id
		// would collide on the second insert.
		n++
		d.ProviderDownloadID = fmt.Sprintf("row-%d", n)
		d.State = state
		if err := db.InsertDownload(ctx, d); err != nil {
			t.Fatalf("InsertDownload() error = %v", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE downloads SET updated_at = ? WHERE id = ?`, updated, d.ID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		return d.ID
	}

	old := time.Now().Add(-48 * time.Hour)
	staleError := mk(StateError, old)
	freshError := mk(StateError, time.Now())
	staleReady := mk(StateReadyForImport, old)

	got, err := db.ListErroredDownloadsEligibleForCleanup(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListErroredDownloadsEligibleForCleanup() error = %v", err)
	}
	ids := map[string]bool{}
	for _, d := range got {
		ids[d.ID] = true
	}
	if !ids[staleError] {
		t.Error("a long-errored download was not eligible for cleanup")
	}
	if ids[freshError] {
		t.Error("a freshly errored download was eligible; it would be deleted before anyone saw it")
	}
	if ids[staleReady] {
		t.Error("a ready_for_import download was returned by the *errored* cleanup query")
	}
}
