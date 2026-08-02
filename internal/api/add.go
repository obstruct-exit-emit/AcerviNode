package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// handleAddTorrent implements POST /api/v1/downloads/torrent — adds a
// torrent directly (a magnet link or an uploaded .torrent file), without
// needing to go through an *arr app or fake being one against the
// qBittorrent shim. See docs/api.md.
func (s *Server) handleAddTorrent(w http.ResponseWriter, r *http.Request) {
	if s.torrentProvider == nil {
		http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	category := r.FormValue("category")
	magnet := strings.TrimSpace(r.FormValue("magnet"))
	header := formFile(r, "file")

	if magnet == "" && header == nil {
		http.Error(w, "either magnet or file is required", http.StatusBadRequest)
		return
	}

	var (
		id           debrid.ProviderDownloadID
		err          error
		fallbackName string
		fallbackHash string
	)
	if header != nil {
		data, readErr := readFormFile(header)
		if readErr != nil {
			http.Error(w, "could not read uploaded file", http.StatusBadRequest)
			return
		}
		fallbackName = header.Filename
		id, err = s.torrentProvider.AddTorrentFile(ctx, header.Filename, data, debrid.AddOptions{Name: header.Filename})
	} else {
		fallbackName = magnetDisplayName(magnet)
		fallbackHash = magnetHash(magnet)
		id, err = s.torrentProvider.AddMagnet(ctx, magnet, debrid.AddOptions{Name: fallbackName})
	}
	if err != nil {
		writeProviderError(w, "torrent", err)
		return
	}

	status, statusErr := s.torrentProvider.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("api: provider status not yet available after torrent add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: fallbackName, Hash: fallbackHash, State: debrid.StateQueued}
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           s.torrentProvider.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindTorrent,
		Hash:               strings.ToLower(status.Hash),
		Name:               status.Name,
		Category:           category,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		Progress:           status.Progress,
		// Source is the magnet itself for a magnet-based add, empty for a
		// .torrent file upload — see database.Download.Source.
		Source: magnet,
		// AddedViaManual: this endpoint is only ever hit directly (the web
		// UI's own "+ Add" form) — an *arr app has no way to reach it, it
		// only knows the qBittorrent/SABnzbd shims — see database.AddedVia.
		AddedVia: database.AddedViaManual,
	}
	if d.Name == "" {
		d.Name = d.Hash
	}
	d, existed, err := s.existingOrInsert(ctx, s.torrentProvider.Name(), string(id), d)
	if err != nil {
		slog.Error("api: store new torrent failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeAddResponse(w, d, existed)
}

// handleAddUsenet implements POST /api/v1/downloads/usenet — adds an NZB
// directly (a URL or an uploaded .nzb file).
func (s *Server) handleAddUsenet(w http.ResponseWriter, r *http.Request) {
	if s.usenetProvider == nil {
		http.Error(w, "no usenet-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	category := r.FormValue("category")
	nzbURL := strings.TrimSpace(r.FormValue("url"))
	header := formFile(r, "file")

	if nzbURL == "" && header == nil {
		http.Error(w, "either url or file is required", http.StatusBadRequest)
		return
	}

	var (
		id             debrid.ProviderDownloadID
		err            error
		fallbackName   string
		uploadedFile   []byte
		uploadedFileNm string
	)
	if header != nil {
		data, readErr := readFormFile(header)
		if readErr != nil {
			http.Error(w, "could not read uploaded file", http.StatusBadRequest)
			return
		}
		fallbackName = header.Filename
		uploadedFile = data
		uploadedFileNm = header.Filename
		id, err = s.usenetProvider.AddNZBFile(ctx, header.Filename, data, debrid.AddOptions{Name: header.Filename})
	} else {
		fallbackName = nzbURL
		id, err = s.usenetProvider.AddNZBURL(ctx, nzbURL, debrid.AddOptions{Name: fallbackName})
	}
	if err != nil {
		writeProviderError(w, "usenet", err)
		return
	}

	status, statusErr := s.usenetProvider.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("api: provider status not yet available after usenet add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: fallbackName, State: debrid.StateQueued}
	}
	if status.Name == "" {
		status.Name = fallbackName
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           s.usenetProvider.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindUsenet,
		Name:               status.Name,
		Category:           category,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		Progress:           status.Progress,
		// Source is the NZB URL itself for a URL-based add, empty for a
		// .nzb file upload — see database.Download.Source. SourceFile is
		// the reverse: the raw uploaded bytes for a file-based add, empty
		// for a URL-based one — see database.Download.SourceFile. Together
		// these are what let Re-add work either way.
		Source:         nzbURL,
		SourceFile:     uploadedFile,
		SourceFileName: uploadedFileNm,
		// AddedViaManual: this endpoint is only ever hit directly (the web
		// UI's own "+ Add" form) — an *arr app has no way to reach it, it
		// only knows the qBittorrent/SABnzbd shims — see database.AddedVia.
		AddedVia: database.AddedViaManual,
	}
	d, existed, err := s.existingOrInsert(ctx, s.usenetProvider.Name(), string(id), d)
	if err != nil {
		slog.Error("api: store new usenet download failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeAddResponse(w, d, existed)
}

// handleAddWebDownload implements POST /api/v1/downloads/webdl — adds a
// hoster link directly (Mega, 1Fichier, Mediafire, and ~160 others TorBox's
// Web Downloads service supports — see docs/providers.md). Link-only, no
// file-upload variant: TorBox's own createwebdownload endpoint has none
// either, unlike the torrent/usenet add endpoints.
func (s *Server) handleAddWebDownload(w http.ResponseWriter, r *http.Request) {
	if s.webDownloadProvider == nil {
		http.Error(w, "no web-download-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	category := r.FormValue("category")
	link := strings.TrimSpace(r.FormValue("link"))
	if link == "" {
		http.Error(w, "link is required", http.StatusBadRequest)
		return
	}

	id, err := s.webDownloadProvider.AddLink(ctx, link, debrid.AddOptions{})
	if err != nil {
		writeProviderError(w, "web download", err)
		return
	}

	status, statusErr := s.webDownloadProvider.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("api: provider status not yet available after web download add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: link, State: debrid.StateQueued}
	}
	if status.Name == "" {
		status.Name = link
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           s.webDownloadProvider.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindWebDL,
		Hash:               strings.ToLower(status.Hash),
		Name:               status.Name,
		Category:           category,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		Progress:           status.Progress,
		// Source is the link itself — always present, since there's no
		// file-upload variant for this kind — so handleReAddDownload can
		// always resubmit it, unlike torrent/usenet where a file-uploaded
		// download has no Source to re-add from.
		Source: link,
		// AddedViaManual: no *arr-facing shim exists for this kind at all —
		// see database.KindWebDL.
		AddedVia: database.AddedViaManual,
	}
	d, existed, err := s.existingOrInsert(ctx, s.webDownloadProvider.Name(), string(id), d)
	if err != nil {
		slog.Error("api: store new web download failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeAddResponse(w, d, existed)
}

// handleReAddDownload implements POST /api/v1/downloads/{id}/retry's
// stronger sibling, POST /api/v1/downloads/{id}/readd — for when the
// original provider-side download itself is gone (e.g. expired from the
// provider's own list, as opposed to a transient fetch failure RetryDownload
// alone can recover from). Resubmits the download's stored Source (the
// original magnet/NZB URL/hoster link) to the provider as a brand new add,
// then points the local row at the new provider_download_id. For a usenet
// download with no Source (added via an uploaded .nzb file, not a URL),
// falls back to resubmitting the stored file bytes instead — see
// database.Download.SourceFile. Only valid for a download that's actually
// given up (StateError) and has *something* stored to resubmit — neither a
// torrent nor webdl file upload has a SourceFile fallback (a torrent
// already gets a resubmittable magnet reconstructed from just its hash —
// see torbox.magnetFromHash — and webdl has no file-upload path at all), so
// for those Source empty really does mean nothing can be done automatically.
func (s *Server) handleReAddDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	if d.State != database.StateError {
		http.Error(w, "download is not in error state", http.StatusConflict)
		return
	}
	if d.Source == "" && d.SourceFileName == "" {
		http.Error(w, "no original source stored for this download — cannot re-add automatically", http.StatusBadRequest)
		return
	}

	var (
		newID    debrid.ProviderDownloadID
		err      error
		provName string
	)
	switch d.Kind {
	case database.KindTorrent:
		if s.torrentProvider == nil {
			http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		provName = s.torrentProvider.Name()
		newID, err = s.torrentProvider.AddMagnet(ctx, d.Source, debrid.AddOptions{Name: d.Name})
	case database.KindUsenet:
		if s.usenetProvider == nil {
			http.Error(w, "no usenet-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		provName = s.usenetProvider.Name()
		if d.Source != "" {
			newID, err = s.usenetProvider.AddNZBURL(ctx, d.Source, debrid.AddOptions{Name: d.Name})
		} else {
			var filename string
			var data []byte
			filename, data, err = s.db.GetSourceFile(ctx, d.ID)
			if err == nil {
				newID, err = s.usenetProvider.AddNZBFile(ctx, filename, data, debrid.AddOptions{Name: d.Name})
			}
		}
	case database.KindWebDL:
		if s.webDownloadProvider == nil {
			http.Error(w, "no web-download-capable provider configured", http.StatusServiceUnavailable)
			return
		}
		provName = s.webDownloadProvider.Name()
		newID, err = s.webDownloadProvider.AddLink(ctx, d.Source, debrid.AddOptions{Name: d.Name})
	default:
		http.Error(w, "unknown download kind", http.StatusInternalServerError)
		return
	}
	if err != nil {
		writeProviderError(w, string(d.Kind), err)
		return
	}

	// Best-effort cleanup of the old, presumably-gone provider-side entry —
	// matches handleDeleteDownload's "provider call is best-effort" stance;
	// it's already lost to us either way.
	if provider := s.deleterForKind(d.Kind); provider != nil {
		if err := provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), false); err != nil {
			slog.Warn("api: best-effort delete of old provider download failed during re-add", "id", d.ID, "error", err)
		}
	}

	// A provider may dedupe by content and hand back an ID that's already
	// tracked under a different local row (see existingOrInsert) — for
	// re-add specifically, that's a conflict rather than something to
	// silently resolve, since it would mean corrupting one row's identity
	// into another's.
	if existing, err := s.db.GetDownloadByProviderID(ctx, provName, string(newID)); err == nil && existing != nil && existing.ID != d.ID {
		http.Error(w, "re-add resolved to an already-tracked download ("+existing.ID+")", http.StatusConflict)
		return
	}

	if err := s.db.ReAddDownload(ctx, d.ID, string(newID)); err != nil {
		slog.Error("api: re-add download failed", "id", d.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	updated, ok := s.downloadByID(w, r)
	if !ok {
		return
	}
	live, _ := s.db.LiveStatus(updated.ID)
	writeJSON(w, toDownloadResponse(updated, live))
}

// deleterForKind returns the deleter for a download's kind, mirroring
// handleDeleteDownload's own switch.
func (s *Server) deleterForKind(kind database.Kind) deleter {
	switch kind {
	case database.KindTorrent:
		return s.torrentProvider
	case database.KindUsenet:
		return s.usenetProvider
	case database.KindWebDL:
		return s.webDownloadProvider
	default:
		return nil
	}
}

// existingOrInsert returns an already-tracked download for provider+id if
// one exists, rather than inserting d — a provider may dedupe by content and
// hand back an ID that's already tracked (e.g. TorBox returning the same
// torrent_id for a magnet whose hash it already has cached under an earlier
// add), which would otherwise trip the (provider, provider_download_id)
// UNIQUE constraint. The bool reports whether the row already existed, so
// the caller can respond 200 instead of 201.
func (s *Server) existingOrInsert(ctx context.Context, providerName, providerDownloadID string, d *database.Download) (*database.Download, bool, error) {
	existing, err := s.db.GetDownloadByProviderID(ctx, providerName, providerDownloadID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
	}
	if err := s.db.InsertDownload(ctx, d); err != nil {
		return nil, false, err
	}
	return d, false, nil
}

func (s *Server) writeAddResponse(w http.ResponseWriter, d *database.Download, existed bool) {
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	live, _ := s.db.LiveStatus(d.ID)
	writeJSON(w, toDownloadResponse(d, live))
}

// writeProviderError maps a provider Add* failure to an HTTP response —
// debrid.ErrNoProvider (no key configured yet) is a routine 503, distinct
// from every other provider failure (a real upstream error), reported as 502.
func writeProviderError(w http.ResponseWriter, kind string, err error) {
	if errors.Is(err, debrid.ErrNoProvider) {
		http.Error(w, fmt.Sprintf("no %s-capable provider configured", kind), http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "provider error: "+err.Error(), http.StatusBadGateway)
}

func formFile(r *http.Request, field string) *multipart.FileHeader {
	if r.MultipartForm == nil || len(r.MultipartForm.File[field]) == 0 {
		return nil
	}
	return r.MultipartForm.File[field][0]
}

func readFormFile(header *multipart.FileHeader) ([]byte, error) {
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// magnetHash extracts the infohash from a magnet URI's xt=urn:btih:HASH
// parameter, lowercased. A small, self-contained duplicate of
// internal/qbittorrent's own helper — not shared across packages since
// internal/api shouldn't depend on a protocol-compat shim package.
func magnetHash(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	xt := u.Query().Get("xt")
	const prefix = "urn:btih:"
	if !strings.HasPrefix(xt, prefix) {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(xt, prefix))
}

// magnetDisplayName extracts the dn= (display name) parameter from a magnet
// URI, falling back to the hash if there isn't one.
func magnetDisplayName(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil {
		return magnet
	}
	if dn := u.Query().Get("dn"); dn != "" {
		return dn
	}
	return magnetHash(magnet)
}
