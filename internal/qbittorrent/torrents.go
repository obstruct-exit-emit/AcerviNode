package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
	"time"
)

// handleAdd implements POST /api/v2/torrents/add. qBittorrent's real
// endpoint accepts either newline-separated magnet/URL strings in a "urls"
// field, or one or more .torrent files in a "torrents" field — *arr apps use
// whichever their indexer gave them.
//
// Real qBittorrent's own request parser (confirmed against its source,
// src/base/http/requestparser.cpp) accepts a magnet-only add as a plain
// application/x-www-form-urlencoded POST, not just multipart/form-data —
// LibriNode sends exactly that. ParseMultipartForm always calls ParseForm
// first internally, which is all a urlencoded body needs (r.FormValue below
// works either way); it only returns http.ErrNotMultipart afterward because
// there's no file part to read, which isn't a real failure here — treating
// it as one rejected every magnet-only add with a 400 "Unsupported Media
// Type" no matter how correctly the client behaved, found live.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseMultipartForm(64 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		writeText(w, http.StatusBadRequest, "Unsupported Media Type")
		return
	}

	category := r.FormValue("category")
	savePath := r.FormValue("savepath")

	added := false

	for _, line := range strings.Split(r.FormValue("urls"), "\n") {
		magnet := strings.TrimSpace(line)
		if magnet == "" {
			continue
		}
		if err := s.addMagnet(ctx, magnet, category, savePath); err != nil {
			slog.Error("qbittorrent: add magnet failed", "error", err)
			continue
		}
		added = true
	}

	if r.MultipartForm != nil {
		for _, header := range r.MultipartForm.File["torrents"] {
			data, err := readFormFile(header)
			if err != nil {
				slog.Error("qbittorrent: read uploaded torrent failed", "error", err)
				continue
			}
			if err := s.addTorrentFile(ctx, header.Filename, data, category, savePath); err != nil {
				slog.Error("qbittorrent: add torrent file failed", "error", err)
				continue
			}
			added = true
		}
	}

	if !added {
		writeText(w, http.StatusBadRequest, "Fails.")
		return
	}
	writeText(w, http.StatusOK, "Ok.")
}

func readFormFile(header *multipart.FileHeader) ([]byte, error) {
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (s *Server) addMagnet(ctx context.Context, magnet, category, savePath string) error {
	id, err := s.provider.AddMagnet(ctx, magnet, debrid.AddOptions{Name: magnetDisplayName(magnet)})
	if err != nil {
		return err
	}
	return s.storeNewDownload(ctx, id, magnet, category, savePath)
}

func (s *Server) addTorrentFile(ctx context.Context, filename string, data []byte, category, savePath string) error {
	id, err := s.provider.AddTorrentFile(ctx, filename, data, debrid.AddOptions{Name: filename})
	if err != nil {
		return err
	}
	return s.storeNewDownload(ctx, id, "", category, savePath)
}

// storeNewDownload fetches the provider's own view of a just-added download
// (for its hash and name) and records it locally. If the provider hasn't
// reflected the add yet, a magnet-derived fallback keeps the add from
// failing outright — *arr apps will see the row on their next /info poll
// either way.
func (s *Server) storeNewDownload(ctx context.Context, id debrid.ProviderDownloadID, magnet, category, savePath string) error {
	status, err := s.provider.Status(ctx, id)
	if err != nil {
		slog.Warn("qbittorrent: provider status not yet available after add, using fallback", "id", id, "error", err)
		status = debrid.DownloadStatus{
			ID:    id,
			Name:  magnetDisplayName(magnet),
			Hash:  magnetHash(magnet),
			State: debrid.StateQueued,
		}
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           s.provider.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindTorrent,
		Hash:               strings.ToLower(status.Hash),
		Name:               status.Name,
		Category:           category,
		SavePath:           savePath,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		Progress:           status.Progress,
		// Source is the magnet itself for a magnet-based add, empty for a
		// .torrent file upload (nothing to resubmit without keeping the raw
		// bytes) — see database.Download.Source and ReAddDownload.
		Source: magnet,
		// AddedViaArr, not AddedViaManual: this shim only exists for *arr
		// apps, which need the files to land on local disk for their own
		// import step — see database.AddedVia.
		AddedVia: database.AddedViaArr,
	}
	if d.Name == "" {
		d.Name = d.Hash
	}
	// Not a plain InsertDownload: a row for this provider id may already
	// exist (TorBox dedupes by content, and the importer's discovery pass
	// can adopt a just-added item first), in which case that row is claimed
	// for *arr rather than colliding with it — see InsertOrClaimForArr.
	_, err = s.db.InsertOrClaimForArr(ctx, d)
	return err
}

// handleInfo implements GET /api/v2/torrents/info. It refreshes every
// tracked torrent from the provider in one bulk call, persists whatever
// changed, then reports current state — this is what makes repeated polling
// by an *arr app actually observe progress.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.ListDownloads(ctx, database.KindTorrent)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	liveByProviderID := s.refreshFromProvider(ctx, rows)

	wantHashes := splitFilter(r.URL.Query().Get("hashes"))
	wantCategory := r.URL.Query().Get("category")

	items := make([]torrentInfo, 0, len(rows))
	for _, d := range rows {
		if len(wantHashes) > 0 && !wantHashes[d.Hash] {
			continue
		}
		if wantCategory != "" && d.Category != wantCategory {
			continue
		}
		fetchProgress, hasFetchProgress := s.db.FetchProgress(d.ID)
		items = append(items, toTorrentInfo(d, liveByProviderID[d.ProviderDownloadID], fetchProgress, hasFetchProgress))
	}

	writeJSON(w, items)
}

