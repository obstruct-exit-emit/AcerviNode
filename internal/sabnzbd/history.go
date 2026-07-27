package sabnzbd

import (
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
)

type historySlot struct {
	NzoID       string `json:"nzo_id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Status      string `json:"status"`
	Storage     string `json:"storage"`
	FailMessage string `json:"fail_message"`
}

// handleHistory implements mode=history: everything in a terminal state
// (completed or failed). AcerviNode's "ready_for_import" isn't reachable yet
// in this vertical slice (see ROADMAP.md) but is treated the same as
// provider_completed here for forward compatibility.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.ListDownloads(ctx, database.KindUsenet)
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	s.refreshFromProvider(ctx, rows)

	slots := make([]historySlot, 0, len(rows))
	for _, d := range rows {
		switch d.State {
		case database.StateProviderCompleted, database.StateReadyForImport:
			slots = append(slots, historySlot{
				NzoID: d.ID, Name: d.Name, Category: d.Category,
				Status: "Completed", Storage: d.SavePath,
			})
		case database.StateError:
			slots = append(slots, historySlot{
				NzoID: d.ID, Name: d.Name, Category: d.Category,
				Status: "Failed", FailMessage: d.ErrorMessage,
			})
		}
	}
	writeJSON(w, map[string]any{"history": map[string]any{"slots": slots}})
}
