package torbox

import (
	"context"
	"errors"
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

// Status is UsenetProvider's counterpart to Provider.Status — same
// GetUsenetDownload id-filter/cost reasoning and ListQueued fallback.
func (p *UsenetProvider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	d, err := p.client.GetUsenetDownload(ctx, string(id))
	if err != nil {
		if errors.Is(err, debrid.ErrRateLimited) {
			return debrid.DownloadStatus{}, fmt.Errorf("torbox: usenet status: %w", err)
		}
	} else if d.ID != 0 {
		return usenetToStatus(d), nil
	}
	if queued, err := p.client.ListQueued(ctx, "usenet"); err == nil {
		for _, q := range queued {
			if formatID(q.ID) == string(id) {
				return queuedToStatus(q), nil
			}
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
	seen := make(map[string]bool, len(downloads))
	for _, d := range downloads {
		out = append(out, usenetToStatus(d))
		seen[formatID(d.ID)] = true
	}
	// See Provider.List's identical reasoning: merge in anything still
	// sitting in TorBox's pre-processing queue, best-effort.
	if queued, err := p.client.ListQueued(ctx, "usenet"); err == nil {
		for _, q := range queued {
			id := formatID(q.ID)
			if seen[id] {
				continue
			}
			out = append(out, queuedToStatus(q))
		}
	}
	return out, nil
}

// Files uses the same id-filtered GetUsenetDownload lookup Status does — see
// Provider.Files' identical reasoning: the bulk listing can lag behind a
// targeted lookup for a download that's only moments old.
func (p *UsenetProvider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	d, err := p.client.GetUsenetDownload(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("torbox: usenet files: %w", err)
	}
	if d.ID == 0 {
		return nil, fmt.Errorf("torbox: usenet download %s not found", id)
	}
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

func (p *UsenetProvider) RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error) {
	url, err := p.client.RequestUsenetDownloadLink(ctx, string(id), fileID)
	if err != nil {
		return "", fmt.Errorf("torbox: usenet request download link: %w", err)
	}
	return url, nil
}

func (p *UsenetProvider) RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error) {
	url, err := p.client.RequestUsenetZipDownloadLink(ctx, string(id))
	if err != nil {
		return "", fmt.Errorf("torbox: usenet request zip download link: %w", err)
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
		ID:          debrid.ProviderDownloadID(formatID(d.ID)),
		Name:        d.Name,
		SizeBytes:   int64(d.Size),
		Progress:    d.Progress,
		State:       mapDownloadState(d.DownloadState), // TorBox shares one state vocabulary across both services
		ETASeconds:  int64(d.Eta),
		RawState:    d.DownloadState,
		OriginalURL: d.OriginalURL,
	}
}
