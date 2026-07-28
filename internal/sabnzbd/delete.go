package sabnzbd

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// handleDelete implements name=delete, layered onto both mode=queue and
// mode=history the same way SABnzbd's real API does — there's no separate
// delete mode, name=delete removes from whichever list you'd otherwise be
// querying. value is one or more comma-separated nzo_ids (SABnzbd's real API
// also accepts "all", but *arr apps always pass specific ids — see
// internal/qbittorrent's handleDelete for the same convention).
// del_files=1 also deletes the provider-side download, matching the
// qBittorrent shim's deleteFiles semantics.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deleteFiles := r.FormValue("del_files") == "1" || r.FormValue("del_files") == "true"

	for _, nzoID := range strings.Split(r.FormValue("value"), ",") {
		nzoID = strings.TrimSpace(nzoID)
		if nzoID == "" {
			continue
		}
		d, err := s.db.GetDownloadByID(ctx, nzoID)
		if err != nil || d == nil || d.Kind != database.KindUsenet {
			continue
		}
		if err := s.provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
			slog.Error("sabnzbd: provider delete failed", "nzo_id", nzoID, "error", err)
		}
		if err := s.db.DeleteDownload(ctx, d.ID); err != nil {
			slog.Error("sabnzbd: local delete failed", "nzo_id", nzoID, "error", err)
		}
	}
	writeJSON(w, map[string]any{"status": true})
}
