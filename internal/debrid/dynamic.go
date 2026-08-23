package debrid

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoProvider is returned by a Dynamic*Provider's methods when no
// underlying provider has been configured yet — e.g. before a TorBox API key
// has ever been set via the settings API.
var ErrNoProvider = errors.New("debrid: no provider configured")

// ErrRateLimited is a sentinel a concrete provider's error chain should
// include (via errors.Is/%w, not necessarily be) whenever a call failed
// specifically because the provider rate-limited the request — as opposed
// to any other failure. Provider-agnostic on purpose: internal/importer
// checks for this via errors.Is to back off its own polling specifically
// for a rate limit, without needing to know which concrete provider (or
// concrete error type, e.g. torbox.APIError) produced it — see
// docs/providers.md for the concrete torbox.APIError.Unwrap wiring.
var ErrRateLimited = errors.New("debrid: provider rate limit exceeded")

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
	// listCache is shared by every caller holding this wrapper —
	// internal/importer's poll and the compat shims' reactive refreshes —
	// so one fetch per interval serves all of them. See ListCached.
	listCache ListCache
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

// ListCached is List through the shared cache, and is what every periodic
// caller should use — see ListCache for why. The returned time is when the
// underlying provider call was *started*, which for a reused response is
// the original call's start time, not now: database.RefreshFromProvider
// gates its writes on that timestamp, so reporting a reused response as
// current would let it overwrite fresher state that landed in between.
//
// List itself is deliberately left uncached: it's part of the TorrentProvider
// interface, and a caller holding only that interface has nowhere to put
// the fetch timestamp this returns.
func (d *DynamicTorrentProvider) ListCached(ctx context.Context) ([]DownloadStatus, time.Time, error) {
	return d.listCache.List(ctx, d.List)
}

