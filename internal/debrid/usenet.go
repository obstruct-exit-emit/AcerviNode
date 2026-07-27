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
	Delete(ctx context.Context, id ProviderDownloadID, deleteFiles bool) error
}
