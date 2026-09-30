package importer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// amphibiaFiles is the shape of what TorBox actually delivered for
// Amphibia.S01E37 on the first production burn-in: the raw usenet post, marked
// completed, with no media file in it.
func amphibiaFiles() []debrid.DownloadFile {
	base := "Amphibia.S01E37.1080p.AMZN.WEB-DL.DDP2.0.H.264-TVSmash/Amphibia.S01E37.Children.of.the.Spore"
	return []debrid.DownloadFile{
		{ProviderFileID: "1", Path: base + ".nfo", SizeBytes: 3996},
		{ProviderFileID: "2", Path: base + ".nzb", SizeBytes: 85682},
		{ProviderFileID: "3", Path: base + ".par2", SizeBytes: 21768},
		{ProviderFileID: "4", Path: base + ".part01.rar", SizeBytes: 19490048},
		{ProviderFileID: "5", Path: base + ".part02.rar", SizeBytes: 20000000},
		{ProviderFileID: "6", Path: base + ".srr", SizeBytes: 10416},
		{ProviderFileID: "7", Path: base + ".vol00+01.par2", SizeBytes: 1026048},
		{ProviderFileID: "8", Path: base + ".vol15+16.par2", SizeBytes: 159364877},
	}
}

func unpackRow(t *testing.T, db *database.DB, id string, kind database.Kind) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: "faketorbox", ProviderDownloadID: "p-" + id, Kind: kind,
		Hash: "h-" + id, Name: "Unpack " + id, Category: "tv", SavePath: filepath.Join(t.TempDir(), "Unpack "+id),
		State: database.StateProviderCompleted, SizeBytes: 200_000_000, AddedVia: database.AddedViaArr,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	return d
}

// TestProcessDownload_UsenetLeftPackedIsNotFetched.
//
// TorBox's usenet service repairs and extracts server-side, and its default is
// documented to leave only the wanted files. On the first production burn-in it
// did not, for one job: it marked Amphibia.S01E37 completed with only the raw
// post in its file list. AcerviNode fetched all of it -- about 1.2 GB, most of
// it par2 -- and Sonarr has sat on "Found archive file, might need to be
// extracted" ever since, because nothing will ever import it.
//
// Such a download is now recognised before a byte is fetched, and treated as a
// failed attempt with a reason that says so.
func TestProcessDownload_UsenetLeftPackedIsNotFetched(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fetched %s from a usenet download that was never unpacked", r.URL.Path)
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: amphibiaFiles()}
	d := unpackRow(t, db, "packed", database.KindUsenet)
	im := New(db, testRegistry(nil, provider), t.TempDir(), time.Minute, 5)

	err := im.processDownload(context.Background(), d)
	if err == nil {
		t.Fatal("processDownload() = nil, want a failed attempt")
	}
	msg := err.Error()
	for _, want := range []string{"not unpacked", "2 .rar", "3 .par2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to mention %q", msg, want)
		}
	}
	// Sonarr downgrades exactly this one fail_message from Failed to Warning,
	// which would stop its failed-download handling. Never send it.
	if strings.Contains(msg, "write error or disk is full") {
		t.Errorf("error = %q uses the one wording Sonarr treats as a warning", msg)
	}
	if entries, _ := os.ReadDir(d.SavePath); len(entries) != 0 {
		t.Errorf("%d entries written for a download that should not have been fetched", len(entries))
	}
}

// TestTick_UsenetLeftPackedEndsFailedWithItsReason — once retries run out it
// is an error carrying that reason, which the SABnzbd shim reports as Failed
// with it as fail_message. That is what lets Sonarr blocklist the release and
// search again, the way it would for a real SABnzbd unpack failure, instead of
// waiting on it forever.
func TestTick_UsenetLeftPackedEndsFailedWithItsReason(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fetched %s from a usenet download that was never unpacked", r.URL.Path)
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: amphibiaFiles()}
	d := unpackRow(t, db, "packed-final", database.KindUsenet)
	im := New(db, testRegistry(nil, provider), t.TempDir(), time.Minute, 1)

	if err := im.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	got, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	if got.State != database.StateError {
		t.Errorf("state = %q, want error once retries are exhausted", got.State)
	}
	if !strings.Contains(got.ErrorMessage, "not unpacked") {
		t.Errorf("error_message = %q, want the reason recorded", got.ErrorMessage)
	}
}

// TestProcessDownload_TorrentOfRarsIsStillFetched — the rule is usenet-only.
// A torrent that is nothing but a RAR set is a real, common release (many are
// packed; people extract them with unpackerr), and TorBox never unpacks
// torrents in the first place, so there is nothing that failed.
func TestProcessDownload_TorrentOfRarsIsStillFetched(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("rar bytes"))
	}))
	t.Cleanup(cdn.Close)

	files := []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "Release/release.rar", SizeBytes: int64(len("rar bytes"))},
		{ProviderFileID: "2", Path: "Release/release.r00", SizeBytes: int64(len("rar bytes"))},
		{ProviderFileID: "3", Path: "Release/release.sfv", SizeBytes: int64(len("rar bytes"))},
	}
	provider := &fakeProvider{cdn: cdn, files: files}
	d := unpackRow(t, db, "rar-torrent", database.KindTorrent)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	if err := im.processDownload(context.Background(), d); err != nil {
		t.Fatalf("processDownload() error = %v, want a RAR torrent fetched as it always was", err)
	}
}

// TestProcessDownload_UnpackedUsenetIsFetched — what TorBox normally delivers:
// the extracted content, with sources deleted. Verified live by re-submitting
// the very NZB it had failed on, which came back as exactly this.
func TestProcessDownload_UnpackedUsenetIsFetched(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("video bytes"))
	}))
	t.Cleanup(cdn.Close)

	n := int64(len("video bytes"))
	files := []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "Show/episode.mkv", SizeBytes: n},
		{ProviderFileID: "2", Path: "Show/episode.nfo", SizeBytes: n},
		{ProviderFileID: "3", Path: "Show/episode.srr", SizeBytes: n},
	}
	provider := &fakeProvider{cdn: cdn, files: files}
	d := unpackRow(t, db, "unpacked", database.KindUsenet)
	im := New(db, testRegistry(nil, provider), t.TempDir(), time.Minute, 5)

	if err := im.processDownload(context.Background(), d); err != nil {
		t.Fatalf("processDownload() error = %v, want an unpacked usenet download fetched", err)
	}
}
