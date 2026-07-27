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

// handleHistory implements mode=history: only downloads that are actually
// done — ready_for_import (files fetched to local disk by internal/importer)
// or error. provider_completed stays in the queue (see handleQueue) since
// the provider being done isn't the same as AcerviNode being done.
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
		case database.StateReadyForImport:
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
