package sabnzbd

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// TestHandleDelete_CancelsTheFetchFirst — see internal/qbittorrent's test of
// the same name for the full reasoning. Sonarr's SABnzbd client removes items
// through name=delete on both mode=queue and mode=history, so both routes are
// covered.
func TestHandleDelete_CancelsTheFetchFirst(t *testing.T) {
	for _, tc := range []struct{ mode, delFiles string }{
		{"queue", "1"}, {"queue", "0"}, {"history", "1"},
	} {
		t.Run("mode="+tc.mode+"/del_files="+tc.delFiles, func(t *testing.T) {
			settings := &fakeSettings{}
			ts := newTestServerWithSettings(t, settings)
			db := ts.Config.Handler.(*Server).db

			d := &database.Download{
				ID: "nzo-1", Provider: fakeProviderName, ProviderDownloadID: "p-1",
				Kind: database.KindUsenet, Name: "Some.Release",
				State: database.StateProviderCompleted, AddedVia: database.AddedViaArr,
			}
			if err := db.InsertDownload(t.Context(), d); err != nil {
				t.Fatalf("InsertDownload() error = %v", err)
			}

			resp, err := http.Get(ts.URL + "/api?mode=" + tc.mode + "&name=delete&value=nzo-1&del_files=" + tc.delFiles + "&apikey=" + testAPIKey)
			if err != nil {
				t.Fatalf("delete error = %v", err)
			}
			resp.Body.Close()

			want := []string{"cancel:nzo-1"}
			if tc.delFiles == "1" {
				want = append(want, "delete:nzo-1")
			}
			if !reflect.DeepEqual(settings.events, want) {
				t.Errorf("events = %v, want %v — the fetch must be stopped before anything is removed", settings.events, want)
			}
		})
	}
}
