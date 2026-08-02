package sabnzbd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/acervinode/acervinode/internal/database"
)

// refreshFromProvider syncs every tracked usenet download's local state
// against one provider List() call. See database.RefreshFromProvider, which
// this and internal/importer's own proactive background refresh both share,
// so an *arr app polling here still gets the freshest possible view even
// between importer ticks. Also returns each row's current ETA and sub-phase
// (see debrid.DownloadStatus.Phase), both keyed by provider download ID —
// fast-moving, purely informational values the provider recomputes on every
// call, so unlike state/progress/size neither is persisted to the database,
// just read fresh and attached to the response here (see toQueueSlot).
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) (eta map[string]int64, phase map[string]string) {
	statuses, err := s.provider.List(ctx)
	if err != nil {
		slog.Error("sabnzbd: provider list failed", "error", err)
		return nil, nil
	}
	s.db.RefreshFromProvider(ctx, rows, statuses)

	eta = make(map[string]int64, len(statuses))
	phase = make(map[string]string, len(statuses))
	for _, st := range statuses {
		eta[string(st.ID)] = st.ETASeconds
		phase[string(st.ID)] = st.Phase
	}
	return eta, phase
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
	etaByProviderID, phaseByProviderID := s.refreshFromProvider(ctx, rows)

	slots := make([]queueSlot, 0, len(rows))
	for _, d := range rows {
		switch d.State {
		case database.StateQueued, database.StateDownloading, database.StateProviderCompleted:
			slots = append(slots, toQueueSlot(d, etaByProviderID[d.ProviderDownloadID], phaseByProviderID[d.ProviderDownloadID]))
		}
	}
	writeJSON(w, map[string]any{"queue": map[string]any{"slots": slots}})
}

func toQueueSlot(d *database.Download, etaSeconds int64, phase string) queueSlot {
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
	mb := float64(d.SizeBytes) / 1_000_000
	return queueSlot{
		NzoID:      d.ID,
		Filename:   d.Name,
		Cat:        d.Category,
		Status:     status,
		Percentage: fmt.Sprintf("%.0f", d.Progress*100),
		MB:         fmt.Sprintf("%.2f", mb),
		MBLeft:     fmt.Sprintf("%.2f", mb*(1-d.Progress)),
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
