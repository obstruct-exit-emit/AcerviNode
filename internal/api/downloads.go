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
	ID          string  `json:"id"`
	Provider    string  `json:"provider"`
	Protocol    string  `json:"protocol"`
	Hash        string  `json:"hash,omitempty"`
	Name        string  `json:"name"`
	Category    string  `json:"category,omitempty"`
	SavePath    string  `json:"save_path,omitempty"`
	SizeBytes   int64   `json:"size_bytes"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`
	AddedAt     string  `json:"added_at"`
	UpdatedAt   string  `json:"updated_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
	// CachedAt is when the provider first reported this download done —
	// see database.Download.CachedAt. Distinct from CompletedAt: for a
	// Manual download that's never fetched to local disk, CachedAt is set
	// but CompletedAt stays nil forever.
	CachedAt     *string `json:"cached_at,omitempty"`
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

	// ETASeconds/Seeders/Leechers/DownloadSpeedBytes/Phase are fast-moving,
	// provider-reported fields that are deliberately never persisted to the
	// downloads table — read from database.DB's own in-memory LiveStatus
	// cache (populated as a side effect of whichever poller last refreshed
	// this download; see database.DB's own doc comment), not a live
	// provider call made by this handler itself. Zero-valued (and Phase
	// empty) whenever nothing's been polled yet, or for a provider/kind
	// with no such concept (Seeders/Leechers/DownloadSpeedBytes are
	// torrent-only; Phase is usenet-only) — indistinguishable in JSON from
	// a genuine zero, the same tradeoff both compat shims already accept
	// for their own equivalent fields.
	ETASeconds         int64  `json:"eta_seconds"`
	Seeders            int64  `json:"seeders"`
	Leechers           int64  `json:"leechers"`
	DownloadSpeedBytes int64  `json:"download_speed_bytes"`
	Phase              string `json:"phase,omitempty"`
	// Airlocked reports whether the provider is keeping this download in
	// permanent storage, exempt from the retention policy that would
	// otherwise eventually remove it (TorBox AirLock). Same never-persisted
	// treatment as the fields above, so it reads false until this download
	// has actually been polled once.
	Airlocked bool `json:"airlocked"`
}

type downloadFileResponse struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// ProviderFileID is what GET .../files/{fileId}/link needs to resolve a
	// direct download URL for this specific file (see handleGetFileLink).
	ProviderFileID string `json:"provider_file_id,omitempty"`
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

// toDownloadResponse builds the response shape for d. live is d's current
// database.LiveStatus — deliberately a parameter rather than something this
// function looks up itself, so it stays a pure, easily-testable mapping;
// see handleListDownloads/handleGetDownload and the other callers for
// where it's actually read from database.DB's own cache. Pass the zero
// database.LiveStatus for a download nothing's polled yet.
//
// fetchProgress/hasFetchProgress are database.DB.FetchProgress's own
// return values — see database.EffectiveProgress, which this delegates to
// for the reported Progress field: internal/importer's own live local-
// transfer progress substitutes for d.Progress while a Managed download is
// in the "Fetching" phase (StateProviderCompleted), instead of a stale
// 100% sitting there for however long the actual file copy takes.
func toDownloadResponse(d *database.Download, live database.LiveStatus, fetchProgress float64, hasFetchProgress bool) downloadResponse {
	var completedAt *string
	if d.CompletedAt != nil {
		s := d.CompletedAt.UTC().Format(timeFormat)
		completedAt = &s
	}
	var cachedAt *string
	if d.CachedAt != nil {
		s := d.CachedAt.UTC().Format(timeFormat)
		cachedAt = &s
	}
	var nextRetryAt *string
	if d.NextRetryAt != nil {
		s := d.NextRetryAt.UTC().Format(timeFormat)
		nextRetryAt = &s
	}
	return downloadResponse{
		ID:                 d.ID,
		Provider:           d.Provider,
		Protocol:           string(d.Kind),
		Hash:               d.Hash,
		Name:               d.Name,
		Category:           d.Category,
		SavePath:           d.SavePath,
		SizeBytes:          d.SizeBytes,
		State:              d.State,
		Progress:           database.EffectiveProgress(d, fetchProgress, hasFetchProgress),
		AddedAt:            d.AddedAt.UTC().Format(timeFormat),
		UpdatedAt:          d.UpdatedAt.UTC().Format(timeFormat),
		CompletedAt:        completedAt,
		CachedAt:           cachedAt,
		ErrorMessage:       d.ErrorMessage,
		RetryCount:         d.RetryCount,
		NextRetryAt:        nextRetryAt,
		AddedVia:           string(d.AddedVia),
		HasSource:          d.Source != "" || d.SourceFileName != "",
		ETASeconds:         live.ETASeconds,
		Seeders:            live.Seeders,
		Leechers:           live.Leechers,
		DownloadSpeedBytes: live.DownloadSpeedBytes,
		Phase:              live.Phase,
		Airlocked:          live.Airlocked,
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
		live, _ := s.db.LiveStatus(d.ID)
		fetchProgress, hasFetchProgress := s.db.FetchProgress(d.ID)
		out = append(out, toDownloadResponse(d, live, fetchProgress, hasFetchProgress))
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
	live, _ := s.db.LiveStatus(d.ID)
	fetchProgress, hasFetchProgress := s.db.FetchProgress(d.ID)
	writeJSON(w, struct {
		downloadResponse
		Files      []downloadFileResponse `json:"files"`
		FilesError string                 `json:"files_error,omitempty"`
	}{toDownloadResponse(d, live, fetchProgress, hasFetchProgress), fileResp, filesError})
}

// filesForDownload queries the provider for a download's current file list —
// shared by handleGetDownload and handleGetFileLink.
func (s *Server) filesForDownload(ctx context.Context, d *database.Download) ([]debrid.DownloadFile, error) {
	p, err := s.providerFor(d)
	if err != nil {
		return nil, err
	}
	return p.Files(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID))
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
	p, err := s.providerFor(d)
	if err != nil {
		writeProviderError(w, string(d.Kind), err)
		return
	}
	url, err := p.RequestDownloadLink(ctx, id, fileID)
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
	p, err := s.providerFor(d)
	if err != nil {
		writeProviderError(w, string(d.Kind), err)
		return
	}
	url, err = p.RequestZipDownloadLink(ctx, id)
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

	// Interrupt any in-flight fetch for this download before touching
	// anything else — internal/importer might be mid-write for it right
	// now (Completed Download Handling), and without this, its goroutine
	// has no way to know the row was just deleted: it would keep writing
	// (recreating whatever DeleteLocalFiles below just removed) and only
	// notice once its own final status update fails against the
	// already-gone row, well after the damage is done. Blocks briefly
	// until the fetch has genuinely stopped, not just been asked to — a
	// no-op if nothing's actively fetching this download right now. Closes
	// a theoretically real race identified by code inspection: without it,
	// deleting a download mid-fetch could leave an orphaned partial file on
	// disk despite the API/database correctly showing it gone.
	s.settings.CancelFetch(d.ID)

	deleteFiles := r.URL.Query().Get("deleteFiles") == "true"
	// Whether the provider actually removed its own copy decides how long
	// the tombstone below has to last. A failed delete leaves the item on
	// the account, so the short listing-lag window would let discovery
	// re-adopt it as a ghost the moment it lapsed — see
	// database.RecordDeletedDownload. True when there's no provider to call
	// at all: there's then no provider-side copy left to come back.
	providerConfirmed := true
	if provider, err := s.providerFor(d); err == nil {
		if err := provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
			providerConfirmed = false
			slog.Error("api: provider delete failed, tombstoning for longer so it can't come back as a ghost",
				"id", d.ID, "provider_id", d.ProviderDownloadID, "error", err)
		}
	}
	// The provider call above only ever removes the provider-side copy —
	// deleteFiles otherwise did nothing to local disk at all. Best-effort,
	// same tone as internal/importer's own retention/cleanup policy: a
	// failure here shouldn't block the row itself from being deleted.
	if deleteFiles {
		if err := s.settings.DeleteLocalFiles(d); err != nil {
			slog.Warn("api: delete local files failed", "id", d.ID, "error", err)
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
	if err := s.db.RecordDeletedDownload(ctx, d.Provider, d.Kind, d.ProviderDownloadID, providerConfirmed); err != nil {
		slog.Error("api: record deleted-download tombstone failed", "id", d.ID, "error", err)
	}

	if err := s.db.DeleteDownload(ctx, d.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retryableState reports whether a manual retry makes sense for a download
// in this state. The provider side has to be done: retry re-runs the local
// fetch, and there is nothing to fetch from a download the provider is still
// working on.
func retryableState(state string) bool {
	switch state {
	case database.StateError, database.StateReadyForImport, database.StateProviderCompleted:
		return true
	default:
		// queued, downloading — still in flight provider-side.
		return false
	}
}

// handleRetryDownload implements POST /api/v1/downloads/{id}/retry — the
// manual counterpart to internal/importer's automatic retry/backoff.
//
// Retrying means "run the local fetch again" (RetryDownload resets the row to
// provider_completed), so it is valid for any state where the provider side
// is already finished, and refused for one still in flight: forcing a queued
// or downloading row to provider_completed would have the importer fetch
// something the provider hasn't produced yet.
//
// Deliberately wider than StateError alone, which it used to require. A
// download can be wrong without having given up — a ready_for_import row
// whose files never actually landed is the case that motivated this, and
// under the old rule it answered 409 and left delete-and-re-add as the only
// way out. That particular cause is fixed (see internal/importer's
// zero-file guard), but "wrong, yet not in error" is a shape that will
// recur, and refusing to re-run a fetch is a poor answer to it.
func (s *Server) handleRetryDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	if !retryableState(d.State) {
		http.Error(w, fmt.Sprintf(
			"download is %s — retry re-runs the local fetch, so it only applies once the provider has finished",
			d.State), http.StatusConflict)
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
	live, _ := s.db.LiveStatus(updated.ID)
	fetchProgress, hasFetchProgress := s.db.FetchProgress(updated.ID)
	writeJSON(w, toDownloadResponse(updated, live, fetchProgress, hasFetchProgress))
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