// liveTorrentInfo is the fast-moving, purely informational subset of a
// provider's status that's never persisted to the database — read fresh on
// every poll and attached to the response by toTorrentInfo, the same
// treatment ETA always had, now shared with real qBittorrent's own swarm
// visibility (num_seeds/num_leechs/dlspeed) once that was found to be
// missing entirely (see debrid.DownloadStatus.Seeders's own doc comment).
type liveTorrentInfo struct {
	ETASeconds         int64
	Seeders            int64
	Leechers           int64
	DownloadSpeedBytes int64
}

// listCachedProvider is the optional half of this shim's provider,
// implemented by debrid's Dynamic*Provider wrapper — the same pointer
// internal/importer holds. Going through it means one provider listing per
// interval serves the importer and every connected *arr app at once,
// instead of this handler fetching its own copy on every request. Optional
// so a plain provider (this package's test fake) still works, fetching
// directly as before.
type listCachedProvider interface {
	ListCached(ctx context.Context) ([]debrid.DownloadStatus, time.Time, error)
}

// refreshFromProvider syncs every row's local state against one provider
// List() call — a single bulk request rather than one Status() call per row.
// See database.RefreshFromProvider, which this and internal/importer's own
// proactive background refresh both share, so an *arr app polling here still
// gets the freshest possible view even between importer ticks. Also returns
// each row's current liveTorrentInfo keyed by provider download ID.
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) map[string]liveTorrentInfo {
	var statuses []debrid.DownloadStatus
	var fetchedAt time.Time
	var err error
	if lc, ok := s.provider.(listCachedProvider); ok {
		statuses, fetchedAt, err = lc.ListCached(ctx)
	} else {
		fetchedAt = time.Now()
		statuses, err = s.provider.List(ctx)
	}
	if err != nil {
		slog.Error("qbittorrent: provider list failed", "error", err)
		return nil
	}
	s.db.RefreshFromProvider(ctx, rows, statuses, fetchedAt)

	live := make(map[string]liveTorrentInfo, len(statuses))
	for _, st := range statuses {
		live[string(st.ID)] = liveTorrentInfo{
			ETASeconds:         st.ETASeconds,
			Seeders:            st.Seeders,
			Leechers:           st.Leechers,
			DownloadSpeedBytes: st.DownloadSpeedBytes,
		}
	}
	return live
}

// handleProperties implements GET /api/v2/torrents/properties?hash=...
func (s *Server) handleProperties(w http.ResponseWriter, r *http.Request) {
	d, ok := s.downloadByHash(w, r)
	if !ok {
		return
	}
	writeJSON(w, torrentProperties{
		SavePath:  d.SavePath,
		Name:      d.Name,
		TotalSize: d.SizeBytes,
	})
}