// SetListCacheTTL retunes the shared cache — see ListCache.SetTTL.
func (d *DynamicTorrentProvider) SetListCacheTTL(ttl time.Duration) {
	d.listCache.SetTTL(ttl)
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

// Account satisfies debrid.AccountProvider by delegating to the current
// inner provider, if it happens to implement AccountProvider too — reuses
// this wrapper's existing live-swap machinery rather than needing a whole
// separate DynamicAccountProvider just for one read-only call.
func (d *DynamicTorrentProvider) Account(ctx context.Context) (AccountStatus, error) {
	p, err := d.current()
	if err != nil {
		return AccountStatus{}, err
	}
	ap, ok := p.(AccountProvider)
	if !ok {
		return AccountStatus{}, fmt.Errorf("debrid: provider %q does not support account status", d.name)
	}
	return ap.Account(ctx)
}

// TorrentInfo satisfies debrid.TorrentInfoProvider by delegating to the
// current inner provider, if it happens to implement TorrentInfoProvider too
// — same type-assertion-through-the-live-swap approach as Account above.
func (d *DynamicTorrentProvider) TorrentInfo(ctx context.Context, hash string) (TorrentInfo, error) {
	p, err := d.current()
	if err != nil {
		return TorrentInfo{}, err
	}
	tip, ok := p.(TorrentInfoProvider)
	if !ok {
		return TorrentInfo{}, fmt.Errorf("debrid: provider %q does not support torrent info previews", d.name)
	}
	return tip.TorrentInfo(ctx, hash)
}

// DynamicUsenetProvider is DynamicTorrentProvider's counterpart for
// UsenetProvider — see its docs for the rationale.
type DynamicUsenetProvider struct {
	name  string
	mu    sync.RWMutex
	inner UsenetProvider
	// listCache is shared by every caller holding this wrapper —
	// internal/importer's poll and the compat shims' reactive refreshes —
	// so one fetch per interval serves all of them. See ListCached.
	listCache ListCache
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

// ListCached is List through the shared cache, and is what every periodic
// caller should use — see ListCache for why. The returned time is when the
// underlying provider call was *started*, which for a reused response is
// the original call's start time, not now: database.RefreshFromProvider
// gates its writes on that timestamp, so reporting a reused response as
// current would let it overwrite fresher state that landed in between.
//
// List itself is deliberately left uncached: it's part of the UsenetProvider
// interface, and a caller holding only that interface has nowhere to put
// the fetch timestamp this returns.
func (d *DynamicUsenetProvider) ListCached(ctx context.Context) ([]DownloadStatus, time.Time, error) {
	return d.listCache.List(ctx, d.List)
}

// SetListCacheTTL retunes the shared cache — see ListCache.SetTTL.
func (d *DynamicUsenetProvider) SetListCacheTTL(ttl time.Duration) {
	d.listCache.SetTTL(ttl)
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

func (d *DynamicUsenetProvider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.CheckCached(ctx, hashes)
}

// DynamicWebDownloadProvider is DynamicTorrentProvider's counterpart for
// WebDownloadProvider — see its docs for the rationale.
type DynamicWebDownloadProvider struct {
	name  string
	mu    sync.RWMutex
	inner WebDownloadProvider
	// listCache is shared by every caller holding this wrapper —
	// internal/importer's poll and the compat shims' reactive refreshes —
	// so one fetch per interval serves all of them. See ListCached.
	listCache ListCache
}

func NewDynamicWebDownloadProvider(name string) *DynamicWebDownloadProvider {
	return &DynamicWebDownloadProvider{name: name}
}

func (d *DynamicWebDownloadProvider) Set(p WebDownloadProvider) {
	d.mu.Lock()
	d.inner = p
	d.mu.Unlock()
}

func (d *DynamicWebDownloadProvider) Configured() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.inner != nil
}

func (d *DynamicWebDownloadProvider) current() (WebDownloadProvider, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.inner == nil {
		return nil, ErrNoProvider
	}
	return d.inner, nil
}

func (d *DynamicWebDownloadProvider) Name() string { return d.name }

func (d *DynamicWebDownloadProvider) AddLink(ctx context.Context, link string, opts AddOptions) (ProviderDownloadID, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.AddLink(ctx, link, opts)
}

func (d *DynamicWebDownloadProvider) Status(ctx context.Context, id ProviderDownloadID) (DownloadStatus, error) {
	p, err := d.current()
	if err != nil {
		return DownloadStatus{}, err
	}
	return p.Status(ctx, id)
}

func (d *DynamicWebDownloadProvider) List(ctx context.Context) ([]DownloadStatus, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.List(ctx)
}

// ListCached is List through the shared cache, and is what every periodic
// caller should use — see ListCache for why. The returned time is when the
// underlying provider call was *started*, which for a reused response is
// the original call's start time, not now: database.RefreshFromProvider
// gates its writes on that timestamp, so reporting a reused response as
// current would let it overwrite fresher state that landed in between.
//
// List itself is deliberately left uncached: it's part of the WebDownloadProvider
// interface, and a caller holding only that interface has nowhere to put
// the fetch timestamp this returns.
func (d *DynamicWebDownloadProvider) ListCached(ctx context.Context) ([]DownloadStatus, time.Time, error) {
	return d.listCache.List(ctx, d.List)
}

// SetListCacheTTL retunes the shared cache — see ListCache.SetTTL.
func (d *DynamicWebDownloadProvider) SetListCacheTTL(ttl time.Duration) {
	d.listCache.SetTTL(ttl)
}

func (d *DynamicWebDownloadProvider) Files(ctx context.Context, id ProviderDownloadID) ([]DownloadFile, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.Files(ctx, id)
}

func (d *DynamicWebDownloadProvider) RequestDownloadLink(ctx context.Context, id ProviderDownloadID, fileID string) (string, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.RequestDownloadLink(ctx, id, fileID)
}

func (d *DynamicWebDownloadProvider) RequestZipDownloadLink(ctx context.Context, id ProviderDownloadID) (string, error) {
	p, err := d.current()
	if err != nil {
		return "", err
	}
	return p.RequestZipDownloadLink(ctx, id)
}

func (d *DynamicWebDownloadProvider) Delete(ctx context.Context, id ProviderDownloadID, deleteFiles bool) error {
	p, err := d.current()
	if err != nil {
		return err
	}
	return p.Delete(ctx, id, deleteFiles)
}

func (d *DynamicWebDownloadProvider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	p, err := d.current()
	if err != nil {
		return nil, err
	}
	return p.CheckCached(ctx, hashes)
}
