package torbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

func (p *UsenetProvider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	result, err := p.client.CheckCachedUsenet(ctx, hashes)
	if err != nil {
		return nil, fmt.Errorf("torbox: usenet check cached: %w", err)
	}
	return result, nil
}

func usenetToStatus(d UsenetDownload) debrid.DownloadStatus {
	state := mapUsenetState(d)
	// Phase is only meaningful for an in-progress download — usenetPhase
	// substring-matches the raw state, and a *failed* repair (e.g. TorBox's
	// real "failed (Repair failed, not enough repair blocks (165 short))")
	// contains "repair" too, which would otherwise mislabel a genuinely
	// errored download as still "Repairing" in the UI. Found live testing a
	// real NZB the user supplied specifically because it fails this way.
	var phase string
	if state == debrid.StateDownloading {
		phase = usenetPhase(d.DownloadState)
	}
	return debrid.DownloadStatus{
		ID:                 debrid.ProviderDownloadID(formatID(d.ID)),
		Name:               d.Name,
		SizeBytes:          int64(d.Size),
		Progress:           d.Progress,
		State:              state,
		ETASeconds:         int64(d.Eta),
		RawState:           d.DownloadState,
		OriginalURL:        d.OriginalURL,
		Phase:              phase,
		DownloadSpeedBytes: int64(d.DownloadSpeed),
	}
}

// usenetPhase derives a coarse, provider-agnostic sub-phase from TorBox's
// raw usenet state — see debrid.DownloadStatus.Phase. Substring-matched
// rather than exact: TorBox's precise phrasing for each "Direct Unpack:
// <phase>" isn't documented anywhere exactly (see mapUsenetState's doc
// comment for how this was sourced), so this degrades gracefully if the
// real wording turns out to differ slightly from what's guessed here — a
// state matching none of these just reports no specific phase, the same as
// it would have before this existed, rather than a wrong one. Matched
// against the suffix after the last colon, not the whole string: every
// state in this family shares the literal "Direct Unpack:" prefix, which
// itself contains "unpack" — matching the whole string would wrongly tag
// every one of them (including "Direct Unpack: Completed") as "extracting".
//
// "processing" is confirmed live, not guessed — a real usenet download on
// the real account (a 6.8GB DVD9 boxset) sat at raw state exactly
// "processing" (download_finished=true, download_present=false, active=true)
// for several minutes mid-transfer, never once reporting a "Direct Unpack:
// <phase>" string at all. TorBox's own help center documents "Processing" as
// its own distinct phase ("doing some processing in the background and
// putting the file in the correct spot... usually takes less than 5
// minutes"), separate from the Direct Unpack family — so this may be the
// more common real-world case for a straightforward download, with granular
// Direct-Unpack sub-states reserved for ones that actually need repair.
func usenetPhase(raw string) string {
	normalized := strings.ToLower(raw)
	if idx := strings.LastIndex(normalized, ":"); idx != -1 {
		normalized = normalized[idx+1:]
	}
	switch {
	case strings.Contains(normalized, "repair"):
		return "repairing"
	case strings.Contains(normalized, "verif"):
		return "verifying"
	case strings.Contains(normalized, "extract"), strings.Contains(normalized, "unpack"):
		return "extracting"
	case strings.Contains(normalized, "process"):
		return "processing"
	default:
		return ""
	}
}

// mapUsenetState translates a usenet download's raw fields into AcerviNode's
// provider-agnostic DownloadState — deliberately not mapDownloadState's
// exact-string whitelist (shared by the torrent and webdl services, ported
// from decypharr's own qBittorrent-vocabulary mapping — see
// docs/providers.md#state-mapping). TorBox's usenet service does its own
// SABnzbd-style post-processing server-side (par2 verify/repair, archive
// extraction) before a download is actually retrievable, surfaced through a
// family of download_state strings TorBox's own help center calls "Direct
// Unpack" (e.g. "Direct Unpack: Verifying", "Direct Unpack: Repairing",
// "Direct Unpack: Completed") that aren't documented exhaustively anywhere —
// and decypharr's mapping was never exercised against them in the first
// place, since decypharr runs its own separate NNTP/par2/unpack engine for
// usenet rather than routing it through TorBox at all. Whitelisting every
// "Direct Unpack: X" phase by exact string would just break again the next
// time TorBox adds or renames one (found without ever seeing that happen
// live — see docs/providers.md#usenet-post-processing-states for how this
// was sourced instead: TorBox's own help center's state descriptions, plus
// a production-proven fix for this exact gap in a comparable open-source
// project, Viren070/AIOStreams issue #903 and its torbox.ts). Following that
// same shape: DownloadPresent/DownloadFinished/Active/Progress are the
// authoritative signals, and the raw string is only consulted for the two
// outcomes that genuinely need it — a failure, and TorBox's own "Direct
// Unpack: Completed", confirmed (in that issue's own logs) to sometimes
// arrive with download_present still false, i.e. slightly ahead of the
// field TorBox itself otherwise uses to mean "ready."
func mapUsenetState(d UsenetDownload) debrid.DownloadState {
	normalized := strings.ToLower(d.DownloadState)
	switch {
	case d.DownloadFinished && (d.DownloadPresent || strings.HasPrefix(normalized, "direct unpack: completed")):
		return debrid.StateCompleted
	case strings.Contains(normalized, "fail") || strings.Contains(normalized, "invalid"):
		return debrid.StateError
	case d.Active && d.Progress > 0:
		// Covers every in-progress phase uniformly — plain "downloading" and
		// every "Direct Unpack: <phase>" TorBox might report — without
		// needing to know each one's exact spelling.
		return debrid.StateDownloading
	case d.DownloadState == "":
		return debrid.StateUnknown
	default:
		return debrid.StateQueued
	}
}
