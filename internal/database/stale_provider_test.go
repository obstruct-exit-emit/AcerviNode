package database

import (
	"context"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

// These tests replay what was recorded on production on 2026-10-06, polling
// TorBox side by side with AcerviNode while CantiNode grabbed two albums.
//
// TorBox's bulk listing is not consistent from one call to the next. One call
// reported "Always Ascending" completed (TorBox's own updated_at 02:52:19);
// a later call still reported it metaDL at 0% with the old updated_at
// 02:48:19. AcerviNode's ordering guard compares when *it* asked, so the
// later-asked, older answer won: the row went from provider_completed --
// mid-copy -- back to downloading at 0%, and a dead torrent went from error
// back to downloading. Earlier the same night two albums sat at 0% for about
// four minutes after TorBox had them ready, then flipped in the same second.
//
// TorBox's per-download lookup returned 404 for both new torrents, so the
// fast poll could not help; the bulk list was the only source.

func ts(hhmmss string) *time.Time {
	t, err := time.Parse("2006-01-02T15:04:05Z", "2026-10-06T"+hhmmss+"Z")
	if err != nil {
		panic(err)
	}
	return &t
}

func refreshOne(db *DB, d *Download, st debrid.DownloadStatus, fetchedAt time.Time) {
	st.ID = debrid.ProviderDownloadID(d.ProviderDownloadID)
	db.RefreshFromProvider(context.Background(), []*Download{d}, []debrid.DownloadStatus{st}, fetchedAt, RefreshOptions{})
}

func insertDownloading(t *testing.T, db *DB) *Download {
	t.Helper()
	d := newTestDownload(KindTorrent)
	d.State = StateDownloading
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	return d
}

func stateInDB(t *testing.T, db *DB, id string) (string, float64) {
	t.Helper()
	got, err := db.GetDownloadByID(context.Background(), id)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	return got.State, got.Progress
}

// TestRefreshFromProvider_OlderProviderRecordDoesNotOverwriteNewer is the
// production sequence itself: the fresh answer first, the stale one asked
// later.
func TestRefreshFromProvider_OlderProviderRecordDoesNotOverwriteNewer(t *testing.T) {
	db := openTestDB(t)
	d := insertDownloading(t, db)
	base := time.Now()

	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateCompleted, Progress: 1, ProviderUpdatedAt: ts("02:52:19")}, base)
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0, ProviderUpdatedAt: ts("02:48:19")}, base.Add(15*time.Second))

	if state, progress := stateInDB(t, db, d.ID); state != StateProviderCompleted || progress != 1 {
		t.Errorf("state = %s at %.2f, want provider_completed at 1.00 -- an older TorBox record must not overwrite a newer one", state, progress)
	}
}

// TestRefreshFromProvider_NewerProviderRecordWinsEvenIfAskedEarlier is the
// mirror: the fresh answer was asked first but landed last. Judged by
// AcerviNode's own clock it looks stale; judged by TorBox's it is the newest.
func TestRefreshFromProvider_NewerProviderRecordWinsEvenIfAskedEarlier(t *testing.T) {
	db := openTestDB(t)
	d := insertDownloading(t, db)
	base := time.Now()

	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0, ProviderUpdatedAt: ts("02:48:19")}, base.Add(15*time.Second))
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateCompleted, Progress: 1, ProviderUpdatedAt: ts("02:52:19")}, base)

	if state, _ := stateInDB(t, db, d.ID); state != StateProviderCompleted {
		t.Errorf("state = %s, want provider_completed -- the newer TorBox record must win", state)
	}
}

