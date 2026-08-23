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
		// Whether the provider actually removed its own copy decides the
		// tombstone's lifetime: a failed delete leaves the item on the
		// account, where discovery would re-adopt it as a ghost once a
		// short window lapsed — see database.RecordDeletedDownload.
		providerConfirmed := true
		if !s.ownsDownload(d) {
			providerConfirmed = false
		} else if err := s.provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
			providerConfirmed = false
			slog.Error("sabnzbd: provider delete failed", "nzo_id", nzoID, "error", err)
		}
		// The provider call above only ever removes the provider-side copy —
		// deleteFiles otherwise did nothing to local disk at all.
		if deleteFiles {
			if err := s.settings.DeleteLocalFiles(d); err != nil {
				slog.Warn("sabnzbd: delete local files failed", "nzo_id", nzoID, "error", err)
			}
		}
		// Tombstone before the row is gone — the provider's own delete isn't
		// always instantly reflected in its listing endpoints, and
		// internal/importer's background discovery poll runs independently
		// of this request. Without this, a Managed download an *arr app just
		// removed (e.g. a routine post-import cleanup step) could get
		// rediscovered on the very next tick as a brand-new Manual download,
		// since the provider's listing hadn't caught up with its own delete
		// yet and the local row protecting it from re-adoption is gone —
		// matches handleDeleteDownload's identical reasoning in internal/api.
		if err := s.db.RecordDeletedDownload(ctx, d.Provider, d.Kind, d.ProviderDownloadID, providerConfirmed); err != nil {
			slog.Error("sabnzbd: record deleted-download tombstone failed", "nzo_id", nzoID, "error", err)
		}
		if err := s.db.DeleteDownload(ctx, d.ID); err != nil {
			slog.Error("sabnzbd: local delete failed", "nzo_id", nzoID, "error", err)
		}
	}
	writeJSON(w, map[string]any{"status": true})
}
