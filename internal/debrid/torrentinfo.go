package debrid

import "context"

// TorrentInfoFile is one file within a TorrentInfo preview.
type TorrentInfoFile struct {
	Path      string
	SizeBytes int64
}

// TorrentInfo is a preview of a torrent's contents straight from the
// BitTorrent network — distinct from DownloadStatus, which is about a
// download AcerviNode is actually tracking. Queried by hash, before ever
// adding anything, so a user (or *arr app) can see what they're about to
// commit to.
type TorrentInfo struct {
	Name      string
	Hash      string
	SizeBytes int64
	Seeds     int64
	Peers     int64
	Files     []TorrentInfoFile
}

// TorrentInfoProvider is implemented by a provider that can preview a
// torrent's metadata by hash alone, without adding it to the account first.
// Not every provider needs to — same "structural, not every provider needs
// every capability" approach as AccountProvider/UsenetProvider/
// WebDownloadProvider — a provider that doesn't simply doesn't satisfy this
// interface, and callers fall back to having nothing to preview.
type TorrentInfoProvider interface {
	TorrentInfo(ctx context.Context, hash string) (TorrentInfo, error)
}
