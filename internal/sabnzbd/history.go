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
	// Bytes is real SABnzbd's own history size field — confirmed against
	// Sonarr's real source (SabnzbdHistoryItem/Sabnzbd.cs's GetHistory) that
	// it's directly read into the download item's own TotalSize, not just
	// cosmetic, unlike nzb_name/download_time (present on the real schema
	// too, but confirmed unused by Sonarr's parsing — not worth adding).
	// Missing entirely until an API-parity audit found it: every completed
	// or failed item was reporting size 0 in Sonarr/Radarr's Activity view.
	Bytes int64 `json:"bytes"`
}

// handleHistory implements mode=history: only downloads that are actually
// done — ready_for_import (files fetched to local disk by internal/importer)
// or error. provider_completed stays in the queue (see handleQueue) since
// the provider being done isn't the same as AcerviNode being done. Also
// handles name=delete (see handleDelete), same as handleQueue.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("name") == "delete" {
		s.handleDelete(w, r)
		return
	}

	ctx := r.Context()
	// Managed only, and this is the more important of the two listings: a
	// Manual row here reports "Failed", the one status Sonarr maps to
	// DownloadItemStatus.Failed — which blocklists a release and re-searches
	// over someone's personal download. See database.ListManagedDownloads.
	rows, err := s.db.ListManagedDownloads(ctx, database.KindUsenet)
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	// No refresh: this shim reads what the poll last wrote and never
	// reaches the provider itself. See the package comment.

	slots := make([]historySlot, 0, len(rows))
	for _, d := range rows {
		switch d.State {
		case database.StateReadyForImport:
			slots = append(slots, historySlot{
				NzoID: d.ID, Name: d.Name, Category: d.Category,
				Status: "Completed", Storage: d.SavePath, Bytes: d.SizeBytes,
			})
		case database.StateError:
			slots = append(slots, historySlot{
				NzoID: d.ID, Name: d.Name, Category: d.Category,
				Status: "Failed", FailMessage: d.ErrorMessage, Bytes: d.SizeBytes,
			})
		}
	}
	writeJSON(w, map[string]any{"history": map[string]any{"slots": slots}})
}
