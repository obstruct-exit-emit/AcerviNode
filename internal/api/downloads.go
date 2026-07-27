package api

import (
	"log/slog"
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// downloadResponse is the native API's own JSON shape for a download — a
// clean superset of what's needed by the UI, independent of either compat
// shim's protocol-specific vocabulary (qBittorrent/SABnzbd state strings
// never appear here; only AcerviNode's own state machine does).
type downloadResponse struct {
	ID           string  `json:"id"`
	Provider     string  `json:"provider"`
	Kind         string  `json:"kind"`
	Hash         string  `json:"hash,omitempty"`
	Name         string  `json:"name"`
	Category     string  `json:"category,omitempty"`
	SavePath     string  `json:"save_path,omitempty"`
	SizeBytes    int64   `json:"size_bytes"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	AddedAt      string  `json:"added_at"`
	UpdatedAt    string  `json:"updated_at"`
	CompletedAt  *string `json:"completed_at,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
}

type downloadFileResponse struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

func toDownloadResponse(d *database.Download) downloadResponse {
	var completedAt *string
	if d.CompletedAt != nil {
		s := d.CompletedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		completedAt = &s
	}
	return downloadResponse{
		ID:           d.ID,
		Provider:     d.Provider,
		Kind:         string(d.Kind),
		Hash:         d.Hash,
		Name:         d.Name,
		Category:     d.Category,
		SavePath:     d.SavePath,
		SizeBytes:    d.SizeBytes,
		State:        d.State,
		Progress:     d.Progress,
		AddedAt:      d.AddedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:    d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CompletedAt:  completedAt,
		ErrorMessage: d.ErrorMessage,
	}
}

// handleListDownloads implements GET /api/v1/downloads — every download,
// either kind, most recently added first.
func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListAllDownloads(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]downloadResponse, len(rows))
	for i, d := range rows {
		out[i] = toDownloadResponse(d)
	}
	writeJSON(w, out)
}

// handleGetDownload implements GET /api/v1/downloads/{id} — detail plus its
// file list.
func (s *Server) handleGetDownload(w http.ResponseWriter, r *http.Request) {
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	files, err := s.db.ListDownloadFiles(r.Context(), d.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	fileResp := make([]downloadFileResponse, len(files))
	for i, f := range files {
		fileResp[i] = downloadFileResponse{Path: f.Path, SizeBytes: f.SizeBytes}
	}
	writeJSON(w, struct {
		downloadResponse
		Files []downloadFileResponse `json:"files"`
	}{toDownloadResponse(d), fileResp})
}

// handleDeleteDownload implements DELETE /api/v1/downloads/{id}?deleteFiles=true.
// Mirrors internal/qbittorrent's delete: the provider call is best-effort —
// a failure there (e.g. the provider already forgot about it) doesn't stop
// the local row from being cleaned up, matching the behavior already proven
// against a real upstream error in Phase 1's live test.
func (s *Server) handleDeleteDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}

	var provider deleter
	switch d.Kind {
	case database.KindTorrent:
		provider = s.torrentProvider
	case database.KindUsenet:
		provider = s.usenetProvider
	}
	if provider != nil {
		deleteFiles := r.URL.Query().Get("deleteFiles") == "true"
		if err := provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
			slog.Error("api: provider delete failed", "id", d.ID, "error", err)
		}
	}

	if err := s.db.DeleteDownload(ctx, d.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) downloadByID(w http.ResponseWriter, r *http.Request) (*database.Download, bool) {
	id := r.PathValue("id")
	d, err := s.db.GetDownloadByID(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	if d == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	return d, true
}
