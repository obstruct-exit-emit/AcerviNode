// Package debrid defines the provider interfaces both compat shims
// (internal/qbittorrent and internal/sabnzbd) are built against. Neither shim,
// nor internal/database, ever imports a concrete provider package directly —
// that's what lets a new provider be a pure addition. See docs/providers.md.
package debrid

import "context"

// ProviderDownloadID is the identifier a provider assigns to something it's
// downloading — a TorBox torrent_id, a Real-Debrid torrent id, etc. Providers
// decide their own ID format; callers treat it as opaque.
type ProviderDownloadID string

// DownloadState is AcerviNode's own, provider-agnostic view of where a
// download is in its lifecycle. Each compat shim translates this to whatever
// vocabulary its emulated protocol expects (see docs/qbittorrent-api.md and
// docs/sabnzbd-api.md) — providers never need to know either protocol.
type DownloadState string

const (
	StateQueued      DownloadState = "queued"
	StateDownloading DownloadState = "downloading"
	StateCompleted   DownloadState = "completed"
	StateError       DownloadState = "error"
	StateUnknown     DownloadState = "unknown"
)

// AddOptions carries the handful of hints a caller can give when adding a
// download. Category and save path are deliberately absent — those are a
// compat-shim/database concern (see internal/database), not a provider one.
type AddOptions struct {
	// Name, if set, hints at the display name to use; providers that infer a
	// name from the magnet/NZB itself may ignore this.
	Name string
}

// DownloadStatus is a provider's point-in-time view of one download.
type DownloadStatus struct {
	ID         ProviderDownloadID
	Name       string
	Hash       string // torrent infohash; empty for usenet downloads
	SizeBytes  int64
	Progress   float64 // 0..1
	State      DownloadState
	ETASeconds int64
	// RawState is the provider's own state string (e.g. TorBox's
	// "stalled (no seeds)"), kept only for logs/debugging — never surfaced
	// through either compat shim directly.
	RawState string
}

// DownloadFile is one file within a download.
type DownloadFile struct {
	ProviderFileID string
	Path           string
	SizeBytes      int64
}

// TorrentProvider backs the qBittorrent compat shim (internal/qbittorrent).
type TorrentProvider interface {
	Name() string
	AddMagnet(ctx context.Context, magnetURI string, opts AddOptions) (ProviderDownloadID, error)
	AddTorrentFile(ctx context.Context, filename string, data []byte, opts AddOptions) (ProviderDownloadID, error)
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
	// CheckCached reports, per hash, whether the torrent is already cached on
	// the provider's side. Providers without a cache-check endpoint may
	// return all-false rather than implement real logic.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
}
