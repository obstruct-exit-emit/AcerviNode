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
// string id AcerviNode stored when it was created. Both createusenetdownload's
// "usenetdownload_id" and mylist's "id" are JSON numbers — confirmed live
// against a real account (the official SDK's docs describe the create
// response's id as a string, which turned out not to match reality; see
// CreateUsenetDownload) — so both are formatted the same way torrent ids are.
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
	// Not in mylist yet doesn't mean TorBox doesn't know about it — see
	// Provider.Status's identical torrent-side reasoning and ListQueued.
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
