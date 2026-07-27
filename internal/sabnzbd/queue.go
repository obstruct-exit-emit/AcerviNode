package sabnzbd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
)

// refreshFromProvider syncs every tracked usenet download's local state
// against one provider List() call. See database.RefreshFromProvider, which
// this and internal/importer's own proactive background refresh both share,
// so an *arr app polling here still gets the freshest possible view even
// between importer ticks.
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) {
	statuses, err := s.provider.List(ctx)
	if err != nil {
		slog.Error("sabnzbd: provider list failed", "error", err)
		return
	}
	s.db.RefreshFromProvider(ctx, rows, statuses)
}

type queueSlot struct {
	NzoID      string `json:"nzo_id"`
	Filename   string `json:"filename"`
	Cat        string `json:"cat"`
	Status     string `json:"status"`
	Percentage string `json:"percentage"`
	MB         string `json:"mb"`
	MBLeft     string `json:"mbleft"`
}

// handleQueue implements mode=queue: everything not yet actually on local
// disk. provider_completed stays here (as "Downloading") rather than moving
// to history — the provider is done, but internal/importer hasn't fetched
// the files yet, and Sonarr's import step would find nothing if told
// otherwise (see docs/quickstart.md's Phase 1 caveat, now closed).
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.ListDownloads(ctx, database.KindUsenet)
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	s.refreshFromProvider(ctx, rows)

	slots := make([]queueSlot, 0, len(rows))
	for _, d := range rows {
		switch d.State {
		case database.StateQueued, database.StateDownloading, database.StateProviderCompleted:
			slots = append(slots, toQueueSlot(d))
		}
	}
	writeJSON(w, map[string]any{"queue": map[string]any{"slots": slots}})
}

func toQueueSlot(d *database.Download) queueSlot {
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
	}
}