// ownsDownload reports whether d belongs to this shim's provider. A shim
// only ever has one, so unlike the native API there is nothing to look up —
// but a row can still name a different provider, either because more than
// one is configured or because the API key was swapped for a different
// account after the row was created. Its provider_download_id means nothing
// to whoever is configured now, so acting on it would at best fail and at
// worst hit an unrelated download that happens to share the id.
func (s *Server) ownsDownload(d *database.Download) bool {
	if d.Provider == "" || d.Provider == s.provider.Name() {
		return true
	}
	slog.Warn("qbittorrent: skipping provider call, download belongs to a different provider",
		"id", d.ID, "download_provider", d.Provider, "configured_provider", s.provider.Name())
	return false
}

// handleFiles implements GET /api/v2/torrents/files?hash=...
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	d, ok := s.downloadByHash(w, r)
	if !ok {
		return
	}
	if !s.ownsDownload(d) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	files, err := s.provider.Files(r.Context(), debrid.ProviderDownloadID(d.ProviderDownloadID))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]torrentFileInfo, len(files))
	for i, f := range files {
		out[i] = torrentFileInfo{Index: i, Name: f.Path, Size: f.SizeBytes, Progress: 1, Priority: 1}
	}
	writeJSON(w, out)
}

// handleDelete implements POST /api/v2/torrents/delete.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "")
		return
	}
	deleteFiles := r.FormValue("deleteFiles") == "true"

	// Note: unlike the info-filter helper below, "all" isn't special-cased
	// here — *arr apps always pass specific hashes when deleting.
	for _, hash := range strings.Split(r.FormValue("hashes"), "|") {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if hash == "" {
			continue
		}
		d, err := s.db.GetDownloadByHash(ctx, hash)
		if err != nil || d == nil {
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
			slog.Error("qbittorrent: provider delete failed", "hash", hash, "error", err)
		}
		// The provider call above only ever removes the provider-side copy —
		// deleteFiles otherwise did nothing to local disk at all.
		if deleteFiles {
			if err := s.settings.DeleteLocalFiles(d); err != nil {
				slog.Warn("qbittorrent: delete local files failed", "hash", hash, "error", err)
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
			slog.Error("qbittorrent: record deleted-download tombstone failed", "hash", hash, "error", err)
		}
		if err := s.db.DeleteDownload(ctx, d.ID); err != nil {
			slog.Error("qbittorrent: local delete failed", "hash", hash, "error", err)
		}
	}
	writeText(w, http.StatusOK, "Ok.")
}

func (s *Server) downloadByHash(w http.ResponseWriter, r *http.Request) (*database.Download, bool) {
	hash := r.URL.Query().Get("hash")
	d, err := s.db.GetDownloadByHash(r.Context(), hash)
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

// --- response shapes --------------------------------------------------------

type torrentInfo struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	Category     string  `json:"category"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	State        string  `json:"state"`
	Eta          int64   `json:"eta"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
	// NumSeeds/NumLeechs/DlSpeed are real qBittorrent field names (Web API
	// v2's own documented convention) — swarm visibility that was simply
	// never being passed through anywhere before, found live while
	// watching a real, genuinely uncached torrent download. 0 for a
	// download whose provider doesn't report this (or hasn't yet).
	NumSeeds  int64 `json:"num_seeds"`
	NumLeechs int64 `json:"num_leechs"`
	DlSpeed   int64 `json:"dlspeed"`
	// Ratio/RatioLimit are always 0 — AcerviNode never actually seeds a
	// torrent locally (TorBox handles that server-side), so there's no
	// real ratio to report. Sent as an explicit, deliberate 0/0 rather than
	// omitted: Sonarr/Radarr's own HasReachedSeedLimit check (confirmed
	// against their real source) treats ratio_limit >= 0 && ratio_limit -
	// ratio <= 0.001 as "done seeding" — 0/0 satisfies that unconditionally,
	// which is the semantically honest answer here (AcerviNode is always
	// "done seeding," having never started) and is what actually lets
	// Sonarr/Radarr hardlink/clean up a completed torrent instead of always
	// falling back to copy-only — see qbtState's own doc comment for the
	// other half of this (the reported state string itself).
	Ratio      float64 `json:"ratio"`
	RatioLimit float64 `json:"ratio_limit"`
}

type torrentProperties struct {
	SavePath  string `json:"save_path"`
	Name      string `json:"name"`
	TotalSize int64  `json:"total_size"`
}

type torrentFileInfo struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
}

