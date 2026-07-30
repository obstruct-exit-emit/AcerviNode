package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// downloadResponse is the native API's own JSON shape for a download — a
// clean superset of what's needed by the UI, independent of either compat
// shim's protocol-specific vocabulary (qBittorrent/SABnzbd state strings
// never appear here; only AcerviNode's own state machine does).
//
// Protocol is named that way externally (JSON, UI) because it reads better
// to API consumers than the internal Go domain type's name — see
// database.Kind, which stays as-is internally (matches the standard
// library's own reflect.Kind naming for "which variant of a thing this is",
// and avoids "type" clashing with the Go keyword throughout the backend).
type downloadResponse struct {
	ID           string  `json:"id"`
	Provider     string  `json:"provider"`
	Protocol     string  `json:"protocol"`
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
	// RetryCount/NextRetryAt reflect internal/importer's backoff — non-zero
	// only for a download that has failed at least once and is still being
	// retried (state stays provider_completed until either it succeeds or
	// hits the configured max, at which point it moves to error instead).
	RetryCount  int     `json:"retry_count,omitempty"`
	NextRetryAt *string `json:"next_retry_at,omitempty"`
	// HasSource reports whether there's something stored to resubmit this
	// download with if it's in error state — Source (never exposed
	// directly: the original magnet/NZB URL/hoster link) for a link-based
	// add, or a stored file for a usenet download added via an uploaded
	// .nzb (see database.Download.SourceFile). False for a discovered
	// download (adopted from the provider's own account with no original
	// link or file ever known) — see handleReAddDownload, which 400s when
	// neither is present. Not scoped to added_via=arr: Re-add works for any
	// kind/added_via as long as one of the two is stored (see the web UI's
	// error-state action buttons).
	HasSource bool `json:"has_source"`
	// AddedVia is "arr" (added through the qBittorrent/SABnzbd compat shim,
	// auto-fetched to local disk) or "manual" (added directly, or adopted
	// from the provider's own account — see internal/importer's discovery
	// step — never auto-fetched). What the web UI's Managed/Manual tabs
	// filter GET /api/v1/downloads?added_via=... on.
	AddedVia string `json:"added_via"`
}

type downloadFileResponse struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// ProviderFileID is what GET .../files/{fileId}/link needs to resolve a
	// direct download URL for this specific file (see handleGetFileLink).
	ProviderFileID string `json:"provider_file_id,omitempty"`
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

func toDownloadResponse(d *database.Download) downloadResponse {
	var completedAt *string
	if d.CompletedAt != nil {
		s := d.CompletedAt.UTC().Format(timeFormat)
		completedAt = &s
	}
	var nextRetryAt *string
	if d.NextRetryAt != nil {
		s := d.NextRetryAt.UTC().Format(timeFormat)
		nextRetryAt = &s
	}
	return downloadResponse{
		ID:           d.ID,
		Provider:     d.Provider,
		Protocol:     string(d.Kind),
		Hash:         d.Hash,
		Name:         d.Name,
		Category:     d.Category,
		SavePath:     d.SavePath,
		SizeBytes:    d.SizeBytes,
		State:        d.State,
		Progress:     d.Progress,
		AddedAt:      d.AddedAt.UTC().Format(timeFormat),
		UpdatedAt:    d.UpdatedAt.UTC().Format(timeFormat),
		CompletedAt:  completedAt,
		ErrorMessage: d.ErrorMessage,
		RetryCount:   d.RetryCount,
		NextRetryAt:  nextRetryAt,
		AddedVia:     string(d.AddedVia),
		HasSource:    d.Source != "" || d.SourceFileName != "",
	}
}

