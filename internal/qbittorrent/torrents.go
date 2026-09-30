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
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
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

// recordPendingAdd notes an *arr add that failed after the request went out,
// so discovery can recognise the item as Managed if the provider took it
// anyway. Best-effort: a failure to record leaves exactly the behaviour that
// existed before this, so it is logged rather than surfaced to the *arr app,
// which is already being told the add failed.
func (s *Server) recordPendingAdd(ctx context.Context, kind database.Kind, hash, name, category, savePath string) {
	if err := s.db.RecordPendingArrAdd(ctx, &database.PendingArrAdd{
		Provider: s.registry.DefaultNameFor(debrid.KindTorrent),
		Kind:     kind,
		Hash:     hash,
		Name:     name,
		Category: category,
		// Namespaced here too: discovery stamps the recovered row with
		// this exact value, so recording the bare directory would bring
		// the shared-destination collision back by a different route.
		SavePath: namespaceSavePath(savePath, name),
	}); err != nil {
		slog.Error("qbittorrent: could not record a failed add for later reconciliation", "error", err)
	}
}

// infohashFromMagnet pulls the btih out of a magnet, lowercased, or returns
// empty when there is not one to find.
func infohashFromMagnet(magnet string) string {
	m := magnetInfohash.FindStringSubmatch(magnet)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

var magnetInfohash = regexp.MustCompile(`(?i)xt=urn:btih:([0-9a-z]+)`)

// namespaceSavePath puts a download into its own directory beneath the
// save_path an *arr app asked for, instead of directly into it.
//
// internal/importer's resolveDestDir treats a non-empty save_path as the final
// destination verbatim, while every other branch appends the download's own
// name -- and its doc comment promises the result is "always namespaced by the
// download's own name so sibling downloads in the same category never collide."
// An explicit save_path broke that promise: every download sent with the same
// one landed directly in it, siblings overwriting each other's files, and
// cleanupDownload/RemoveLocalFiles then os.RemoveAll'd that shared directory,
// taking every other download in it.
//
// Doing it here rather than in resolveDestDir is deliberate. After a fetch,
// database.UpdateDownloadSavePath writes the resolved destination back into
// save_path, so resolveDestDir's verbatim branch is load-bearing for every
// already-fetched row -- appending the name there would turn
// <dir>/<category>/<name> into <dir>/<category>/<name>/<name> and break cleanup
// for existing downloads. Fixing it at the point the value is stored needs no
// migration and leaves those rows alone.
//
// It also makes the two fields this shim reports mean what real qBittorrent
// means by them: the directory the *arr app asked for stays save_path, and
// content_path becomes the per-download directory beneath it (see
// toTorrentInfo, which derives the pair by splitting this value).
//
// An empty save_path stays empty -- that is the common case, and the signal
// internal/importer uses to resolve and persist a destination itself. A name
// that is not a single path segment is ignored rather than joined: it cannot be
// built into a directory safely, and refusing keeps a "../.." out of the path.
func namespaceSavePath(supplied, name string) string {
	dir := strings.TrimSpace(supplied)
	if dir == "" {
		return ""
	}
	dir = filepath.Clean(dir)
	n := strings.TrimSpace(name)
	if n == "" || n != filepath.Base(n) || n == "." || n == ".." {
		return dir
	}
	// Already namespaced -- a re-add of a row whose save_path was recorded by
	// an earlier pass through here must not gain a second copy of the name.
	if filepath.Base(dir) == n {
		return dir
	}
	return filepath.Join(dir, n)
}

func (s *Server) addMagnet(ctx context.Context, magnet, category, savePath string) error {
	p := s.defaultTorrent()
	if p == nil {
		return debrid.ErrNoProvider
	}
	id, err := p.AddMagnet(ctx, magnet, debrid.AddOptions{Name: magnetDisplayName(magnet)})
	if err != nil {
		// The provider may have accepted this even though the reply did not
		// reach us. Without a record of the attempt, the next discovery pass
		// finds it untracked and adopts it as a Manual download -- which is
		// what "a Managed download turned into a Manual one" actually is.
		s.recordPendingAdd(ctx, database.KindTorrent, infohashFromMagnet(magnet), magnetDisplayName(magnet), category, savePath)
		return err
	}
	return s.storeNewDownload(ctx, id, magnet, category, savePath)
}

func (s *Server) addTorrentFile(ctx context.Context, filename string, data []byte, category, savePath string) error {
	p := s.defaultTorrent()
	if p == nil {
		return debrid.ErrNoProvider
	}
	id, err := p.AddTorrentFile(ctx, filename, data, debrid.AddOptions{Name: filename})
	if err != nil {
		// No infohash without parsing the torrent, so this one matches on
		// name alone -- see database.ClaimPendingArrAdd.
		s.recordPendingAdd(ctx, database.KindTorrent, "", filename, category, savePath)
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
	p := s.defaultTorrent()
	if p == nil {
		return debrid.ErrNoProvider
	}
	status, err := p.Status(ctx, id)
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
		Provider:           p.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindTorrent,
		Hash:               strings.ToLower(status.Hash),
		Name:               status.Name,
		Category:           category,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		// A provider can report a failure the instant it accepts an add,
		// and a row born in StateError needs its reason like any other --
		// see database.ProviderErrorMessage.
		ErrorMessage: database.ProviderErrorMessage(status.State, status.RawState),
		Progress:     status.Progress,
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
	// Set after the name is settled, since that is what it is namespaced by.
	d.SavePath = namespaceSavePath(savePath, d.Name)
	// Not a plain InsertDownload: a row for this provider id may already
	// exist (TorBox dedupes by content, and the importer's discovery pass
	// can adopt a just-added item first), in which case that row is claimed
	// for *arr rather than colliding with it — see InsertOrClaimForArr.
	_, err = s.db.InsertOrClaimForArr(ctx, d)
	return err
}

// handleInfo implements GET /api/v2/torrents/info: whatever
// internal/importer's last poll wrote, for Managed torrents only. It does not
// reach the provider (see the package comment on why this shim is a wall) and
// it does not report Manual downloads (see database.ListManagedDownloads).
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.ListManagedDownloads(ctx, database.KindTorrent)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

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
		// Read, never fetch. Whatever internal/importer's last poll wrote is
		// what an *arr app sees, however often it asks — see the package
		// comment on why this shim is deliberately a wall.
		live, _ := s.db.LiveStatus(d.ID)
		items = append(items, toTorrentInfo(d, live, fetchProgress, hasFetchProgress))
	}

	writeJSON(w, items)
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

// defaultTorrent is the provider a new torrent goes to, or nil if nothing
// registered supports torrents.
func (s *Server) defaultTorrent() *debrid.DynamicTorrentProvider {
	return s.registry.DefaultTorrent()
}

// torrentFor resolves the provider d belongs to, or nil if that provider
// isn't available for torrents. A provider_download_id means nothing to a
// different account, so acting on it would at best fail and at worst hit an
// unrelated download that happens to share the id. A row with no provider
// recorded falls back to the default — older rows predate the column being
// populated, and nothing writes an empty provider today.
func (s *Server) torrentFor(d *database.Download) *debrid.DynamicTorrentProvider {
	name := d.Provider
	if name == "" {
		name = s.registry.DefaultNameFor(debrid.KindTorrent)
	}
	p := s.registry.Torrent(name)
	if p == nil {
		slog.Warn("qbittorrent: no provider available for download",
			"id", d.ID, "download_provider", d.Provider, "resolved_name", name)
	}
	return p
}

// handleFiles implements GET /api/v2/torrents/files?hash=...
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	d, ok := s.downloadByHash(w, r)
	if !ok {
		return
	}
	p := s.torrentFor(d)
	if p == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	files, err := p.Files(r.Context(), debrid.ProviderDownloadID(d.ProviderDownloadID))
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
		// Managed-scoped: an *arr app naming a Manual download's hash must not
		// be able to remove the operator's own download — see
		// database.GetManagedDownloadByHash.
		d, err := s.db.GetManagedDownloadByHash(ctx, hash)
		if err != nil || d == nil {
			continue
		}
		// Whether the provider actually removed its own copy decides the
		// tombstone's lifetime: a failed delete leaves the item on the
		// account, where discovery would re-adopt it as a ghost once a
		// short window lapsed — see database.RecordDeletedDownload.
		providerConfirmed := true
		p := s.torrentFor(d)
		if p == nil {
			providerConfirmed = false
		} else if err := p.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), deleteFiles); err != nil {
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
	// Managed-scoped for the same reason as the listing: a row an *arr app
	// cannot see should not be one it can interrogate either.
	d, err := s.db.GetManagedDownloadByHash(r.Context(), hash)
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
func toTorrentInfo(d *database.Download, live database.LiveStatus, fetchProgress float64, hasFetchProgress bool) torrentInfo {
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
		Eta:          qbtETA(live.ETASeconds),
		AddedOn:      d.AddedAt.Unix(),
		CompletionOn: completionOn,
		NumSeeds:     live.Seeders,
		NumLeechs:    live.Leechers,
		DlSpeed:      live.DownloadSpeedBytes,
		Ratio:        0,
		RatioLimit:   0,
	}
}

