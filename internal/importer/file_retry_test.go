package importer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// Most importer tests should not sit through real retry delays.
func init() {
	fileOpenRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
}

func retryTestDownload(t *testing.T, db *database.DB, id string, size int64) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: "faketorbox", ProviderDownloadID: "p-" + id, Kind: database.KindTorrent,
		Hash: "h-" + id, Name: "Retry " + id, Category: "tv",
		SavePath: filepath.Join(t.TempDir(), "Retry "+id),
		State:    database.StateProviderCompleted, SizeBytes: size,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("InsertDownload() error = %v", err)
	}
	return d
}

func linkRequests(p *fakeProvider, fileID string) int {
	p.requestedAtMu.Lock()
	defer p.requestedAtMu.Unlock()
	n := 0
	for _, id := range p.requestedAt {
		if id == fileID {
			n++
		}
	}
	return n
}

// TestFetchFile_RetriesARefusedFileWithAFreshLink.
//
// Found on the first production burn-in: TorBox's CDN intermittently answered
// a file's download request with 400. It hit different files on each attempt
// (E07, then E15, of a 26-file season pack) and a later probe of every file's
// link came back clean, so it is transient, not a property of any file. But a
// single refused file failed the whole download attempt, throwing away the
// progress of that attempt and backing off exponentially — so a pack with
// enough files could run out of attempts and fail permanently over refusals
// that each would have succeeded a moment later.
//
// A refused file is now retried on its own, with a freshly resolved link,
// before the download attempt is given up on.
func TestFetchFile_RetriesARefusedFileWithAFreshLink(t *testing.T) {
	db := openTestDB(t)
	var hits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "link token rejected", http.StatusBadRequest)
			return
		}
		w.Write([]byte("episode bytes"))
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: int64(len("episode bytes"))},
	}}
	d := retryTestDownload(t, db, "refused-once", int64(len("episode bytes")))
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	if err := im.processDownload(context.Background(), d); err != nil {
		t.Fatalf("processDownload() error = %v, want the file retried and fetched", err)
	}
	if n := linkRequests(provider, "1"); n != 2 {
		t.Errorf("link resolved %d times, want 2: the retry must use a fresh link, not reuse the refused one", n)
	}
	got, err := os.ReadFile(filepath.Join(d.SavePath, "episode.mkv"))
	if err != nil || string(got) != "episode bytes" {
		t.Errorf("file = %q, %v; want the full body", got, err)
	}
}

// TestFetchFile_GivesUpAfterRepeatedRefusalsAndSaysWhy — bounded, and the
// error now carries what the server said, not just its status code. The
// production error read only "unexpected status 400", which left nothing to
// diagnose from.
func TestFetchFile_GivesUpAfterRepeatedRefusalsAndSaysWhy(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "  download   limit\n reached  ", http.StatusBadRequest)
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 10},
	}}
	d := retryTestDownload(t, db, "refused-always", 10)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	err := im.processDownload(context.Background(), d)
	if err == nil {
		t.Fatal("processDownload() = nil, want an error after every retry was refused")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "download limit reached") {
		t.Errorf("error = %q, want the status and the server's own (whitespace-collapsed) reason", err)
	}
	if want := len(fileOpenRetryDelays) + 1; linkRequests(provider, "1") != want {
		t.Errorf("link resolved %d times, want %d (one attempt plus one per retry delay)", linkRequests(provider, "1"), want)
	}
}

// TestFetchFile_CancelDuringARetryWaitStopsPromptly — the wait between
// retries must not hold up a delete. CancelFetch has to interrupt it at once,
// and the stop still counts as a removal rather than a failure
// (errFetchCancelled), exactly as it does mid-transfer.
func TestFetchFile_CancelDuringARetryWaitStopsPromptly(t *testing.T) {
	old := fileOpenRetryDelays
	fileOpenRetryDelays = []time.Duration{time.Minute}
	t.Cleanup(func() { fileOpenRetryDelays = old })

	db := openTestDB(t)
	refused := make(chan struct{}, 4)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusBadRequest)
		refused <- struct{}{}
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 10},
	}}
	d := retryTestDownload(t, db, "cancel-in-wait", 10)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)

	done := make(chan error, 1)
	go func() { done <- im.Tick(context.Background()) }()

	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never arrived")
	}
	time.Sleep(50 * time.Millisecond) // let it settle into the one-minute wait
	start := time.Now()
	im.CancelFetch(d.ID)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick did not return: the retry wait ignored the cancellation")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("stopping took %v, want it prompt", waited)
	}
	got, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil || got == nil {
		t.Fatalf("GetDownloadByID() = %v, %v", got, err)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0: a cancel during the retry wait is still a removal, not a failure", got.RetryCount)
	}
}

// TestFetchFile_StalledServerIsNotRetriedPerFile — only a *refused* request is
// retried per file. A server that accepts and then never answers is the idle
// timeout's job; retrying that would multiply the stall wait.
func TestFetchFile_StalledServerIsNotRetriedPerFile(t *testing.T) {
	db := openTestDB(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // accept, then say nothing
	}))
	t.Cleanup(cdn.Close)

	provider := &fakeProvider{cdn: cdn, files: []debrid.DownloadFile{
		{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 10},
	}}
	d := retryTestDownload(t, db, "stalled", 10)
	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)
	im.SetFetchTimeout(50 * time.Millisecond)

	if err := im.processDownload(context.Background(), d); err == nil {
		t.Fatal("processDownload() = nil, want the stall to fail the attempt")
	}
	if n := linkRequests(provider, "1"); n != 1 {
		t.Errorf("link resolved %d times, want 1: a stall is not a refusal and must not be retried per file", n)
	}
}
