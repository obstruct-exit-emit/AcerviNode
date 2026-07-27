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

// refreshFromProvider syncs every tracked usenet download's local state
// against one provider List() call, mirroring internal/qbittorrent's
// approach — a single bulk request rather than one Status() call per row.
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) {
	statuses, err := s.provider.List(ctx)
	if err != nil {
		slog.Error("sabnzbd: provider list failed", "error", err)
		return
	}
	byID := make(map[string]debrid.DownloadStatus, len(statuses))
	for _, st := range statuses {
		byID[string(st.ID)] = st
	}

	for _, d := range rows {
		st, ok := byID[d.ProviderDownloadID]
		if !ok {
			continue
		}
		newState := localState(st.State)
		if newState == d.State && st.Progress == d.Progress {
			continue
		}
		var completedAt *time.Time
		if newState == database.StateProviderCompleted {
			now := time.Now().UTC()
			completedAt = &now
		}
		if err := s.db.UpdateDownloadStatus(ctx, d.ID, newState, st.Progress, completedAt, ""); err != nil {
			slog.Error("sabnzbd: update download status failed", "id", d.ID, "error", err)
			continue
		}
		d.State = newState
		d.Progress = st.Progress
	}
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

// handleQueue implements mode=queue: everything not yet in a terminal state.
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
		if d.State != database.StateQueued && d.State != database.StateDownloading {
			continue
		}
		slots = append(slots, toQueueSlot(d))
	}
	writeJSON(w, map[string]any{"queue": map[string]any{"slots": slots}})
}

func toQueueSlot(d *database.Download) queueSlot {
	status := "Queued"
	if d.State == database.StateDownloading {
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