// qbtUnknownETA is qBittorrent's own value for "no idea how long this will
// take" -- 8640000 seconds, i.e. 100 days. Sonarr and Radarr special-case it by
// name (GetRemainingTime: `if (torrent.Eta == 8640000) return null;`, commented
// "qBittorrent sends eta=8640000 if unknown such as queued"), so it is the only
// way to say "unknown" in this protocol.
const qbtUnknownETA int64 = 8640000

// qbtETA maps a provider's reported seconds-remaining onto that vocabulary.
//
// A provider that has not reported an ETA -- or a row nothing has been polled
// for yet, which is the common case right after an add and exactly when an *arr
// app is most likely to be watching -- leaves this at 0. Sent as 0 it does not
// read as "unknown" at the far end: it parses as zero seconds remaining, so
// every queued download claimed to be finishing immediately.
//
// Values beyond a year are normalised to the sentinel as well. *arr already
// treats those as unknown (`if (torrent.Eta < 0 || torrent.Eta > 365 * 24 *
// 3600) return null;`), so this only makes the one value we emit for "unknown"
// consistent rather than relying on two code paths there agreeing.
func qbtETA(seconds int64) int64 {
	if seconds <= 0 || seconds > 365*24*3600 {
		return qbtUnknownETA
	}
	return seconds
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
