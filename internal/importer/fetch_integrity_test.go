package importer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// flushWrite writes b and flushes, which forces chunked transfer encoding: the
// response then carries no Content-Length, so the client has nothing but the
// provider's own reported size to check the body against.
func flushWrite(w http.ResponseWriter, b []byte) {
	w.Write(b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func integrityDownload(t *testing.T, db *database.DB, id string, size int64) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: "faketorbox", ProviderDownloadID: "p-" + id, Kind: database.KindTorrent,
		Hash: "h-" + id, Name: "Integrity " + id, Category: "tv",
		SavePath: filepath.Join(t.TempDir(), "Integrity "+id),
		State:    database.StateProviderCompleted, SizeBytes: size,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	return d
}

// TestFetchFile_RejectsAShortChunkedBody is the integrity gap.
//
// A response with a Content-Length that ends early is already caught — Go's
// client returns io.ErrUnexpectedEOF. A *chunked* response has no length to
// hold it to, so one that ends cleanly but early made io.Copy return nil, and
// the .part file was renamed into place as though it were complete. The
// .part-then-rename design exists precisely so a truncated file never lands at
// the real path; it only ever protected against a crash or an error, not a
// clean short read. The download was then marked ready_for_import and handed
// to *arr as a finished release.
func TestFetchFile_RejectsAShortChunkedBody(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushWrite(w, []byte("only half")) // 9 bytes, clean end, no Content-Length
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 18},
	}}
	d := integrityDownload(t, db, "short", 18)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	err := im.processDownload(context.Background(), d)
	if err == nil {
		t.Fatal("processDownload() = nil, want an error: 9 of 18 bytes arrived")
	}
	if !strings.Contains(err.Error(), "9") || !strings.Contains(err.Error(), "18") {
		t.Errorf("error = %q, want it to name both the received and expected size", err)
	}
	if _, statErr := os.Stat(filepath.Join(d.SavePath, "episode.mkv")); !os.IsNotExist(statErr) {
		t.Error("a truncated file was renamed into place at the real destination")
	}
	if _, statErr := os.Stat(filepath.Join(d.SavePath, "episode.mkv.part")); !os.IsNotExist(statErr) {
		t.Error("the .part file was left behind after the size check failed")
	}
}

// TestFetchFile_AcceptsACompleteChunkedBody — the check must not break the
// normal chunked case it sits in front of.
func TestFetchFile_AcceptsACompleteChunkedBody(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushWrite(w, []byte("first half"))
		flushWrite(w, []byte("second hal"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 20},
	}}
	d := integrityDownload(t, db, "whole", 20)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	if err := im.processDownload(context.Background(), d); err != nil {
		t.Fatalf("processDownload() error = %v, want a complete 20-byte chunked body accepted", err)
	}
	got, err := os.ReadFile(filepath.Join(d.SavePath, "episode.mkv"))
	if err != nil || string(got) != "first halfsecond hal" {
		t.Errorf("file = %q, %v; want the full body", got, err)
	}
}

// TestFetchFile_ServerLengthIsTrustedOverAnInexactProviderSize is the
// conservative half, and the reason for it. The provider's reported size is
// only consulted when the server gave no length of its own. TorBox's sizes were
// verified exact against its CDN for torrents and web downloads, but usenet
// could not be verified, and failing every download of a kind because a
// provider reported an estimate would be far worse than the gap being closed.
// When the server states a length and delivers exactly that, the transfer is
// intact by the only authority that actually knows.
func TestFetchFile_ServerLengthIsTrustedOverAnInexactProviderSize(t *testing.T) {
	db := openTestDB(t)
	body := []byte("twelve bytes")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		w.Write(body)
	}))
	t.Cleanup(cdn.Close)

	// The provider claims 13; the server says, and sends, 12.
	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 13},
	}}
	d := integrityDownload(t, db, "inexact", 13)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	if err := im.processDownload(context.Background(), d); err != nil {
		t.Fatalf("processDownload() error = %v, want the server's own complete Content-Length honoured", err)
	}
}

// TestProcessDownload_ProgressNeverGoesBackwardsBetweenFiles covers two bugs in
// the same few lines.
//
// The boundary update after each file was onProgress(0). The closure measures
// from the byte count *before* the file started, so that reported the start of
// the file just finished rather than its end: after every file in a season
// pack, progress jumped back. The comment beside it says "exact boundary
// update" — the value it plainly meant was the file's own size.
//
// And the denominator was d.SizeBytes, which counts every file the provider
// has, while only the files that pass the configured filters are fetched. With
// a sample or an extras folder excluded, progress could never reach 100%.
//
// The CDN handler for the last file observes the boundary value left by the
// one before it, which is deterministic: nothing else writes progress between
// one file finishing and the next request arriving.
func TestProcessDownload_ProgressNeverGoesBackwardsBetweenFiles(t *testing.T) {
	db := openTestDB(t)

	var observed float64
	var observedOK bool
	var d *database.Download
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2":
			w.Write(make([]byte, 10))
		case "/3":
			observed, observedOK = db.FetchProgress(d.ID)
			w.Write(make([]byte, 30))
		default:
			t.Errorf("unexpected request for %s — the excluded file was fetched", r.URL.Path)
		}
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "Sample/sample.mkv", SizeBytes: 100}, // filtered out
		{ProviderFileID: "2", Path: "episode.nfo", SizeBytes: 10},
		{ProviderFileID: "3", Path: "episode.mkv", SizeBytes: 30},
	}}
	// d.SizeBytes is the provider's total, sample included.
	d = integrityDownload(t, db, "progress", 140)

	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)
	im.SetFileFilters(0, 0, nil, regexp.MustCompile(`(?i)sample`))

	if err := im.processDownload(context.Background(), d); err != nil {
		t.Fatalf("processDownload() error = %v", err)
	}
	if !observedOK {
		t.Fatal("no fetch progress was recorded by the time the second kept file was requested")
	}
	// 10 of the 40 bytes actually being fetched.
	if want := 0.25; observed != want {
		t.Errorf("progress after the first kept file = %v, want %v (10 of 40 kept bytes) — "+
			"0 means the boundary reported the start of the file, 10/140 means the excluded sample was counted", observed, want)
	}
}