// handleListDownloads implements GET /api/v1/downloads — every download,
// either kind, most recently added first. An optional ?added_via=arr|manual
// filters to just the web UI's Managed or Manual tab; omitted or any other
// value returns everything, unfiltered.
func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListAllDownloads(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var addedVia database.AddedVia
	switch r.URL.Query().Get("added_via") {
	case string(database.AddedViaArr):
		addedVia = database.AddedViaArr
	case string(database.AddedViaManual):
		addedVia = database.AddedViaManual
	}
	// A member only ever sees Manual downloads, regardless of what the
	// query param asked for — the web UI never asks for anything else for
	// a member, but this is what actually enforces it server-side (see
	// docs/providers.md#roles).
	if role, _ := s.currentRole(r); role != RoleAdmin {
		addedVia = database.AddedViaManual
	}

	out := make([]downloadResponse, 0, len(rows))
	for _, d := range rows {
		if addedVia != "" && d.AddedVia != addedVia {
			continue
		}
		out = append(out, toDownloadResponse(d))
	}
	writeJSON(w, out)
}

// handleGetDownload implements GET /api/v1/downloads/{id} — detail plus its
// file list, queried live from the provider (the same as
// internal/qbittorrent's own handleFiles) rather than a local cache: a
// download's files aren't knowable until the provider has actually
// processed it, and there's nowhere in AcerviNode that persists them
// locally otherwise. A provider error here (e.g. still queued, nothing to
// list yet) isn't a hard failure — it just means an empty file list, not a
// broken download page — but the reason is still worth keeping: an empty
// list because nothing's processed yet and an empty list because the
// provider genuinely has no record of this download anymore (e.g. deleted
// directly through the provider's own site — a real, observed case for a
// Manual/discovered download, which nothing else ever detects since it's
// never in internal/importer's fetch-retry path) look identical to a caller
// unless the underlying error comes along too. See FilesError below.
func (s *Server) handleGetDownload(w http.ResponseWriter, r *http.Request) {
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	files, err := s.filesForDownload(r.Context(), d)
	var filesError string
	if err != nil {
		files = nil
		filesError = err.Error()
	}
	fileResp := make([]downloadFileResponse, len(files))
	for i, f := range files {
		fileResp[i] = downloadFileResponse{Path: f.Path, SizeBytes: f.SizeBytes, ProviderFileID: f.ProviderFileID}
	}
	writeJSON(w, struct {
		downloadResponse
		Files      []downloadFileResponse `json:"files"`
		FilesError string                 `json:"files_error,omitempty"`
	}{toDownloadResponse(d), fileResp, filesError})
}

// filesForDownload queries the provider for a download's current file list —
// shared by handleGetDownload and handleGetFileLink.
func (s *Server) filesForDownload(ctx context.Context, d *database.Download) ([]debrid.DownloadFile, error) {
	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	switch d.Kind {
	case database.KindTorrent:
		if s.torrentProvider == nil {
			return nil, debrid.ErrNoProvider
		}
		return s.torrentProvider.Files(ctx, id)
	case database.KindUsenet:
		if s.usenetProvider == nil {
			return nil, debrid.ErrNoProvider
		}
		return s.usenetProvider.Files(ctx, id)
	case database.KindWebDL:
		if s.webDownloadProvider == nil {
			return nil, debrid.ErrNoProvider
		}
		return s.webDownloadProvider.Files(ctx, id)
	default:
		return nil, fmt.Errorf("unknown download kind %q", d.Kind)
	}
}

