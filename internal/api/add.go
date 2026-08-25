package api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
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

// resolveAddedVia determines whether a new download from one of this file's
// add endpoints should be tracked as Managed (AddedViaArr) or Manual
// (AddedViaManual, the default) — an admin-only choice, requested via the
// added_via form field ("arr", or "manual"/omitted for the existing
// behavior). Rejected outright for a non-admin requesting "arr" rather than
// silently downgrading to Manual: the web UI never sends this for a member
// (the option isn't shown at all — see docs/providers.md#roles), so this
// only ever fires for direct API misuse, and an explicit error is clearer
// than silently doing something different from what was asked. Once
// inserted as Managed, a download behaves exactly like one Sonarr/Radarr
// added — auto-fetched to download_dir/a category override by
// internal/importer, and admin-only from then on (see downloadByID/
// handleListDownloads' own added_via scoping).
func (s *Server) resolveAddedVia(r *http.Request) (database.AddedVia, error) {
	if r.FormValue("added_via") != string(database.AddedViaArr) {
		return database.AddedViaManual, nil
	}
	if role, _ := s.currentRole(r); role != RoleAdmin {
		return "", fmt.Errorf("admin access required to add a Managed download")
	}
	return database.AddedViaArr, nil
}

// resolveAddProvider determines which provider a new download goes to: the
// optional "provider" form field if given, otherwise the configured default
// (see config.Config.DefaultProvider). Deliberately not admin-gated, unlike
// added_via — choosing between providers already configured on this
// instance grants nothing a member couldn't otherwise reach.
//
// A name that isn't registered is rejected rather than quietly falling back
// to the default: the caller asked for a specific account, and silently
// using a different one would put the download somewhere they didn't
// choose. Both compat shims have no equivalent, since neither the
// qBittorrent nor the SABnzbd protocol has a field to carry it — they
// always use the default.
func (s *Server) resolveAddProvider(r *http.Request, kind database.Kind) (string, error) {
	name := r.FormValue("provider")
	if name == "" {
		// Per kind, not the bare default: the default provider is one
		// setting across every kind, and it may not support this one.
		name = s.registry.DefaultNameFor(debrid.Kind(kind))
	}
	if s.providerNamed(name, kind) == nil {
		return "", fmt.Errorf("no %s-capable provider named %q is configured", kind, name)
	}
	return name, nil
}

// providerNamed resolves one provider's handling of one kind, or nil if
// that provider isn't registered or doesn't support the kind. Careful to
// return a genuinely nil interface rather than one holding a nil pointer.
func (s *Server) providerNamed(name string, kind database.Kind) downloadProvider {
	switch kind {
	case database.KindTorrent:
		if p := s.registry.Torrent(name); p != nil {
			return p
		}
	case database.KindUsenet:
		if p := s.registry.Usenet(name); p != nil {
			return p
		}
	case database.KindWebDL:
		if p := s.registry.WebDL(name); p != nil {
			return p
		}
	}
	return nil
}

