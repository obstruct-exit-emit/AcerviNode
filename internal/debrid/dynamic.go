package debrid

import (
	"context"
	"errors"
	"sync"
)

// ErrNoProvider is returned by a Dynamic*Provider's methods when no
// underlying provider has been configured yet — e.g. before a TorBox API key
// has ever been set via the settings API.
var ErrNoProvider = errors.New("debrid: no provider configured")

// DynamicTorrentProvider implements TorrentProvider by delegating to
// whichever provider is currently set, so it can be swapped at runtime (see
// the settings API in internal/api) without restarting the process or
// re-registering any HTTP routes. name is fixed at construction — it
// describes the provider *slot* ("torbox"), not the specific credentialed
// instance, so it stays valid even before Set has ever been called.
type DynamicTorrentProvider struct {
	name  string
	mu    sync.RWMutex
	inner TorrentProvider
}

// NewDynamicTorrentProvider creates a wrapper with no provider configured
// yet; Set must be called before any other method succeeds.
func NewDynamicTorrentProvider(name string) *DynamicTorrentProvider {
	return &DynamicTorrentProvider{name: name}
}

// Set atomically swaps the underlying provider.
func (d *DynamicTorrentProvider) Set(p TorrentProvider) {
	d.mu.Lock()
	d.inner = p
	d.mu.Unlock()
}

// Configured reports whether Set has been called with a non-nil provider.
func (d *DynamicTorrentProvider) Configured() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.inner != nil
}

func (d *DynamicTorrentProvider) current() (TorrentProvider, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.inner == nil {
		return nil, ErrNoProvider
	}
	return d.inner, nil
}

func (d *DynamicTorrentProvider) Name() string { return d.name }

func (d *DynamicTorrentProvider) AddMagnet(ctx context.Context, magnetURI string, opts AddOptions) (ProviderDownloadID, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.AddMagnet(ctx, magnetURI, opts)
}

func (d *DynamicTorrentProvider) AddTorrentFile(ctx context.Context, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.AddTorrentFile(ctx, filename, data, opts)
}

func (d *DynamicTorrentProvider) Status(ctx context.Context, id ProviderDownloadID) (DownloadStatus, error) {
	p, err := d.current()
	if err != nil {
		return DownloadStatus{}, err
	}
	return p.Status(ctx, id)
}

func (d *DynamicTorrentProvider) List(ctx context.Context) ([]DownloadStatus, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.List(ctx)
}

func (d *DynamicTorrentProvider) Files(ctx context.Context, id ProviderDownloadID) ([]DownloadFile, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.Files(ctx, id)
}

func (d *DynamicTorrentProvider) RequestDownloadLink(ctx context.Context, id ProviderDownloadID, fileID string) (string, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.RequestDownloadLink(ctx, id, fileID)
}

func (d *DynamicTorrentProvider) RequestZipDownloadLink(ctx context.Context, id ProviderDownloadID) (string, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.RequestZipDownloadLink(ctx, id)
}

func (d *DynamicTorrentProvider) Delete(ctx context.Context, id ProviderDownloadID, deleteFiles bool) error {
	p, err := d.current()
	if err != nil {
		return err
	}
	return p.Delete(ctx, id, deleteFiles)
}

func (d *DynamicTorrentProvider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.CheckCached(ctx, hashes)
}

// DynamicUsenetProvider is DynamicTorrentProvider's counterpart for
// UsenetProvider — see its docs for the rationale.
type DynamicUsenetProvider struct {
	name  string
	mu    sync.RWMutex
	inner UsenetProvider
}

func NewDynamicUsenetProvider(name string) *DynamicUsenetProvider {
	return &DynamicUsenetProvider{name: name}
}

func (d *DynamicUsenetProvider) Set(p UsenetProvider) {
	d.mu.Lock()
	d.inner = p
	d.mu.Unlock()
}

func (d *DynamicUsenetProvider) Configured() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.inner != nil
}

func (d *DynamicUsenetProvider) current() (UsenetProvider, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.inner == nil {
		return nil, ErrNoProvider
	}
	return d.inner, nil
}

func (d *DynamicUsenetProvider) Name() string { return d.name }

func (d *DynamicUsenetProvider) AddNZBFile(ctx context.Context, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.AddNZBFile(ctx, filename, data, opts)
}

func (d *DynamicUsenetProvider) AddNZBURL(ctx context.Context, url string, opts AddOptions) (ProviderDownloadID, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.AddNZBURL(ctx, url, opts)
}

func (d *DynamicUsenetProvider) Status(ctx context.Context, id ProviderDownloadID) (DownloadStatus, error) {
	p, err := d.current()
	if err != nil {
		return DownloadStatus{}, err
	}
	return p.Status(ctx, id)
}

func (d *DynamicUsenetProvider) List(ctx context.Context) ([]DownloadStatus, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.List(ctx)
}

func (d *DynamicUsenetProvider) Files(ctx context.Context, id ProviderDownloadID) ([]DownloadFile, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.Files(ctx, id)
}

func (d *DynamicUsenetProvider) RequestDownloadLink(ctx context.Context, id ProviderDownloadID, fileID string) (string, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.RequestDownloadLink(ctx, id, fileID)
}

func (d *DynamicUsenetProvider) RequestZipDownloadLink(ctx context.Context, id ProviderDownloadID) (string, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.RequestZipDownloadLink(ctx, id)
}

func (d *DynamicUsenetProvider) Delete(ctx context.Context, id ProviderDownloadID, deleteFiles bool) error {
	p, err := d.current()
	if err != nil {
		return err
	}
	return p.Delete(ctx, id, deleteFiles)
}