// handleGetFileLink implements GET /api/v1/downloads/{id}/files/{fileId}/link
// — resolves a direct, provider-hosted download URL for one file, so it can
// be downloaded straight through the browser instead of (or in addition to)
// AcerviNode fetching it to local disk. The URL comes straight from the
// provider (see debrid.TorrentProvider/UsenetProvider.RequestDownloadLink,
// the same call internal/importer itself makes) — AcerviNode doesn't proxy
// or cache it.
func (s *Server) handleGetFileLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	fileID := r.PathValue("fileId")
	if fileID == "" {
		http.Error(w, "file id is required", http.StatusBadRequest)
		return
	}

	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	var (
		url string
		err error
	)
	switch d.Kind {
	case database.KindTorrent:
		if s.torrentProvider == nil {
			http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		url, err = s.torrentProvider.RequestDownloadLink(ctx, id, fileID)
	case database.KindUsenet:
		if s.usenetProvider == nil {
			http.Error(w, "no usenet-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		url, err = s.usenetProvider.RequestDownloadLink(ctx, id, fileID)
	case database.KindWebDL:
		if s.webDownloadProvider == nil {
			http.Error(w, "no web-download-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		url, err = s.webDownloadProvider.RequestDownloadLink(ctx, id, fileID)
	default:
		http.Error(w, "unknown download kind", http.StatusInternalServerError)
		return
	}
	if err != nil {
		writeProviderError(w, string(d.Kind), err)
		return
	}
	writeJSON(w, map[string]string{"url": url})
}

// handleGetZipLink implements GET /api/v1/downloads/{id}/zip-link — resolves
// one URL for every file in the download at once, zipped provider-side (see
// debrid.TorrentProvider/UsenetProvider.RequestZipDownloadLink). An explicit
// opt-in alternative to downloading files individually (see
// handleGetFileLink and the web UI's per-row "Download all" button, which
// downloads files individually by default) — some people want one archive,
// some don't.
func (s *Server) handleGetZipLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}

	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	var (
		url string
		err error
	)
	switch d.Kind {
	case database.KindTorrent:
		if s.torrentProvider == nil {
			http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		url, err = s.torrentProvider.RequestZipDownloadLink(ctx, id)
	case database.KindUsenet:
		if s.usenetProvider == nil {
			http.Error(w, "no usenet-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		url, err = s.usenetProvider.RequestZipDownloadLink(ctx, id)
	case database.KindWebDL:
		if s.webDownloadProvider == nil {
			http.Error(w, "no web-download-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		url, err = s.webDownloadProvider.RequestZipDownloadLink(ctx, id)
	default:
		http.Error(w, "unknown download kind", http.StatusInternalServerError)
		return
	}
	if err != nil {
		writeProviderError(w, string(d.Kind), err)
		return
	}
	writeJSON(w, map[string]string{"url": url})
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

	if provider := s.deleterForKind(d.Kind); provider != nil {
		deleteFiles := r.URL.Query().Get("deleteFiles") == "true"
		if err := provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
			slog.Error("api: provider delete failed", "id", d.ID, "error", err)
		}
	}

	// Tombstone before the local row is actually gone — a real, observed
	// race otherwise exists: the provider's own delete isn't always
	// instantly reflected in its listing endpoints, and internal/importer's
	// background discovery poll runs independently of this request, so a
	// tick landing in that window would see the still-technically-present
	// item with no local row anymore and adopt it fresh as a ghost Manual
	// download for something that was just intentionally deleted. See
	// database.RecordDeletedDownload.
	if err := s.db.RecordDeletedDownload(ctx, d.Provider, d.Kind, d.ProviderDownloadID); err != nil {
		slog.Error("api: record deleted-download tombstone failed", "id", d.ID, "error", err)
	}

	if err := s.db.DeleteDownload(ctx, d.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRetryDownload implements POST /api/v1/downloads/{id}/retry — the
// manual counterpart to internal/importer's automatic retry/backoff. Only
// valid for a download that has actually given up (StateError); anything
// else is rejected rather than silently reprocessed out of turn.
func (s *Server) handleRetryDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	if d.State != database.StateError {
		http.Error(w, "download is not in error state", http.StatusConflict)
		return
	}
	if err := s.db.RetryDownload(ctx, d.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	updated, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	writeJSON(w, toDownloadResponse(updated))
}

// downloadByID resolves the {id} path value, and is the single choke point
// every single-download handler (Get/Delete/Retry/Re-add/file-link/
// zip-link) routes through — which is why the member-role check lives here
// rather than duplicated in each of them: a member's access is scoped to
// Manual downloads only, never the *arr-driven Managed pipeline (see
// docs/providers.md#roles). requireAuth already guaranteed a valid identity
// before any handler here ran, so currentRole's ok is always true.
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
	if role, _ := s.currentRole(r); role != RoleAdmin && d.AddedVia != database.AddedViaManual {
		http.Error(w, "member access is limited to Manual downloads", http.StatusForbidden)
		return nil, false
	}
	return d, true
}
