package sabnzbd

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// handleAddURL implements mode=addurl: *arr apps pass the NZB's URL directly
// (the "name" parameter, despite the misleading name — this is SABnzbd's
// actual field name for the URL in addurl mode) plus a category.
func (s *Server) handleAddURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nzbURL := r.FormValue("name")
	if nzbURL == "" {
		writeJSON(w, map[string]any{"status": false, "error": "no URL given"})
		return
	}
	category := r.FormValue("cat")
	displayName := r.FormValue("nzbname")

	id, err := s.provider.AddNZBURL(ctx, nzbURL, debrid.AddOptions{Name: displayName})
	if err != nil {
		slog.Error("sabnzbd: add nzb url failed", "error", err)
		writeJSON(w, map[string]any{"status": false, "error": err.Error()})
		return
	}

	nzoID, err := s.storeNewDownload(ctx, id, displayName, category, nzbURL)
	if err != nil {
		slog.Error("sabnzbd: store new download failed", "error", err)
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	s.categories.add(category)
	writeJSON(w, map[string]any{"status": true, "nzo_ids": []string{nzoID}})
}

// handleAddFile implements mode=addfile: a multipart upload where "name" is
// the NZB file part itself, matching SABnzbd's real (slightly confusing)
// field-naming convention.
func (s *Server) handleAddFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.MultipartForm == nil {
		writeJSON(w, map[string]any{"status": false, "error": "no file given"})
		return
	}
	headers := r.MultipartForm.File["name"]
	if len(headers) == 0 {
		writeJSON(w, map[string]any{"status": false, "error": "no file given"})
		return
	}
	header := headers[0]
	f, err := header.Open()
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "could not read file"})
		return
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "could not read file"})
		return
	}

	category := r.FormValue("cat")
	displayName := r.FormValue("nzbname")

	id, err := s.provider.AddNZBFile(ctx, header.Filename, data, debrid.AddOptions{Name: displayName})
	if err != nil {
		slog.Error("sabnzbd: add nzb file failed", "error", err)
		writeJSON(w, map[string]any{"status": false, "error": err.Error()})
		return
	}

	nzoID, err := s.storeNewDownload(ctx, id, displayName, category, "")
	if err != nil {
		slog.Error("sabnzbd: store new download failed", "error", err)
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	s.categories.add(category)
	writeJSON(w, map[string]any{"status": true, "nzo_ids": []string{nzoID}})
}

// storeNewDownload fetches the provider's own view of a just-added download
// and records it under a fresh AcerviNode-assigned id, which doubles as the
// nzo_id handed back to the *arr app — SABnzbd's real nzo_id has no fixed
// format, so there's nothing to preserve from the provider side (contrast
// with the qBittorrent shim, which must expose a real infohash).
func (s *Server) storeNewDownload(ctx context.Context, id debrid.ProviderDownloadID, fallbackName, category, source string) (nzoID string, err error) {
	status, statusErr := s.provider.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("sabnzbd: provider status not yet available after add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: fallbackName, State: debrid.StateQueued}
	}
	if status.Name == "" {
		status.Name = fallbackName
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           s.provider.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindUsenet,
		Name:               status.Name,
		Category:           category,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		Progress:           status.Progress,
		// Source is the NZB URL itself for a URL-based add, empty for a
		// .nzb file upload — see database.Download.Source and ReAddDownload.
		Source: source,
	}
	if err := s.db.InsertDownload(ctx, d); err != nil {
		return "", err
	}
	return d.ID, nil
}
