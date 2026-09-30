package qbittorrent

import (
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// TestAdd_NamespacesSuppliedSavePath guards against a shared destination
// directory.
//
// internal/importer's resolveDestDir treats a non-empty save_path as the final
// destination verbatim, while every other branch appends the download's own
// name — and its doc comment promises the result is "always namespaced by the
// download's own name so sibling downloads in the same category never collide."
// An *arr-supplied save_path broke that promise: every download sent with the
// same one landed directly in it, siblings overwriting each other's files, and
// cleanupDownload/RemoveLocalFiles then os.RemoveAll'd that shared directory —
// taking every other download in it.
//
// Namespacing here rather than in resolveDestDir is deliberate: after a fetch,
// UpdateDownloadSavePath writes the resolved destination back into save_path,
// so resolveDestDir's verbatim branch is load-bearing for every already-fetched
// row. Joining the name there instead would turn <dir>/<category>/<name> into
// <dir>/<category>/<name>/<name> and break cleanup for existing downloads.
//
// It also makes the two reported fields match real qBittorrent exactly: the
// save_path an *arr app asked for stays save_path, and content_path becomes the
// per-download directory beneath it.
func TestAdd_NamespacesSuppliedSavePath(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	resp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     testMagnet,
		"category": "tv-sonarr",
		"savepath": "/data/torrents/tv",
	})
	resp.Body.Close()

	rows, err := db.ListManagedDownloads(t.Context(), database.KindTorrent)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListManagedDownloads() = %d rows, %v; want 1", len(rows), err)
	}
	d := rows[0]

	want := "/data/torrents/tv/" + d.Name
	if d.SavePath != want {
		t.Errorf("stored SavePath = %q, want %q (namespaced by the download's own name)", d.SavePath, want)
	}

	// And the pair the *arr app reads back must match real qBittorrent's
	// meaning of the two fields.
	items := getTorrentInfo(t, client, ts.URL)
	if len(items) != 1 {
		t.Fatalf("/info returned %d items, want 1", len(items))
	}
	if items[0].ContentPath != want {
		t.Errorf("content_path = %q, want the per-download directory %q", items[0].ContentPath, want)
	}
	if items[0].SavePath != "/data/torrents/tv" {
		t.Errorf("save_path = %q, want the directory the *arr app asked for", items[0].SavePath)
	}
}

// TestAdd_SavePathWithNoNameIsLeftAlone: the provider may not have reported a
// name yet, and joining an empty one would produce a trailing-separator path
// pointing at the shared directory again — the exact thing being prevented.
func TestAdd_SavePathWithNoNameIsLeftAlone(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	// A magnet with no dn= and an unknown hash: magnetDisplayName falls back to
	// the hash, so there is always something to namespace with. Assert the
	// invariant that matters instead — the stored path never ends in a
	// separator and is never the bare supplied directory.
	resp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"savepath": "/data/torrents/tv",
	})
	resp.Body.Close()

	rows, err := db.ListManagedDownloads(t.Context(), database.KindTorrent)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListManagedDownloads() = %d rows, %v; want 1", len(rows), err)
	}
	if got := rows[0].SavePath; got == "/data/torrents/tv" || got == "/data/torrents/tv/" {
		t.Errorf("stored SavePath = %q, want a per-download directory beneath it", got)
	}
}

// TestAdd_NoSavePathStaysEmpty pins the untouched path: with no save_path from
// the *arr app, the row keeps an empty one and internal/importer computes and
// persists the destination itself at fetch time (the common case — SABnzbd's
// real API has no such parameter at all, and Sonarr/Radarr's qBittorrent client
// never sends one).
func TestAdd_NoSavePathStaysEmpty(t *testing.T) {
	ts, client, db := newTestServerWithDB(t)
	login(t, client, ts.URL)

	resp := postMultipart(t, client, ts.URL+"/api/v2/torrents/add", map[string]string{
		"urls":     testMagnet,
		"category": "tv-sonarr",
	})
	resp.Body.Close()

	rows, err := db.ListManagedDownloads(t.Context(), database.KindTorrent)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListManagedDownloads() = %d rows, %v; want 1", len(rows), err)
	}
	if rows[0].SavePath != "" {
		t.Errorf("stored SavePath = %q, want empty so the importer resolves it", rows[0].SavePath)
	}
}

// TestNamespaceSavePath covers the helper directly, including the cases the
// HTTP-level tests above cannot reach cleanly. A lost add reply is reconciled by
// discovery, which stamps the row with the save_path recorded in
// pending_arr_adds — so the same helper has to be what records that value too,
// or the collision returns by a different route.
func TestNamespaceSavePath(t *testing.T) {
	tests := []struct {
		name     string
		supplied string
		dlName   string
		want     string
	}{
		{"namespaces under the supplied directory", "/data/torrents/tv", "Some.Release", "/data/torrents/tv/Some.Release"},
		{"no save_path stays empty for the importer to resolve", "", "Some.Release", ""},
		{"empty name is left alone rather than producing the bare directory", "/data/torrents/tv", "", "/data/torrents/tv"},
		{"whitespace-only name counts as no name", "/data/torrents/tv", "   ", "/data/torrents/tv"},
		{"a trailing separator on the supplied path is normalised away", "/data/torrents/tv/", "Some.Release", "/data/torrents/tv/Some.Release"},
		{"a name that is already the last segment is not doubled", "/data/torrents/tv/Some.Release", "Some.Release", "/data/torrents/tv/Some.Release"},
		{"a path separator in the name cannot escape the directory", "/data/torrents/tv", "../../etc", "/data/torrents/tv"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespaceSavePath(tc.supplied, tc.dlName); got != tc.want {
				t.Errorf("namespaceSavePath(%q, %q) = %q, want %q", tc.supplied, tc.dlName, got, tc.want)
			}
		})
	}
}
