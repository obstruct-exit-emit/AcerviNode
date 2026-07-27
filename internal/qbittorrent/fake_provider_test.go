package qbittorrent

import (
	"context"
	"fmt"
	"sync"

	"github.com/acervinode/acervinode/internal/debrid"
)

// fakeProvider is a deterministic, in-memory debrid.TorrentProvider used only
// by this package's tests. Each entry "completes" over a small, fixed number
// of Status()/List() calls, so a test can assert a real queued -> downloading
// -> completed transition the same way *arr apps observe it by polling.
type fakeProvider struct {
	mu      sync.Mutex
	entries map[debrid.ProviderDownloadID]*fakeEntry
	nextID  int
}

type fakeEntry struct {
	hash  string
	name  string
	size  int64
	calls int
	files []debrid.DownloadFile
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{entries: map[debrid.ProviderDownloadID]*fakeEntry{}}
}

var _ debrid.TorrentProvider = (*fakeProvider)(nil)

func (f *fakeProvider) Name() string { return "faketorbox" }

func (f *fakeProvider) AddMagnet(_ context.Context, magnet string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	return f.add(magnetHash(magnet), opts.Name), nil
}

func (f *fakeProvider) AddTorrentFile(_ context.Context, filename string, _ []byte, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	name := opts.Name
	if name == "" {
		name = filename
	}
	return f.add(fmt.Sprintf("hash-of-%s", filename), name), nil
}

func (f *fakeProvider) add(hash, name string) debrid.ProviderDownloadID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := debrid.ProviderDownloadID(fmt.Sprintf("fake-%d", f.nextID))
	f.entries[id] = &fakeEntry{
		hash: hash,
		name: name,
		size: 1024,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "movie.mkv", SizeBytes: 1024},
		},
	}
	return id
}

func (f *fakeProvider) statusFor(id debrid.ProviderDownloadID, e *fakeEntry) debrid.DownloadStatus {
	state := debrid.StateQueued
	progress := 0.0
	switch {
	case e.calls <= 1:
		state, progress = debrid.StateQueued, 0
	case e.calls == 2:
		state, progress = debrid.StateDownloading, 0.5
	default:
		state, progress = debrid.StateCompleted, 1.0
	}
	return debrid.DownloadStatus{
		ID: id, Name: e.name, Hash: e.hash, SizeBytes: e.size, Progress: progress, State: state,
	}
}

func (f *fakeProvider) Status(_ context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	if !ok {
		return debrid.DownloadStatus{}, fmt.Errorf("fake provider: %s not found", id)
	}
	e.calls++
	return f.statusFor(id, e), nil
}

func (f *fakeProvider) List(_ context.Context) ([]debrid.DownloadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]debrid.DownloadStatus, 0, len(f.entries))
	for id, e := range f.entries {
		e.calls++
		out = append(out, f.statusFor(id, e))
	}
	return out, nil
}

func (f *fakeProvider) Files(_ context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	if !ok {
		return nil, fmt.Errorf("fake provider: %s not found", id)
	}
	return e.files, nil
}

func (f *fakeProvider) RequestDownloadLink(_ context.Context, id debrid.ProviderDownloadID, fileID string) (string, error) {
	return fmt.Sprintf("https://fake-cdn.test/%s/%s", id, fileID), nil
}

func (f *fakeProvider) Delete(_ context.Context, id debrid.ProviderDownloadID, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, id)
	return nil
}

func (f *fakeProvider) CheckCached(_ context.Context, hashes []string) (map[string]bool, error) {
	out := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		out[h] = false
	}
	return out, nil
}
