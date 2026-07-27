package qbittorrent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// handleAdd implements POST /api/v2/torrents/add. qBittorrent's real
// endpoint accepts either newline-separated magnet/URL strings in a "urls"
// field, or one or more .torrent files in a "torrents" field — *arr apps use
// whichever their indexer gave them.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseMultipartForm(64 << 20); err != nil {
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
		State:              localState(status.State),
		Progress:           status.Progress,
	}
	if d.Name == "" {
		d.Name = d.Hash
	}
	return s.db.InsertDownload(ctx, d)
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
	s.refreshFromProvider(ctx, rows)

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
		items = append(items, toTorrentInfo(d))
	}

	writeJSON(w, items)
}

// refreshFromProvider syncs every row's local state against one provider
// List() call — a single bulk request rather than one Status() call per row.
func (s *Server) refreshFromProvider(ctx context.Context, rows []*database.Download) {
	statuses, err := s.provider.List(ctx)
	if err != nil {
		slog.Error("qbittorrent: provider list failed", "error", err)
		return
	}
	byID := make(map[string]debrid.DownloadStatus, len(statuses))
	for _, st := range statuses {
		byID[string(st.ID)] = st
	}

	for _, d := range rows {
		st, ok := byID[d.ProviderDownloadID]
		if !ok {
			continue
		}
		newState := localState(st.State)
		if newState == d.State && st.Progress == d.Progress {
			continue
		}
		var completedAt *time.Time
		if newState == database.StateProviderCompleted {
			now := time.Now().UTC()
			completedAt = &now
		}
		if err := s.db.UpdateDownloadStatus(ctx, d.ID, newState, st.Progress, completedAt, ""); err != nil {
			slog.Error("qbittorrent: update download status failed", "id", d.ID, "error", err)
			continue
		}
		d.State = newState
		d.Progress = st.Progress
	}
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

// handleFiles implements GET /api/v2/torrents/files?hash=...
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	d, ok := s.downloadByHash(w, r)
	if !ok {
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
		if err := s.provider.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
			slog.Error("qbittorrent: provider delete failed", "hash", hash, "error", err)
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
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	State        string  `json:"state"`
	Eta          int64   `json:"eta"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
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

func toTorrentInfo(d *database.Download) torrentInfo {
	completionOn := int64(-1)
	if d.CompletedAt != nil {
		completionOn = d.CompletedAt.Unix()
	}
	return torrentInfo{
		Hash:         d.Hash,
		Name:         d.Name,
		Category:     d.Category,
		SavePath:     d.SavePath,
		Size:         d.SizeBytes,
		Progress:     d.Progress,
		State:        qbtState(d.State),
		AddedOn:      d.AddedAt.Unix(),
		CompletionOn: completionOn,
	}
}

// qbtState translates AcerviNode's local state machine to the qBittorrent
// state vocabulary *arr apps pattern-match on. See docs/qbittorrent-api.md.
func qbtState(local string) string {
	switch local {
	case database.StateQueued:
		return "queuedDL"
	case database.StateDownloading:
		return "downloading"
	case database.StateProviderCompleted, database.StateReadyForImport:
		return "uploading"
	case database.StateError:
		return "error"
	default:
		return "unknown"
	}
}

// localState translates a provider's DownloadState into AcerviNode's local
// state machine. "provider_completed" is the terminal state this vertical
// slice reaches — "ready_for_import" belongs to the not-yet-built local
// mount/import phase (see ROADMAP.md).
func localState(s debrid.DownloadState) string {
	switch s {
	case debrid.StateQueued:
		return database.StateQueued
	case debrid.StateDownloading:
		return database.StateDownloading
	case debrid.StateCompleted:
		return database.StateProviderCompleted
	case debrid.StateError:
		return database.StateError
	default:
		return database.StateQueued
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
