package torbox

import (
	"context"
	"errors"
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

// Status checks one specific torrent via mylist's id-filter (see
// Client.GetTorrent) rather than listing the whole account — internal/importer's
// fast per-download poll leans on this being cheap (see docs/providers.md), and
// every call site that resolves a single just-added/just-checked download
// (internal/api, internal/qbittorrent, internal/sabnzbd) benefits the same way.
// A rate limit is surfaced immediately, matching List()'s own behavior — every
// other GetTorrent failure (including a genuine "not found") falls through to
// the same ListQueued fallback a full-list miss would, since a backlogged add
// doesn't appear in mylist under any filter until it's promoted out of TorBox's
// pre-processing queue.
func (p *Provider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	t, err := p.client.GetTorrent(ctx, string(id))
	if err != nil {
		if errors.Is(err, debrid.ErrRateLimited) {
			return debrid.DownloadStatus{}, fmt.Errorf("torbox: status: %w", err)
		}
	} else if t.ID != 0 {
		return torrentToStatus(t), nil
	}
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

// Files uses the same id-filtered GetTorrent lookup Status does, not
// ListTorrents+scan — found necessary live, not just for consistency: a
// torrent moments old can report StateCompleted via the id-filtered lookup
// before it's indexed into the bulk listing at all, which made Files()
// spuriously fail with "not found" on internal/importer's very first fetch
// attempt right after refreshActiveDownloads' fast poll (see
// docs/providers.md) noticed it — self-healed via retry, but needlessly, and
// only because Files() was still checking a different, slower-to-update view
// of the same data.
func (p *Provider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	t, err := p.client.GetTorrent(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("torbox: files: %w", err)
	}
	if t.ID == 0 {
		return nil, fmt.Errorf("torbox: torrent %s not found", id)
	}
	return torrentFilesToDownloadFiles(t.Files), nil
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

// Account satisfies debrid.AccountProvider — GetUserData's response has many
// more fields than modeled here (see UserData's own doc comment); only what
// the settings UI's account status display actually uses is surfaced.
func (p *Provider) Account(ctx context.Context) (debrid.AccountStatus, error) {
	data, err := p.client.GetUserData(ctx)
	if err != nil {
		return debrid.AccountStatus{}, fmt.Errorf("torbox: account: %w", err)
	}
	return debrid.AccountStatus{
		PlanName:             planName(data.Plan),
		IsSubscribed:         data.IsSubscribed,
		PremiumExpiresAt:     data.PremiumExpiresAt,
		TotalBytesDownloaded: int64(data.TotalBytesDownloaded),
		CooldownUntil:        data.CooldownUntil,
	}, nil
}

// planName maps TorBox's numeric plan tier to a display name — confirmed
// live against the real account (plan: 2, a real Pro subscription) alongside
// the docs' own stated mapping.
func planName(plan float64) string {
	switch int(plan) {
	case 0:
		return "Free"
	case 1:
		return "Essential"
	case 2:
		return "Pro"
	case 3:
		return "Standard"
	default:
		return "Unknown"
	}
}

func torrentToStatus(t Torrent) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:                 debrid.ProviderDownloadID(formatID(t.ID)),
		Name:               t.Name,
		Hash:               t.Hash,
		SizeBytes:          int64(t.Size),
		Progress:           t.Progress,
		State:              mapDownloadState(t.DownloadState),
		ETASeconds:         int64(t.Eta),
		RawState:           t.DownloadState,
		OriginalURL:        magnetFromHash(t.Hash),
		Seeders:            int64(t.Seeds),
		Leechers:           int64(t.Peers),
		DownloadSpeedBytes: int64(t.DownloadSpeed),
	}
}

// magnetFromHash reconstructs a bare, genuinely resubmittable magnet URI from
// just a torrent's infohash — a torrent client/debrid service resolves the
// rest (name, trackers, files) from DHT/trackers on its own, so this doesn't
// need TorBox to have recorded the original magnet anywhere (confirmed live
// it doesn't: a real torrent's mylist entry had both magnet and
// original_url as null even though it was added via a real magnet link).
// Empty hash (e.g. a torrent still mid-indexing at discovery time, before
// TorBox has assigned one — see BackfillHashAndName) returns "" rather than
// a bogus, hash-less magnet.
func magnetFromHash(hash string) string {
	if hash == "" {
		return ""
	}
	return "magnet:?xt=urn:btih:" + hash
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
