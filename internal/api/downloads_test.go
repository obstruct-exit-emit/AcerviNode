package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// TestRetryDownload_AcceptsAnyProviderFinishedState pins the widened rule.
//
// Retry used to require StateError, so a download that was wrong without
// having given up had no way back. The case that motivated this: a
// ready_for_import row whose files never landed answered 409, leaving
// delete-and-re-add as the only escape. That cause is fixed, but the shape
// recurs, and refusing to re-run a fetch is a poor answer to it.
func TestRetryDownload_AcceptsAnyProviderFinishedState(t *testing.T) {
	for _, state := range []string{
		database.StateError,
		database.StateReadyForImport,
		database.StateProviderCompleted,
	} {
		t.Run(state, func(t *testing.T) {
			srv, db := newTestServer(t, &fakeProvider{}, nil, nil)
			d := seedDownloadInState(t, db, "p-"+state, state)

			rec := doRetry(t, srv, d.ID)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for %s: %s", rec.Code, state, rec.Body.String())
			}
			got, err := db.GetDownloadByID(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("GetDownloadByID() error = %v", err)
			}
			if got.State != database.StateProviderCompleted {
				t.Errorf("state = %q, want provider_completed so the fetch runs again", got.State)
			}
		})
	}
}

// TestRetryDownload_RefusesWhileStillInFlight is the other half: forcing a
// queued or downloading row to provider_completed would have the importer
// fetch something the provider has not produced yet.
func TestRetryDownload_RefusesWhileStillInFlight(t *testing.T) {
	for _, state := range []string{database.StateQueued, database.StateDownloading} {
		t.Run(state, func(t *testing.T) {
			srv, db := newTestServer(t, &fakeProvider{}, nil, nil)
			d := seedDownloadInState(t, db, "p-"+state, state)

			rec := doRetry(t, srv, d.ID)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 for %s", rec.Code, state)
			}
			got, _ := db.GetDownloadByID(t.Context(), d.ID)
			if got.State != state {
				t.Errorf("state = %q, want it left at %q", got.State, state)
			}
		})
	}
}

// seedDownloadInState seeds a row already in the state under test —
// seedDownload always starts one downloading.
func seedDownloadInState(t *testing.T, db *database.DB, pid, state string) *database.Download {
	t.Helper()
	d := seedDownload(t, db, database.KindTorrent, pid)
	if err := db.UpdateDownloadStatus(t.Context(), d.ID, state, 1.0, d.SizeBytes, nil, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}
	d.State = state
	return d
}

func doRetry(t *testing.T, srv *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/downloads/"+id+"/retry", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}
