package torbox

import (
	"context"
	"fmt"
	"regexp"

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
	// Not in mylist yet doesn't mean TorBox doesn't know about it — a
	// backlogged add sits in a separate pre-processing queue until promoted
	// (see ListQueued) and won't appear in mylist until then.
	if queued, err := p.client.ListQueued(ctx, "torrent"); err == nil {
		for _, q := range queued {
			if formatID(q.ID) == string(id) {
				return queuedToStatus(q), nil
			}
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
	seen := make(map[string]bool, len(torrents))
	for _, t := range torrents {
		out = append(out, torrentToStatus(t))
		seen[formatID(t.ID)] = true
	}
	// Merge in anything still sitting in TorBox's pre-processing queue —
	// mylist won't list it at all until it's promoted out of there, so
	// without this a backlogged download is indistinguishable from one
	// TorBox has never heard of (see ListQueued). Best-effort: a failure
	// here shouldn't discard perfectly good mylist data.
	if queued, err := p.client.ListQueued(ctx, "torrent"); err == nil {
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

func (p *Provider) RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error) {
	url, err := p.client.RequestTorrentZipDownloadLink(ctx, string(id))
	if err != nil {
		return "", fmt.Errorf("torbox: request zip download link: %w", err)
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

// queuedToStatus maps a QueuedDownload — shared by both the torrent and
// usenet services — into AcerviNode's provider-agnostic status shape.
// Progress/size are left at zero and RawState says so explicitly: there's
// nothing more specific to report until TorBox promotes it into mylist.
func queuedToStatus(q QueuedDownload) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:       debrid.ProviderDownloadID(formatID(q.ID)),
		Name:     q.Name,
		Hash:     q.Hash,
		State:    debrid.StateQueued,
		RawState: "queued (pre-processing, not yet in mylist)",
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

// parentheticalSuffix strips a qualifier TorBox appends to some states, e.g.
// "stalled (no seeds)" -> "stalled", before matching against the known state
// lists below.
var parentheticalSuffix = regexp.MustCompile(`\s*\(.*?\)\s*`)

// downloadingStates and completedStates are TorBox's real download_state
// vocabulary (shared by both the torrent and usenet services — see
// docs/providers.md), largely borrowed from qBittorrent's own state strings.
// Ported from decypharr's own production-proven mapping (the reference
// implementation this project benchmarks against — see ROADMAP.md) rather
// than guessed, since TorBox's official docs don't publish an exhaustive
// list. "completed" here means "fully fetched by TorBox", not "ready to
// serve" — "cached"/"uploading" are the actual ready-to-download signals.
var downloadingStates = map[string]bool{
	"paused": true, "downloading": true, "checkingResumeData": true, "metaDL": true,
	"pausedUP": true, "queuedUP": true, "checkingUP": true, "forcedUP": true,
	"allocating": true, "pausedDL": true, "queuedDL": true, "checkingDL": true,
	"forcedDL": true, "moving": true, "incomplete": true,
}

var completedStates = map[string]bool{
	"completed": true, "cached": true, "uploading": true, "downloaded": true,
}

// mapDownloadState translates a raw download_state into AcerviNode's
// provider-agnostic DownloadState. Anything unmatched — this is the
// important part, not an oversight — is treated as an error, not "still
// downloading": TorBox's own help center documents an explicit "Error" state
// (server error, missing encryption key, missing par2 files, etc.), and a
// stalled/no-seeds torrent is exactly the kind of dead end decypharr's own
// mapping treats the same way rather than waiting on it forever.
func mapDownloadState(raw string) debrid.DownloadState {
	if raw == "" {
		return debrid.StateUnknown
	}
	normalized := parentheticalSuffix.ReplaceAllString(raw, "")
	switch {
	case downloadingStates[normalized]:
		return debrid.StateDownloading
	case completedStates[normalized]:
		return debrid.StateCompleted
	default:
		return debrid.StateError
	}
}