// TestRefreshFromProvider_SameProviderRecordFallsBackToFetchOrder -- equal
// provider timestamps say nothing about which is newer, so the existing rule
// (later-asked wins) decides, as it always did.
func TestRefreshFromProvider_SameProviderRecordFallsBackToFetchOrder(t *testing.T) {
	db := openTestDB(t)
	d := insertDownloading(t, db)
	base := time.Now()

	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.9, ProviderUpdatedAt: ts("02:50:00")}, base.Add(15*time.Second))
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.5, ProviderUpdatedAt: ts("02:50:00")}, base)

	if _, progress := stateInDB(t, db, d.ID); progress != 0.9 {
		t.Errorf("progress = %.2f, want 0.90 -- with equal provider times the later-asked answer still wins", progress)
	}
}

// TestRefreshFromProvider_NeverRegressesProviderCompleted is the safety net
// that holds even for a provider with no record timestamp (AllDebrid), or one
// whose timestamps are wrong: once the provider has said "done" and the row
// is waiting for, or in the middle of, its local copy, no refresh moves it
// back to downloading or queued.
func TestRefreshFromProvider_NeverRegressesProviderCompleted(t *testing.T) {
	for _, back := range []debrid.DownloadState{debrid.StateDownloading, debrid.StateQueued} {
		t.Run(string(back), func(t *testing.T) {
			db := openTestDB(t)
			d := insertDownloading(t, db)
			base := time.Now()

			refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateCompleted, Progress: 1}, base)
			refreshOne(db, d, debrid.DownloadStatus{State: back, Progress: 0}, base.Add(15*time.Second))

			if state, progress := stateInDB(t, db, d.ID); state != StateProviderCompleted || progress != 1 {
				t.Errorf("state = %s at %.2f, want provider_completed at 1.00", state, progress)
			}
		})
	}
}

// TestRefreshFromProvider_ProviderCompletedCanStillFail -- the safety net
// blocks going backwards, not every change: a provider that reports a real
// failure after completion is still heard.
func TestRefreshFromProvider_ProviderCompletedCanStillFail(t *testing.T) {
	db := openTestDB(t)
	d := insertDownloading(t, db)
	base := time.Now()

	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateCompleted, Progress: 1}, base)
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateError, Progress: 1, RawState: "missingFiles"}, base.Add(15*time.Second))

	if state, _ := stateInDB(t, db, d.ID); state != StateError {
		t.Errorf("state = %s, want error -- a real failure after completion must still land", state)
	}
}

// TestRefreshFromProvider_OlderProviderRecordCannotLowerProgress isolates the
// record-time guard from the provider_completed safety net: a stale answer
// that would only lower progress, with no state going backwards.
func TestRefreshFromProvider_OlderProviderRecordCannotLowerProgress(t *testing.T) {
	db := openTestDB(t)
	d := insertDownloading(t, db)
	base := time.Now()

	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.9, ProviderUpdatedAt: ts("02:51:00")}, base)
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.3, ProviderUpdatedAt: ts("02:49:00")}, base.Add(15*time.Second))

	if _, progress := stateInDB(t, db, d.ID); progress != 0.9 {
		t.Errorf("progress = %.2f, want 0.90 -- an older record must not lower it", progress)
	}
}

// TestRefreshFromProvider_UndatedUpdateKeepsTheLastRecordTime -- an update
// with no record time between two dated ones must not wipe out what the guard
// knows, or the stale dated answer after it would slip through.
func TestRefreshFromProvider_UndatedUpdateKeepsTheLastRecordTime(t *testing.T) {
	db := openTestDB(t)
	d := insertDownloading(t, db)
	base := time.Now()

	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.9, ProviderUpdatedAt: ts("02:51:00")}, base)
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.9}, base.Add(10*time.Second))
	refreshOne(db, d, debrid.DownloadStatus{State: debrid.StateDownloading, Progress: 0.3, ProviderUpdatedAt: ts("02:49:00")}, base.Add(20*time.Second))

	if _, progress := stateInDB(t, db, d.ID); progress != 0.9 {
		t.Errorf("progress = %.2f, want 0.90 -- the stale dated answer after an undated one must still be caught", progress)
	}
}
