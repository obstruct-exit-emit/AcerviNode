package debrid

import "context"

// WebDownloadProvider backs Web Downloads — a debrid service that resolves
// direct links from hoster sites (Mega, 1Fichier, Mediafire, and many
// others; the exact list is provider-defined and can change, so this
// interface doesn't try to enumerate them) rather than torrents or NZBs.
// Kept separate from TorrentProvider/UsenetProvider — not every debrid
// service has this capability, same reasoning as UsenetProvider — and
// there's no *arr-facing compat shim for it at all: neither qBittorrent nor
// SABnzbd has a "paste a hoster link" download-client concept, so every
// WebDownloadProvider download is added directly through the native API,
// never auto-fetched the way a Managed download is.
type WebDownloadProvider interface {
	Name() string
	AddLink(ctx context.Context, link string, opts AddOptions) (ProviderDownloadID, error)
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
	// CheckCached reports, per hash, whether the web download is already
	// cached on the provider's side — see TorrentProvider.CheckCached's own
	// doc comment for the shared reasoning. Providers without a cache-check
	// endpoint may return all-false rather than implement real logic.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
}
