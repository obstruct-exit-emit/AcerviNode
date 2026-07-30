package torbox

import (
	"context"
	"fmt"

	"github.com/acervinode/acervinode/internal/debrid"
)

// WebDownloadProvider adapts Client to debrid.WebDownloadProvider. Built on
// the same *Client as Provider/UsenetProvider — TorBox runs all three
// services under one account/API key.
type WebDownloadProvider struct {
	client *Client
}

// NewWebDownloadProvider builds a Web-Downloads-capable TorBox provider.
func NewWebDownloadProvider(apiKey string, opts ...Option) *WebDownloadProvider {
	return &WebDownloadProvider{client: NewClient(apiKey, opts...)}
}

func (p *WebDownloadProvider) Name() string { return "torbox" }

func (p *WebDownloadProvider) AddLink(ctx context.Context, link string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	id, _, err := p.client.CreateWebDownload(ctx, CreateWebDownloadRequest{Link: link, Name: opts.Name})
	if err != nil {
		return "", fmt.Errorf("torbox: add web download: %w", err)
	}
	return debrid.ProviderDownloadID(id), nil
}

func (p *WebDownloadProvider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	downloads, err := p.client.ListWebDownloads(ctx)
	if err != nil {
		return debrid.DownloadStatus{}, fmt.Errorf("torbox: web download status: %w", err)
	}
	for _, d := range downloads {
		if formatID(d.ID) == string(id) {
			return webDownloadToStatus(d), nil
		}
	}
	// Not in mylist yet doesn't mean TorBox doesn't know about it — see
	// Provider.Status's identical torrent-side reasoning and ListQueued.
	// TorBox's queue endpoint only recognizes "torrent"/"usenet" as a type,
	// per its own docs — Web Downloads doesn't have a separate pre-processing
	// queue the same way, so there's no equivalent check to make here.
	return debrid.DownloadStatus{}, fmt.Errorf("torbox: web download %s not found", id)
}

func (p *WebDownloadProvider) List(ctx context.Context) ([]debrid.DownloadStatus, error) {
	downloads, err := p.client.ListWebDownloads(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox: web download list: %w", err)
	}
	out := make([]debrid.DownloadStatus, 0, len(downloads))
	for _, d := range downloads {
		out = append(out, webDownloadToStatus(d))
	}
	return out, nil
}

func (p *WebDownloadProvider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	downloads, err := p.client.ListWebDownloads(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox: web download files: %w", err)
	}
	for _, d := range downloads {
		if formatID(d.ID) == string(id) {
			out := make([]debrid.DownloadFile, 0, len(d.Files))
			for _, f := range d.Files {
				out = append(out, debrid.DownloadFile{
					ProviderFileID: formatID(f.ID),
					Path:           f.Name,
					SizeBytes:      int64(f.Size),
				})
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("torbox: web download %s not found", id)
}

func (p *WebDownloadProvider) RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error) {
	url, err := p.client.RequestWebDownloadLink(ctx, string(id), fileID)
	if err != nil {
		return "", fmt.Errorf("torbox: web download request link: %w", err)
	}
	return url, nil
}

func (p *WebDownloadProvider) RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error) {
	url, err := p.client.RequestWebDownloadZipDownloadLink(ctx, string(id))
	if err != nil {
		return "", fmt.Errorf("torbox: web download request zip link: %w", err)
	}
	return url, nil
}

func (p *WebDownloadProvider) Delete(ctx context.Context, id debrid.ProviderDownloadID, _ bool) error {
	if err := p.client.ControlWebDownload(ctx, string(id), OpDelete); err != nil {
		return fmt.Errorf("torbox: web download delete: %w", err)
	}
	return nil
}

func webDownloadToStatus(d WebDownload) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:          debrid.ProviderDownloadID(formatID(d.ID)),
		Name:        d.Name,
		Hash:        d.Hash,
		SizeBytes:   int64(d.Size),
		Progress:    d.Progress,
		State:       mapDownloadState(d.DownloadState), // TorBox shares one state vocabulary across all three services
		ETASeconds:  int64(d.Eta),
		RawState:    d.DownloadState,
		OriginalURL: d.OriginalURL,
	}
}
