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
		// Once internal/importer has moved a row to ready_for_import (files
		// actually on disk), the provider's own state is no longer
		// authoritative for it — TorBox still reporting "completed" must not
		// regress the row back to provider_completed.
		if d.State == database.StateReadyForImport {
			continue
		}

		st, ok := byID[d.ProviderDownloadID]
		if !ok {
			continue
		}
		newState := localState(st.State)
		if newState == d.State && st.Progress == d.Progress && st.SizeBytes == d.SizeBytes {
			continue
		}
		// completed_at is set once files are actually on disk
		// (internal/importer), not merely when the provider reports done —
		// so it isn't touched here.
		var completedAt *time.Time
		if err := s.db.UpdateDownloadStatus(ctx, d.ID, newState, st.Progress, st.SizeBytes, completedAt, ""); err != nil {
			slog.Error("sabnzbd: update download status failed", "id", d.ID, "error", err)
			continue
		}
		d.State = newState
		d.Progress = st.Progress
		d.SizeBytes = st.SizeBytes
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
