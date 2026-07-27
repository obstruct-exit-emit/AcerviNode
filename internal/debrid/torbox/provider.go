package torbox

import (
	"context"
	"fmt"

	"github.com/acervinode/acervinode/internal/debrid"
)

// Provider adapts Client to debrid.TorrentProvider.
type Provider struct {
	client *Client
}

// NewProvider builds a torrent-capable TorBox provider.
func NewProvider(apiKey string, opts ...Option) *Provider {
	return &Provider{client: NewClient(apiKey, opts...)}
}

func (p *Provider) Name() string { return "torbox" }

func (p *Provider) AddMagnet(ctx context.Context, magnetURI string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	id, _, err := p.client.CreateTorrent(ctx, CreateTorrentRequest{Magnet: magnetURI, Name: opts.Name})
	if err != nil {
		return "", fmt.Errorf("torbox: add magnet: %w", err)
	}
	return debrid.ProviderDownloadID(id), nil
}

func (p *Provider) AddTorrentFile(ctx context.Context, filename string, data []byte, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	id, _, err := p.client.CreateTorrent(ctx, CreateTorrentRequest{File: data, Filename: filename, Name: opts.Name})
	if err != nil {
		return "", fmt.Errorf("torbox: add torrent file: %w", err)
	}
	return debrid.ProviderDownloadID(id), nil
}

func (p *Provider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	torrents, err := p.client.ListTorrents(ctx)
	if err != nil {
		return debrid.DownloadStatus{}, fmt.Errorf("torbox: status: %w", err)
	}
	for _, t := range torrents {
		if formatID(t.ID) == string(id) {
			return torrentToStatus(t), nil
		}
	}
	return debrid.DownloadStatus{}, fmt.Errorf("torbox: torrent %s not found", id)
}

func (p *Provider) List(ctx context.Context) ([]debrid.DownloadStatus, error) {
	torrents, err := p.client.ListTorrents(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox: list: %w", err)
	}
	out := make([]debrid.DownloadStatus, 0, len(torrents))
	for _, t := range torrents {
		out = append(out, torrentToStatus(t))
	}
	return out, nil
}

func (p *Provider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	torrents, err := p.client.ListTorrents(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox: files: %w", err)
	}
	for _, t := range torrents {
		if formatID(t.ID) == string(id) {
			return torrentFilesToDownloadFiles(t.Files), nil
		}
	}
	return nil, fmt.Errorf("torbox: torrent %s not found", id)
}

func (p *Provider) RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error) {
	url, err := p.client.RequestTorrentDownloadLink(ctx, string(id), fileID)
	if err != nil {
		return "", fmt.Errorf("torbox: request download link: %w", err)
	}
	return url, nil
}

func (p *Provider) Delete(ctx context.Context, id debrid.ProviderDownloadID, _ bool) error {
	if err := p.client.ControlTorrent(ctx, string(id), OpDelete); err != nil {
		return fmt.Errorf("torbox: delete: %w", err)
	}
	return nil
}

func (p *Provider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	result, err := p.client.CheckCachedTorrents(ctx, hashes)
	if err != nil {
		return nil, fmt.Errorf("torbox: check cached: %w", err)
	}
	return result, nil
}

func torrentToStatus(t Torrent) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:         debrid.ProviderDownloadID(formatID(t.ID)),
		Name:       t.Name,
		Hash:       t.Hash,
		SizeBytes:  int64(t.Size),
		Progress:   t.Progress,
		State:      mapDownloadState(t.DownloadState),
		ETASeconds: int64(t.Eta),
		RawState:   t.DownloadState,
	}
}

func torrentFilesToDownloadFiles(files []TorrentFile) []debrid.DownloadFile {
	out := make([]debrid.DownloadFile, 0, len(files))
	for _, f := range files {
		out = append(out, debrid.DownloadFile{
			ProviderFileID: formatID(f.ID),
			Path:           f.Name,
			SizeBytes:      int64(f.Size),
		})
	}
	return out
}

// mapDownloadState translates TorBox's download_state vocabulary (shared by
// both the torrent and usenet services — see docs/providers.md) onto
// AcerviNode's provider-agnostic DownloadState. TorBox's own docs note
// "completed" here means "fully fetched by TorBox", not "ready to serve" —
// "cached"/"uploading" are the actual ready-to-download signals.
func mapDownloadState(raw string) debrid.DownloadState {
	switch raw {
	case "":
		return debrid.StateUnknown
	case "cached", "completed", "uploading":
		return debrid.StateCompleted
	default:
		// downloading, metaDL, checkingResumeData, "stalled (no seeds)",
		// paused, and any qBittorrent-vocabulary state TorBox passes through
		// are all "still in progress" from AcerviNode's point of view.
		return debrid.StateDownloading
	}
}