// toTorrentInfo splits d.SavePath into real qBittorrent's two distinct
// fields, rather than reporting it as save_path alone (the only field this
// response had until this was found live). Real qBittorrent's save_path is
// the shared per-category base directory; content_path is one torrent's own
// content root beneath it. Sonarr/Radarr's own source (QBittorrent.cs's
// GetItems, confirmed directly) only ever sets a completed download's
// OutputPath from content_path — and first checks content_path != save_path
// as a sanity guard, warning instead of importing when they match, since for
// a real qBittorrent that only happens when something's misconfigured.
// AcerviNode's own d.SavePath is already the per-download content root (see
// internal/importer.resolveDestDir) — i.e. exactly what real qBittorrent
// calls content_path — so that's reported as content_path here, with
// save_path synthesized as its parent directory purely so the two are never
// equal. Before this, content_path was never sent at all: Sonarr's own
// ContentPath property simply decoded to null, which isn't equal to
// save_path either, so GetItems took the "use content_path" branch
// anyway — using an empty path no completed Managed torrent could ever
// actually import from.
func toTorrentInfo(d *database.Download, live liveTorrentInfo, fetchProgress float64, hasFetchProgress bool) torrentInfo {
	completionOn := int64(-1)
	if d.CompletedAt != nil {
		completionOn = d.CompletedAt.Unix()
	}
	savePath := d.SavePath
	if d.SavePath != "" {
		savePath = filepath.Dir(d.SavePath)
	}
	return torrentInfo{
		Hash:        d.Hash,
		Name:        d.Name,
		Category:    d.Category,
		SavePath:    savePath,
		ContentPath: d.SavePath,
		Size:        d.SizeBytes,
		// EffectiveProgress substitutes internal/importer's own live local-
		// transfer progress in for d.Progress (already 1.0 by this point)
		// while the download is provider_completed — see its own doc
		// comment. Without this, an *arr app polling this field during
		// "downloading" (this shim's own reported state for
		// provider_completed — see qbtState below) would see progress
		// frozen at 100% for however long the actual local copy takes.
		Progress:     database.EffectiveProgress(d, fetchProgress, hasFetchProgress),
		State:        qbtState(d.State),
		Eta:          live.ETASeconds,
		AddedOn:      d.AddedAt.Unix(),
		CompletionOn: completionOn,
		NumSeeds:     live.Seeders,
		NumLeechs:    live.Leechers,
		DlSpeed:      live.DownloadSpeedBytes,
		Ratio:        0,
		RatioLimit:   0,
	}
}

// qbtState translates AcerviNode's local state machine to the qBittorrent
// state vocabulary *arr apps pattern-match on. See docs/qbittorrent-api.md.
//
// provider_completed deliberately still reports as "downloading" — the
// provider is done, but internal/importer hasn't fetched the files to local
// disk yet, and Sonarr's import step would find nothing if told otherwise.
//
// ready_for_import reports "pausedUP", not "uploading" — AcerviNode never
// actually seeds a torrent locally at all (TorBox handles that
// server-side), so "uploading" was never really true; "paused after
// finishing" is the honest state. It matters beyond cosmetics: confirmed
// against Sonarr/Radarr's real source, only "pausedUP"/"stoppedUP" (never
// "uploading") lets CanMoveFiles/CanBeRemoved become true — the two
// conditions gating whether Sonarr/Radarr will actually hardlink/move a
// completed torrent's files instead of always falling back to copy-only
// (silently doubling disk usage on every single torrent import, found live
// investigating a real Radarr "Access ... is denied" NZB bug — see
// docs/providers.md#directory-permissions) and whether it'll call this
// shim's own delete endpoint to clean up afterward once import succeeds
// (gated on top by the user's own "Remove completed downloads" setting in
// their qBittorrent client config — nothing AcerviNode controls). Both
// still require HasReachedSeedLimit too — see torrentInfo's own Ratio/
// RatioLimit fields for how that's satisfied unconditionally.
func qbtState(local string) string {
	switch local {
	case database.StateQueued:
		return "queuedDL"
	case database.StateDownloading, database.StateProviderCompleted:
		return "downloading"
	case database.StateReadyForImport:
		return "pausedUP"
	case database.StateError:
		return "error"
	default:
		return "unknown"
	}
}

// magnetHash extracts the infohash from a magnet URI's xt=urn:btih:HASH
// parameter, lowercased to match qBittorrent's own hash formatting.
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

func splitFilter(raw string) map[string]bool {
	out := map[string]bool{}
	if raw == "" || raw == "all" {
		return out
	}
	for _, h := range strings.Split(raw, "|") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			out[h] = true
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
