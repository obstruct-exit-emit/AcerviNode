package qbittorrent

import (
	"net/url"
	"reflect"
	"testing"
)

// TestHandleDelete_CancelsTheFetchFirst closes, on the path *arr apps actually
// use, a race already fixed on the native API's.
//
// When an *arr app removes a download AcerviNode is still fetching — replacing
// a grab with a better release, failed-download handling, or someone clicking
// remove in the Activity view — internal/importer's fetch goroutine has no way
// to know. It keeps writing, recreating whatever DeleteLocalFiles just removed,
// then renames a finished file into a directory no row tracks any more. The
// result is an orphaned file on disk that nothing will ever clean up, plus the
// bandwidth of finishing a download nobody wanted.
//
// internal/api's handleDeleteDownload has cancelled the fetch first for exactly
// this reason. The shims never did, and the shims are where the deletes that
// hit it come from.
func TestHandleDelete_CancelsTheFetchFirst(t *testing.T) {
	for _, deleteFiles := range []string{"true", "false"} {
		t.Run("deleteFiles="+deleteFiles, func(t *testing.T) {
			settings := &fakeSettings{}
			ts, client := newTestServerWithSettings(t, settings)
			login(t, client, ts.URL)

			resp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
				"urls": testMagnet, "category": "tv-sonarr",
			})
			resp.Body.Close()

			db := ts.Config.Handler.(*Server).db
			all, err := db.ListAllDownloads(t.Context())
			if err != nil || len(all) != 1 {
				t.Fatalf("ListAllDownloads() = %v, %v, want one row", all, err)
			}
			d := all[0]

			del, err := client.PostForm(ts.URL+"/api/v2/torrents/delete", url.Values{
				"hashes": {d.Hash}, "deleteFiles": {deleteFiles},
			})
			if err != nil {
				t.Fatalf("delete error = %v", err)
			}
			del.Body.Close()

			// Cancelled whether or not files are being removed: the row is
			// going either way, and a fetch that finishes into a gone row
			// leaves exactly the orphan this prevents.
			want := []string{"cancel:" + d.ID}
			if deleteFiles == "true" {
				want = append(want, "delete:"+d.ID)
			}
			if !reflect.DeepEqual(settings.events, want) {
				t.Errorf("events = %v, want %v — the fetch must be stopped before anything is removed", settings.events, want)
			}
		})
	}
}
