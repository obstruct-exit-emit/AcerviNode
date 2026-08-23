package sabnzbd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// listCachedProvider is the optional half of this shim's provider,
// implemented by debrid's Dynamic*Provider wrapper — the same pointer
// internal/importer holds. Going through it means one provider listing per
// interval serves the importer and every connected *arr app at once,
// instead of this handler fetching its own copy on every request. Optional
// so a plain provider (this package's test fake) still works, fetching
// directly as before.
type listCachedProvider interface {
	ListCached(ctx context.Context) ([]debrid.DownloadStatus, time.Time, error)
}

// refreshFromProvider syncs every tracked usenet download's local state
// against one provider List() call. See database.RefreshFromProvider, which
// this and internal/importer's own proactive background refresh both share,
// so an *arr app polling here still gets the freshest possible view even
// between importer ticks. Also returns each row's current ETA and sub-phase
// (see debrid.DownloadStatus.Phase), both keyed by provider download ID, and
// totalSpeedBytes — the sum of every download's current speed, matching
// real SABnzbd's own mode=queue shape: an aggregate speed across the whole
// queue, not a per-item field (confirmed against SABnzbd's real API docs —
// there is no per-slot speed in the real API to match even if AcerviNode
// wanted one). All three are fast-moving, purely informational values the
// provider recomputes on every call, so unlike state/progress/size none of
// them are persisted to the database, just read fresh and attached to the
// response here (see toQueueSlot/handleQueue).
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) (eta map[liveKey]int64, phase map[liveKey]string, totalSpeedBytes int64) {
	// Grouped by provider: each account is listed once, and only about its
	// own rows. Listing every registered provider regardless would ask
	// accounts about downloads that aren't theirs and cost a request per
	// provider even when nothing tracked belongs to it.
	byProvider := map[string][]*database.Download{}
	for _, d := range rows {
		name := d.Provider
		if name == "" {
			name = s.registry.DefaultNameFor(debrid.KindUsenet)
		}
		byProvider[name] = append(byProvider[name], d)
	}

	eta = map[liveKey]int64{}
	phase = map[liveKey]string{}
	for name, group := range byProvider {
		p := s.registry.Usenet(name)
		if p == nil {
			slog.Warn("sabnzbd: no provider available, skipping refresh", "provider", name, "downloads", len(group))
			continue
		}
		statuses, fetchedAt, err := p.ListCached(ctx)
		if err != nil {
			slog.Error("sabnzbd: provider list failed", "provider", name, "error", err)
			continue
		}
		// Deliberately no missing-detection: this runs on every *arr poll,
		// with no rate-limit backoff and no view of whether the provider has
		// been answering reliably. Concluding a download vanished is
		// internal/importer's bulk pass's job — see database.RefreshOptions.
		s.db.RefreshFromProvider(ctx, group, statuses, fetchedAt, database.RefreshOptions{})
		for _, st := range statuses {
			k := liveKey{provider: name, id: string(st.ID)}
			eta[k] = st.ETASeconds
			phase[k] = st.Phase
			totalSpeedBytes += st.DownloadSpeedBytes
		}
	}
	return eta, phase, totalSpeedBytes
}

// liveKey identifies one download's live status. Keyed by provider as well
// as id: two providers can legitimately issue the same id, and merging them
// into one map would let one account's numbers be reported for another's
// download.
type liveKey struct {
	provider string
	id       string
}

// liveKeyFor is d's key in the maps refreshFromProvider returns, applying
// the same empty-provider fallback the refresh itself used so the two agree.
func liveKeyFor(d *database.Download, defaultProvider string) liveKey {
	name := d.Provider
	if name == "" {
		name = defaultProvider
	}
	return liveKey{provider: name, id: d.ProviderDownloadID}
}

type queueSlot struct {
	NzoID      string `json:"nzo_id"`
	Filename   string `json:"filename"`
	Cat        string `json:"cat"`
	Status     string `json:"status"`
	Percentage string `json:"percentage"`
	MB         string `json:"mb"`
	MBLeft     string `json:"mbleft"`
	TimeLeft   string `json:"timeleft"`
}

