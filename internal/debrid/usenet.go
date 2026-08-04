package debrid

import "context"

// UsenetProvider backs the SABnzbd compat shim (internal/sabnzbd). It's kept
// separate from TorrentProvider — not folded into one bigger interface —
// because not every debrid service has a real usenet backend. A provider
// implements this only if it genuinely supports NZB downloads; cmd/acervinode
// checks at startup whether the configured provider satisfies this interface
// and only mounts the SABnzbd shim if it does.
type UsenetProvider interface {
	Name() string
	AddNZBFile(ctx context.Context, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error)
	AddNZBURL(ctx context.Context, url string, opts AddOptions) (ProviderDownloadID, error)
	Status(ctx context.Context, id ProviderDownloadID) (DownloadStatus, error)
	List(ctx context.Context) ([]DownloadStatus, error)
	Files(ctx context.Context, id ProviderDownloadID) ([]DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id ProviderDownloadID, fileID string) (string, error)
	// RequestZipDownloadLink resolves one URL for every file in a download at
	// once, zipped provider-side — for a "download all" action that doesn't
	// need per-file requests. Providers without a native zip endpoint may
	// return a not-implemented-style error rather than faking it locally.
	RequestZipDownloadLink(ctx context.Context, id ProviderDownloadID) (string, error)
	Delete(ctx context.Context, id ProviderDownloadID, deleteFiles bool) error
	// CheckCached reports, per hash, whether the usenet download is already
	// cached on the provider's side — see TorrentProvider.CheckCached's own
	// doc comment for the shared reasoning. Providers without a cache-check
	// endpoint may return all-false rather than implement real logic.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
}
