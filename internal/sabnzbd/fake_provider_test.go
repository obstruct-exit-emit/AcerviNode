package sabnzbd

import (
	"context"
	"fmt"
	"sync"

	"github.com/acervinode/acervinode/internal/debrid"
)

// fakeProvider is a deterministic, in-memory debrid.UsenetProvider used only
// by this package's tests — same shape as internal/qbittorrent's fake, kept
// as its own package-local copy since the two shims are meant to stay
// decoupled from each other.
type fakeProvider struct {
	mu      sync.Mutex
	entries map[debrid.ProviderDownloadID]*fakeEntry
	nextID  int
	// addErr, if set, makes AddNZBFile fail -- the path that records a pending
	// *arr add. lastAddName is the name AddNZBFile was last asked to use.
	addErr      error
	lastAddName string
	// urlAdds / fileAdds record what reached the provider by each route:
	// the link handed over, or the bytes uploaded under their file name.
	urlAdds  []string
	fileAdds []fakeFileAdd
}

type fakeFileAdd struct {
	filename string
	data     []byte
}

type fakeEntry struct {
	name  string
	size  int64
	eta   int64
	speed int64
	calls int
	files []debrid.DownloadFile
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{entries: map[debrid.ProviderDownloadID]*fakeEntry{}}
}

var _ debrid.UsenetProvider = (*fakeProvider)(nil)

func (f *fakeProvider) Name() string { return "faketorbox" }

func (f *fakeProvider) AddNZBURL(_ context.Context, link string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.mu.Lock()
	f.urlAdds = append(f.urlAdds, link)
	f.lastAddName = opts.Name
	f.mu.Unlock()
	return f.add(opts.Name), nil
}

func (f *fakeProvider) AddNZBFile(_ context.Context, filename string, data []byte, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.mu.Lock()
	f.fileAdds = append(f.fileAdds, fakeFileAdd{filename: filename, data: data})
	f.lastAddName = opts.Name
	addErr := f.addErr
	f.mu.Unlock()
	if addErr != nil {
		return "", addErr
	}
	name := opts.Name
	if name == "" {
		name = filename
	}
	return f.add(name), nil
}

func (f *fakeProvider) add(name string) debrid.ProviderDownloadID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := debrid.ProviderDownloadID(fmt.Sprintf("fake-usenet-%d", f.nextID))
	f.entries[id] = &fakeEntry{
		name: name,
		size: 4096,
		files: []debrid.DownloadFile{
			{ProviderFileID: "1", Path: "episode.mkv", SizeBytes: 4096},
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
	return debrid.DownloadStatus{ID: id, Name: e.name, SizeBytes: e.size, Progress: progress, State: state, ETASeconds: e.eta, DownloadSpeedBytes: e.speed}
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

func (f *fakeProvider) RequestZipDownloadLink(_ context.Context, id debrid.ProviderDownloadID) (string, error) {
	return fmt.Sprintf("https://fake-cdn.test/%s/all.zip", id), nil
}

func (f *fakeProvider) Delete(_ context.Context, id debrid.ProviderDownloadID, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, id)
	return nil
}

func (f *fakeProvider) CheckCached(_ context.Context, _ []string) (map[string]bool, error) {
	return nil, nil
}