// handleQueue implements mode=queue: everything not yet actually on local
// disk. provider_completed stays here (as "Downloading") rather than moving
// to history — the provider is done, but internal/importer hasn't fetched
// the files yet, and Sonarr's import step would find nothing if told
// otherwise (see docs/quickstart.md's Phase 1 caveat, now closed). Also
// handles name=delete (see handleDelete) — SABnzbd's real API layers delete
// onto the same mode as the list it removes from, rather than a separate one.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("name") == "delete" {
		s.handleDelete(w, r)
		return
	}

	ctx := r.Context()
	rows, err := s.db.ListDownloads(ctx, database.KindUsenet)
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	eta, phase, totalSpeedBytes := s.refreshFromProvider(ctx, rows)

	slots := make([]queueSlot, 0, len(rows))
	for _, d := range rows {
		switch d.State {
		case database.StateQueued, database.StateDownloading, database.StateProviderCompleted:
			fetchProgress, hasFetchProgress := s.db.FetchProgress(d.ID)
			k := liveKeyFor(d, s.registry.DefaultNameFor(debrid.KindUsenet))
			slots = append(slots, toQueueSlot(d, eta[k], phase[k], fetchProgress, hasFetchProgress))
		}
	}
	// kbpersec is real SABnzbd's own aggregate-speed field, at the top of
	// the queue object (not per-slot — see refreshFromProvider's own doc
	// comment), formatted as a decimal string the same way real SABnzbd
	// does (e.g. "1296.02").
	kbPerSec := float64(totalSpeedBytes) / 1024
	writeJSON(w, map[string]any{"queue": map[string]any{
		"slots":    slots,
		"kbpersec": fmt.Sprintf("%.2f", kbPerSec),
	}})
}

func toQueueSlot(d *database.Download, etaSeconds int64, phase string, fetchProgress float64, hasFetchProgress bool) queueSlot {
	status := "Queued"
	switch d.State {
	case database.StateProviderCompleted:
		// The provider itself is done; internal/importer's own fetch-to-
		// local-disk step (Completed Download Handling) hasn't finished yet
		// — the closest real-SABnzbd equivalent is its own "Moving" phase
		// (post-processing already done, now placing the finished files
		// into their final location), which happens locally there the same
		// way this fetch does here. Confirmed safe against Sonarr's real
		// source (Sabnzbd.cs's GetQueue): "Moving," like every other
		// in-progress SabnzbdDownloadStatus, falls to its catch-all
		// DownloadItemStatus.Downloading — never treated as ready to
		// import, so this can't trigger Sonarr looking for files before
		// they're actually placed, same as reporting "Downloading" always
		// safely did.
		status = "Moving"
	case database.StateDownloading:
		status = sabnzbdPhaseStatus(phase)
	}
	// EffectiveProgress substitutes internal/importer's own live local-
	// transfer progress in for d.Progress (already 1.0 by this point) while
	// the download is provider_completed ("Moving," above) — see its own
	// doc comment. Without this, Sonarr/Radarr's Activity view would show
	// this item frozen at 100% for however long the actual local copy
	// takes, the exact same gap the qBittorrent shim's Progress field had.
	progress := database.EffectiveProgress(d, fetchProgress, hasFetchProgress)
	mb := float64(d.SizeBytes) / 1_000_000
	return queueSlot{
		NzoID:      d.ID,
		Filename:   d.Name,
		Cat:        d.Category,
		Status:     status,
		Percentage: fmt.Sprintf("%.0f", progress*100),
		MB:         fmt.Sprintf("%.2f", mb),
		MBLeft:     fmt.Sprintf("%.2f", mb*(1-progress)),
		TimeLeft:   formatTimeLeft(etaSeconds),
	}
}

// sabnzbdPhaseStatus maps a usenet-specific sub-phase (see
// debrid.DownloadStatus.Phase) to the matching real-SABnzbd status string
// Sonarr/Radarr's own SabnzbdDownloadStatus enum already recognizes
// (confirmed against Sonarr's real source — Verifying/Repairing/Extracting
// are first-class enum members there, not something this risks Sonarr
// choking on). "" — no specific phase known, or a torrent download's own
// provider status (which has no such concept) — falls back to the generic
// "Downloading" this endpoint has always reported.
//
// "processing" is TorBox's own generic post-download, pre-ready bucket —
// confirmed live (download_finished=true, download_present=false,
// active=true; see torbox.usenetPhase's own doc comment) — with no exact
// real-SABnzbd equivalent: TorBox's help center describes it as opaque
// background work, not specifically verify, repair, or extract. Reported as
// "Verifying" — real SABnzbd's own first post-download step, the closest
// safe match for the same pipeline position (after download_finished,
// before download_present) — rather than sending the literal word
// "Processing", which has no member in Sonarr's SabnzbdDownloadStatus enum
// and risks a deserialization error there instead of just an imprecise (but
// safe) label here.
func sabnzbdPhaseStatus(phase string) string {
	switch phase {
	case "verifying", "processing":
		return "Verifying"
	case "repairing":
		return "Repairing"
	case "extracting":
		return "Extracting"
	default:
		return "Downloading"
	}
}

// formatTimeLeft matches real SABnzbd's H:MM:SS queue slot format (e.g.
// "0:12:34") — the shape Sonarr/Radarr's SABnzbd client parses for its queue
// ETA display. A non-positive etaSeconds (unknown/stalled/done) reports
// "0:00:00" rather than a negative or garbage duration.
func formatTimeLeft(etaSeconds int64) string {
	if etaSeconds < 0 {
		etaSeconds = 0
	}
	d := time.Duration(etaSeconds) * time.Second
	hours := int64(d.Hours())
	minutes := int64(d.Minutes()) % 60
	seconds := int64(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
}
