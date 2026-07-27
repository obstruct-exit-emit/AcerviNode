package torbox

import (
	"context"
	"fmt"

	"github.com/acervinode/acervinode/internal/debrid"
)

// UsenetProvider adapts Client to debrid.UsenetProvider. It's built on the
// same *Client as Provider — TorBox genuinely runs both services under one
// account/API key, unlike the qBittorrent/SABnzbd split which is purely a
// compat-shim concern (see docs/providers.md).
type UsenetProvider struct {
	client *Client
}

// NewUsenetProvider builds a usenet-capable TorBox provider.
func NewUsenetProvider(apiKey string, opts ...Option) *UsenetProvider {
	return &UsenetProvider{client: NewClient(apiKey, opts...)}
}

func (p *UsenetProvider) Name() string { return "torbox" }

func (p *UsenetProvider) AddNZBFile(ctx context.Context, filename string, data []byte, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	id, _, err := p.client.CreateUsenetDownload(ctx, CreateUsenetDownloadRequest{File: data, Filename: filename, Name: opts.Name})
	if err != nil {
		return "", fmt.Errorf("torbox: add nzb file: %w", err)
	}
	return debrid.ProviderDownloadID(id), nil
}

func (p *UsenetProvider) AddNZBURL(ctx context.Context, url string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	id, _, err := p.client.CreateUsenetDownload(ctx, CreateUsenetDownloadRequest{Link: url, Name: opts.Name})
	if err != nil {
		return "", fmt.Errorf("torbox: add nzb url: %w", err)
	}
	return debrid.ProviderDownloadID(id), nil
}

// idMatches compares a usenet download's list-endpoint numeric id against the
// string id AcerviNode stored when it was created. TorBox's create response
// returns "usenetdownload_id" as a string while the list/mylist response
// returns a numeric "id" — per the official SDK's own struct definitions,
// this asymmetry is real, not a transcription error here. Formatting the
// numeric id the same way torrent ids are formatted is the working
// assumption; if TorBox ever returns a usenetdownload_id that isn't simply
// that same integer as a string, this comparison needs revisiting against a
// live account (see docs/providers.md).
func idMatches(numericID float64, wantID string) bool {
	return formatID(numericID) == wantID
}

func (p *UsenetProvider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	downloads, err := p.client.ListUsenetDownloads(ctx)
	if err != nil {
		return debrid.DownloadStatus{}, fmt.Errorf("torbox: usenet status: %w", err)
	}
	for _, d := range downloads {
		if idMatches(d.ID, string(id)) {
			return usenetToStatus(d), nil
		}
	}
	return debrid.DownloadStatus{}, fmt.Errorf("torbox: usenet download %s not found", id)
}

func (p *UsenetProvider) List(ctx context.Context) ([]debrid.DownloadStatus, error) {
	downloads, err := p.client.ListUsenetDownloads(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox: usenet list: %w", err)
	}
	out := make([]debrid.DownloadStatus, 0, len(downloads))
	for _, d := range downloads {
		out = append(out, usenetToStatus(d))
	}
	return out, nil
}

func (p *UsenetProvider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	downloads, err := p.client.ListUsenetDownloads(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox: usenet files: %w", err)
	}
	for _, d := range downloads {
		if idMatches(d.ID, string(id)) {
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
	return nil, fmt.Errorf("torbox: usenet download %s not found", id)
}

func (p *UsenetProvider) RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error) {
	url, err := p.client.RequestUsenetDownloadLink(ctx, string(id), fileID)
	if err != nil {
		return "", fmt.Errorf("torbox: usenet request download link: %w", err)
	}
	return url, nil
}

func (p *UsenetProvider) Delete(ctx context.Context, id debrid.ProviderDownloadID, _ bool) error {
	if err := p.client.ControlUsenetDownload(ctx, string(id), OpDelete); err != nil {
		return fmt.Errorf("torbox: usenet delete: %w", err)
	}
	return nil
}

func usenetToStatus(d UsenetDownload) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:         debrid.ProviderDownloadID(formatID(d.ID)),
		Name:       d.Name,
		SizeBytes:  int64(d.Size),
		Progress:   d.Progress,
		State:      mapDownloadState(d.DownloadState), // TorBox shares one state vocabulary across both services
		ETASeconds: int64(d.Eta),
		RawState:   d.DownloadState,
	}
}