// handleAddTorrent implements POST /api/v1/downloads/torrent — adds a
// torrent directly (a magnet link or an uploaded .torrent file), without
// needing to go through an *arr app or fake being one against the
// qBittorrent shim. See docs/api.md.
func (s *Server) handleAddTorrent(w http.ResponseWriter, r *http.Request) {
	if len(s.registry.TorrentNames()) == 0 {
		http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	addedVia, err := s.resolveAddedVia(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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

	providerName, err := s.resolveAddProvider(r, database.KindTorrent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := s.registry.Torrent(providerName)

	var (
		id           debrid.ProviderDownloadID
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
		id, err = p.AddTorrentFile(ctx, header.Filename, data, debrid.AddOptions{Name: header.Filename})
	} else {
		fallbackName = magnetDisplayName(magnet)
		fallbackHash = magnetHash(magnet)
		id, err = p.AddMagnet(ctx, magnet, debrid.AddOptions{Name: fallbackName})
	}
	if err != nil {
		writeProviderError(w, "torrent", err)
		return
	}

	status, statusErr := p.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("api: provider status not yet available after torrent add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: fallbackName, Hash: fallbackHash, State: debrid.StateQueued}
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           providerName,
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
		// Manual by default (this endpoint is only ever hit directly — the
		// web UI's own "+ Add" form, an *arr app has no way to reach it, it
		// only knows the qBittorrent/SABnzbd shims), but an admin can
		// explicitly request Managed instead — see resolveAddedVia.
		AddedVia: addedVia,
	}
	if d.Name == "" {
		d.Name = d.Hash
	}
	d, existed, err := s.existingOrInsert(ctx, providerName, string(id), d)
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
	if len(s.registry.UsenetNames()) == 0 {
		http.Error(w, "no usenet-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	addedVia, err := s.resolveAddedVia(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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

	providerName, err := s.resolveAddProvider(r, database.KindUsenet)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := s.registry.Usenet(providerName)

	var (
		id             debrid.ProviderDownloadID
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
		id, err = p.AddNZBFile(ctx, header.Filename, data, debrid.AddOptions{Name: header.Filename})
	} else {
		fallbackName = nzbURL
		id, err = p.AddNZBURL(ctx, nzbURL, debrid.AddOptions{Name: fallbackName})
	}
	if err != nil {
		writeProviderError(w, "usenet", err)
		return
	}

	status, statusErr := p.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("api: provider status not yet available after usenet add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: fallbackName, State: debrid.StateQueued}
	}
	if status.Name == "" {
		status.Name = fallbackName
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           providerName,
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
		// Manual by default, or Managed if an admin explicitly requested it
		// — see resolveAddedVia.
		AddedVia: addedVia,
	}
	d, existed, err := s.existingOrInsert(ctx, providerName, string(id), d)
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
// addWebLink adds link through providerName, falling back to any other
// configured web-download provider when the chosen one doesn't handle that
// file host. Returns the provider that actually accepted it.
//
// Which hosts a service covers varies a lot, and on AllDebrid varies by
// plan — a trial account covers five, against TorBox's ~160 — so with
// several configured, a link one refuses is often one another takes. Before
// this, such an add simply failed against whichever provider routing picked,
// with a perfectly capable provider sitting unused beside it.
//
// Deliberately narrow. Only debrid.ErrHostNotSupported is retried: any other
// failure could mean the add partly landed, and re-sending it elsewhere
// would risk a second copy. An unsupported host is refused outright, so
// nothing was created and nothing is duplicated by trying again.
//
// Web downloads only, since a magnet or NZB isn't tied to a file host.
func (s *Server) addWebLink(ctx context.Context, providerName, link string, chosenByCaller bool) (string, debrid.ProviderDownloadID, error) {
	p := s.registry.WebDL(providerName)
	if p == nil {
		// Routing checked this a moment ago, so only a concurrent settings
		// change gets here — switching web downloads off for this provider,
		// or removing it outright. Narrow, but calling a method on the nil
		// wrapper would panic the request rather than answer it.
		return providerName, "", fmt.Errorf("provider %s no longer handles web downloads", providerName)
	}
	id, err := p.AddLink(ctx, link, debrid.AddOptions{})
	if err == nil || chosenByCaller || !errors.Is(err, debrid.ErrHostNotSupported) {
		return providerName, id, err
	}

	firstErr := err
	for _, name := range s.registry.WebDLNames() {
		if name == providerName {
			continue
		}
		alt := s.registry.WebDL(name)
		if alt == nil || !alt.Configured() {
			continue
		}
		altID, altErr := alt.AddLink(ctx, link, debrid.AddOptions{})
		if altErr == nil {
			slog.Info("api: web download host unsupported by the chosen provider, added through another",
				"link_host", linkHost(link), "chosen", providerName, "used", name)
			return name, altID, nil
		}
		if !errors.Is(altErr, debrid.ErrHostNotSupported) {
			// A real failure elsewhere is more informative than the
			// original "unsupported host", since that provider was willing
			// to try.
			return name, "", altErr
		}
	}
	// Nobody handles it. The first provider's message is the clearest
	// explanation of why.
	return providerName, "", firstErr
}

// linkHost is the host part of link, for logging. Best effort: a link that
// won't parse is not worth failing an add over.
func linkHost(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return u.Host
}

func (s *Server) handleAddWebDownload(w http.ResponseWriter, r *http.Request) {
	if len(s.registry.WebDLNames()) == 0 {
		http.Error(w, "no web-download-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	addedVia, err := s.resolveAddedVia(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	ctx := r.Context()
	category := r.FormValue("category")
	link := strings.TrimSpace(r.FormValue("link"))
	if link == "" {
		http.Error(w, "link is required", http.StatusBadRequest)
		return
	}

	providerName, err := s.resolveAddProvider(r, database.KindWebDL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Only a provider this endpoint chose may be reconsidered. A caller who
	// named one asked for that account specifically, and quietly using a
	// different one would put the download somewhere they didn't pick —
	// the same reasoning readd applies.
	chosenByCaller := r.FormValue("provider") != ""

	providerName, id, err := s.addWebLink(ctx, providerName, link, chosenByCaller)
	if err != nil {
		writeProviderError(w, "web download", err)
		return
	}
	p := s.registry.WebDL(providerName)

	status, statusErr := p.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("api: provider status not yet available after web download add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: link, State: debrid.StateQueued}
	}
	if status.Name == "" {
		status.Name = link
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           providerName,
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
		// Manual by default (no *arr-facing shim exists for this kind at
		// all — see database.KindWebDL — so this never happens on its own
		// the way a torrent/usenet Managed download can), or Managed if an
		// admin explicitly requested it — see resolveAddedVia.
		AddedVia: addedVia,
	}
	d, existed, err := s.existingOrInsert(ctx, providerName, string(id), d)
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

	// Re-add goes back to the account this download already belongs to, not
	// the default. Resubmitting elsewhere would silently migrate it to a
	// different provider — the row would keep its identity while its files
	// moved accounts, which is not what "retry this download" means.
	provName := d.Provider
	if provName == "" {
		provName = s.registry.DefaultNameFor(debrid.Kind(d.Kind))
	}

	var (
		newID debrid.ProviderDownloadID
		err   error
	)
	switch d.Kind {
	case database.KindTorrent:
		p := s.registry.Torrent(provName)
		if p == nil {
			http.Error(w, "no torrent-capable provider named "+provName+" is configured", http.StatusServiceUnavailable)
			return
		}
		newID, err = p.AddMagnet(ctx, d.Source, debrid.AddOptions{Name: d.Name})
	case database.KindUsenet:
		p := s.registry.Usenet(provName)
		if p == nil {
			http.Error(w, "no usenet-capable provider named "+provName+" is configured", http.StatusServiceUnavailable)
			return
		}
		if d.Source != "" {
			newID, err = p.AddNZBURL(ctx, d.Source, debrid.AddOptions{Name: d.Name})
		} else {
			var filename string
			var data []byte
			filename, data, err = s.db.GetSourceFile(ctx, d.ID)
			if err == nil {
				newID, err = p.AddNZBFile(ctx, filename, data, debrid.AddOptions{Name: d.Name})
			}
		}
	case database.KindWebDL:
		p := s.registry.WebDL(provName)
		if p == nil {
			http.Error(w, "no web-download-capable provider named "+provName+" is configured", http.StatusServiceUnavailable)
			return
		}
		newID, err = p.AddLink(ctx, d.Source, debrid.AddOptions{Name: d.Name})
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
	if provider, err := s.providerFor(d); err == nil {
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
	fetchProgress, hasFetchProgress := s.db.FetchProgress(updated.ID)
	writeJSON(w, toDownloadResponse(updated, live, fetchProgress, hasFetchProgress))
}

// --- Check cached & torrent info previews -----------------------------------

// checkCachedResponse is the shared response shape for every check-cached
// endpoint below.
type checkCachedResponse struct {
	Cached bool `json:"cached"`
}

// handleCheckCachedTorrent implements
// GET /api/v1/downloads/torrent/check-cached — reports whether a magnet is
// already cached on the provider's side, without adding it, so the "+ Add"
// form can show it before commit. See debrid.TorrentProvider.CheckCached.
func (s *Server) handleCheckCachedTorrent(w http.ResponseWriter, r *http.Request) {
	if s.defaultTorrent() == nil {
		http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	hash := magnetHash(strings.TrimSpace(r.URL.Query().Get("magnet")))
	if hash == "" {
		http.Error(w, "magnet is required and must include a valid btih hash", http.StatusBadRequest)
		return
	}
	result, err := s.defaultTorrent().CheckCached(r.Context(), []string{hash})
	if err != nil {
		writeProviderError(w, "torrent", err)
		return
	}
	writeJSON(w, checkCachedResponse{Cached: result[hash]})
}

// handleCheckCachedUsenet is handleCheckCachedTorrent's usenet counterpart —
// GET /api/v1/downloads/usenet/check-cached. The hash TorBox actually wants
// here isn't a torrent-style infohash — per its own docs, it's an MD5 of the
// NZB link itself (see md5Hex).
func (s *Server) handleCheckCachedUsenet(w http.ResponseWriter, r *http.Request) {
	if s.defaultUsenet() == nil {
		http.Error(w, "no usenet-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	nzbURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if nzbURL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	hash := md5Hex(nzbURL)
	result, err := s.defaultUsenet().CheckCached(r.Context(), []string{hash})
	if err != nil {
		writeProviderError(w, "usenet", err)
		return
	}
	writeJSON(w, checkCachedResponse{Cached: result[hash]})
}

// handleCheckCachedWebDownload is handleCheckCachedTorrent's Web Downloads
// counterpart — GET /api/v1/downloads/webdl/check-cached. Per TorBox's docs,
// the hash is an MD5 of the link itself.
func (s *Server) handleCheckCachedWebDownload(w http.ResponseWriter, r *http.Request) {
	if s.defaultWebDL() == nil {
		http.Error(w, "no web-download-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	link := strings.TrimSpace(r.URL.Query().Get("link"))
	if link == "" {
		http.Error(w, "link is required", http.StatusBadRequest)
		return
	}
	hash := md5Hex(link)
	result, err := s.defaultWebDL().CheckCached(r.Context(), []string{hash})
	if err != nil {
		writeProviderError(w, "web download", err)
		return
	}
	writeJSON(w, checkCachedResponse{Cached: result[hash]})
}

// torrentInfoResponse is GET /api/v1/downloads/torrent/info's response —
// Available mirrors handleGetAccountStatus's own "available: false" style:
// a provider that doesn't support previews, or a torrent TorBox couldn't
// find on the network within its own search window, is routine, not a hard
// error.
type torrentInfoResponse struct {
	Available bool                      `json:"available"`
	Error     string                    `json:"error,omitempty"`
	Name      string                    `json:"name,omitempty"`
	Hash      string                    `json:"hash,omitempty"`
	SizeBytes int64                     `json:"size_bytes,omitempty"`
	Seeds     int64                     `json:"seeds,omitempty"`
	Peers     int64                     `json:"peers,omitempty"`
	Files     []torrentInfoFileResponse `json:"files,omitempty"`
}

type torrentInfoFileResponse struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// handleTorrentInfo implements GET /api/v1/downloads/torrent/info —
// previews a torrent's metadata (name, size, file list, seeders/peers)
// straight from the BitTorrent network, by hash alone, before ever adding
// it.
func (s *Server) handleTorrentInfo(w http.ResponseWriter, r *http.Request) {
	if s.defaultTorrent() == nil {
		http.Error(w, "no torrent-capable provider configured", http.StatusServiceUnavailable)
		return
	}
	hash := strings.TrimSpace(r.URL.Query().Get("hash"))
	if hash == "" {
		hash = magnetHash(strings.TrimSpace(r.URL.Query().Get("magnet")))
	}
	if hash == "" {
		http.Error(w, "hash, or magnet with a valid btih hash, is required", http.StatusBadRequest)
		return
	}
	info, err := s.defaultTorrent().TorrentInfo(r.Context(), hash)
	if err != nil {
		writeJSON(w, torrentInfoResponse{Available: false, Error: err.Error()})
		return
	}
	files := make([]torrentInfoFileResponse, len(info.Files))
	for i, f := range info.Files {
		files[i] = torrentInfoFileResponse{Path: f.Path, SizeBytes: f.SizeBytes}
	}
	writeJSON(w, torrentInfoResponse{
		Available: true,
		Name:      info.Name,
		Hash:      info.Hash,
		SizeBytes: info.SizeBytes,
		Seeds:     info.Seeds,
		Peers:     info.Peers,
		Files:     files,
	})
}

// providerFor returns the provider d actually belongs to.
//
// Resolving by kind alone would be wrong with more than one provider
// configured: every download row records which provider it came from
// (database.Download.Provider), and a provider_download_id means nothing to
// a different account — the call would at best fail and at worst act on an
// unrelated download that happens to share the id. Looking the name up in
// the registry makes that structural: there is no way to reach the wrong
// provider, rather than a comparison that has to be remembered.
//
// A row with no provider recorded falls back to the default. Nothing writes
// an empty provider today, but older rows predate the column being
// populated, and the default is both the only sensible guess and what the
// pre-registry code did.
//
// The error wraps debrid.ErrNoProvider so callers map it exactly as they
// map an unconfigured provider — from the caller's side "this download's
// provider isn't available" and "no provider is configured" want the same
// answer.
func (s *Server) providerFor(d *database.Download) (downloadProvider, error) {
	name := d.Provider
	if name == "" {
		name = s.registry.DefaultNameFor(debrid.Kind(d.Kind))
	}

	switch d.Kind {
	case database.KindTorrent, database.KindUsenet, database.KindWebDL:
	default:
		return nil, fmt.Errorf("unknown download kind %q", d.Kind)
	}
	p := s.providerNamed(name, d.Kind)
	if p == nil {
		slog.Warn("api: no provider available for download",
			"id", d.ID, "download_provider", d.Provider, "resolved_name", name, "kind", d.Kind)
		return nil, fmt.Errorf("%w: %s", debrid.ErrNoProvider, name)
	}
	return p, nil
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
		// An explicit Managed add claims a Manual row rather than handing
		// back the existing one unchanged. Returning it as-is would answer
		// "added, and it's Manual" to a request that said Managed — the
		// same silent contradiction the compat shims used to produce before
		// InsertOrClaimForArr, which this delegates to so both paths agree.
		// Found live: adding an already-tracked magnet with added_via=arr
		// returned 200 and left the download Manual, so it was never
		// auto-fetched.
		if d.AddedVia == database.AddedViaArr && existing.AddedVia != database.AddedViaArr {
			claimed, err := s.db.InsertOrClaimForArr(ctx, d)
			if err != nil {
				return nil, false, err
			}
			return claimed, true, nil
		}
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
	fetchProgress, hasFetchProgress := s.db.FetchProgress(d.ID)
	writeJSON(w, toDownloadResponse(d, live, fetchProgress, hasFetchProgress))
}

// writeProviderError maps a provider Add* failure to an HTTP response —
// debrid.ErrNoProvider (no key configured yet) is a routine 503 and a
// provider rate limit is a 429, both distinct from every other provider
// failure (a real upstream error), reported as 502.
//
// The rate-limit case is reported as 429 rather than folded into the
// generic 502 because it's genuinely retryable and increasingly routine
// rather than exceptional: TorBox v9 (2026-07-01) set /createtorrent to
// 60/hour for *uncached* torrents (300/minute for cached ones), and since
// v8.4.1 rate limits are counted per API key across all its servers rather
// than per IP — so anything else sharing the same key draws from the same
// bucket. A 502 tells a caller "upstream is broken"; a 429 tells it
// "slow down and try again", which is what's actually true. The provider's
// own error detail is passed through either way (see torbox.APIError).
func writeProviderError(w http.ResponseWriter, kind string, err error) {
	if errors.Is(err, debrid.ErrNoProvider) {
		http.Error(w, fmt.Sprintf("no %s-capable provider configured", kind), http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, debrid.ErrRateLimited) {
		http.Error(w, "provider rate limit reached, try again later: "+err.Error(), http.StatusTooManyRequests)
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

// md5Hex computes an MD5 of s and returns its lowercase hex encoding — what
// TorBox's usenet/webdl checkcached endpoints expect in place of a real
// hash (see torbox.Client.CheckCachedUsenet/CheckCachedWebDownloads' own doc
// comments): an MD5 of the link itself.
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
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
