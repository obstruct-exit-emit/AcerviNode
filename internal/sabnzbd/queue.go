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
// between importer ticks. Also returns each row's current ETA keyed by
// provider download ID — a fast-moving, purely informational value the
// provider recomputes on every call, so unlike state/progress/size it's never
// persisted to the database, just read fresh and attached to the response
// here (see toQueueSlot).
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) map[string]int64 {
	statuses, err := s.provider.List(ctx)
	if err != nil {
		slog.Error("sabnzbd: provider list failed", "error", err)
		return nil
	}
	s.db.RefreshFromProvider(ctx, rows, statuses)

	eta := make(map[string]int64, len(statuses))
	for _, st := range statuses {
		eta[string(st.ID)] = st.ETASeconds
	}
	return eta
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
	etaByProviderID := s.refreshFromProvider(ctx, rows)

	slots := make([]queueSlot, 0, len(rows))
	for _, d := range rows {
		switch d.State {
		case database.StateQueued, database.StateDownloading, database.StateProviderCompleted:
			slots = append(slots, toQueueSlot(d, etaByProviderID[d.ProviderDownloadID]))
		}
	}
	writeJSON(w, map[string]any{"queue": map[string]any{"slots": slots}})
}

func toQueueSlot(d *database.Download, etaSeconds int64) queueSlot {
	status := "Queued"
	if d.State == database.StateDownloading || d.State == database.StateProviderCompleted {
		status = "Downloading"
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
