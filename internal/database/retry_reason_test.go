package database

import (
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

func retryingRow(t *testing.T, db *DB, reason string) *Download {
	t.Helper()
	ctx := t.Context()
	d := &Download{
		ID: "r1", Provider: "p", ProviderDownloadID: "pr1", Kind: KindTorrent, Hash: "h1",
		Name: "Cowboy Bebop S01", State: StateProviderCompleted, Progress: 1, SizeBytes: 100,
		AddedVia: AddedViaArr,
	}
	if err := db.InsertDownload(ctx, d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	// Exactly what importer.handleFailure records after a failed fetch attempt.
	if err := db.UpdateDownloadRetry(ctx, d.ID, 2, time.Now().Add(time.Minute).UTC(), reason); err != nil {
		t.Fatalf("UpdateDownloadRetry() error = %v", err)
	}
	got, err := db.GetDownloadByID(ctx, d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	return got
}

// TestRefreshFromProvider_KeepsTheImportersRetryReason.
//
// Seen on the first production burn-in: a season pack on its third fetch
// attempt showed retry_count 3 and an empty error_message. A download being
// retried sits in provider_completed, the provider keeps reporting it
// complete, and a completed status carries no error -- so every 30-second
// refresh wrote an empty message over the importer's own reason for the last
// failed attempt. The reason lived for at most one poll interval, which is
// exactly when nobody is looking; and the row was rewritten every tick for
// nothing.
func TestRefreshFromProvider_KeepsTheImportersRetryReason(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	const reason = `fetch file "E07.mkv": download: unexpected status 400`
	d := retryingRow(t, db, reason)

	db.RefreshFromProvider(t.Context(), []*Download{d},
		[]debrid.DownloadStatus{{ID: "pr1", State: debrid.StateCompleted, Progress: 1, SizeBytes: 100}},
		time.Now().UTC(), RefreshOptions{})

	got, err := db.GetDownloadByID(t.Context(), d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	if got.ErrorMessage != reason {
		t.Errorf("ErrorMessage = %q after a refresh, want the importer's retry reason kept", got.ErrorMessage)
	}
	if got.State != StateProviderCompleted || got.RetryCount != 2 {
		t.Errorf("state/retry = %q/%d, want provider_completed/2 untouched", got.State, got.RetryCount)
	}
}

// TestRefreshFromProvider_ProviderFailureStillOverridesARetryReason — the
// provider's own verdict wins the moment it has one. If the item fails at the
// provider while the importer is between attempts, that is the more important
// thing to show.
func TestRefreshFromProvider_ProviderFailureStillOverridesARetryReason(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()

	d := retryingRow(t, db, `fetch file "E07.mkv": download: unexpected status 400`)

	db.RefreshFromProvider(t.Context(), []*Download{d},
		[]debrid.DownloadStatus{{ID: "pr1", State: debrid.StateError, RawState: "stalled (no seeds)", Progress: 1, SizeBytes: 100}},
		time.Now().UTC(), RefreshOptions{})

	got, err := db.GetDownloadByID(t.Context(), d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	if got.ErrorMessage != "stalled (no seeds)" {
		t.Errorf("ErrorMessage = %q, want the provider's own failure to win", got.ErrorMessage)
	}
}
