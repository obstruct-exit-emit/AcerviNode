package qbittorrent

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/acervinode/acervinode/internal/database"
)

// resolveHashes returns every tracked torrent matching a real qBittorrent
// "hashes" form parameter — one or more infohashes separated by "|", or the
// literal "all" (also "" — real qBittorrent's own JS client always sends
// "all" explicitly, but treating a missing param the same way costs nothing
// and matches splitFilter's identical convention in handleInfo). Unknown
// hashes are silently skipped, matching real qBittorrent's own behavior of
// simply not acting on a hash it doesn't recognize rather than erroring.
func (s *Server) resolveHashes(ctx context.Context, raw string) []*database.Download {
	if raw == "" || raw == "all" {
		rows, err := s.db.ListDownloads(ctx, database.KindTorrent)
		if err != nil {
			slog.Error("qbittorrent: list downloads failed", "error", err)
			return nil
		}
		return rows
	}
	var out []*database.Download
	for _, h := range strings.Split(raw, "|") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		d, err := s.db.GetDownloadByHash(ctx, h)
		if err != nil {
			slog.Error("qbittorrent: get download by hash failed", "hash", h, "error", err)
			continue
		}
		if d != nil {
			out = append(out, d)
		}
	}
	return out
}

// handleSetCategory implements POST /api/v2/torrents/setCategory — called by
// Sonarr/Radarr's MarkItemAsImported when a separate "post-import category"
// setting differs from the add-time one (confirmed against their real
// source, QBittorrent.cs's SetTorrentLabel caller: an optional setting, not
// part of the default add flow). Real qBittorrent 409s if category doesn't
// already exist there (categories must be created via createCategory
// first — Sonarr/Radarr's own Test() step already does this automatically,
// see QBittorrent.cs's TestCategory, unlike the SABnzbd shim's equivalent
// gap — see docs/sabnzbd-api.md#categories). AcerviNode doesn't replicate
// that strict check: it auto-registers the category here too, matching this
// shim's existing permissive philosophy (categories are stored and echoed
// back, never validated — see docs/qbittorrent-api.md) rather than adding a
// new failure mode with no real benefit.
func (s *Server) handleSetCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "")
		return
	}
	category := r.FormValue("category")
	ctx := r.Context()
	for _, d := range s.resolveHashes(ctx, r.FormValue("hashes")) {
		if err := s.db.UpdateDownloadCategory(ctx, d.ID, category); err != nil {
			slog.Error("qbittorrent: set category failed", "hash", d.Hash, "error", err)
		}
	}
	s.categories.add(category, "")
	writeText(w, http.StatusOK, "Ok.")
}

// handleSetShareLimits implements POST /api/v2/torrents/setShareLimits —
// called by Sonarr/Radarr on add when a release has seed ratio/time criteria
// configured (confirmed against their real source, SetTorrentSeedingConfiguration's
// caller: gated on remoteEpisode.SeedConfiguration being set, an optional
// per-release/profile setting). A no-op here: AcerviNode has no seeding
// concept at all — TorBox handles that server-side, entirely outside
// AcerviNode's control — so there is nothing to actually apply. Accepting
// and ignoring the call (rather than 404ing) is what lets Sonarr/Radarr's
// add flow complete normally for a user who has this setting turned on,
// same reasoning as handleTopPrio/handleSetForceStart below.
func (s *Server) handleSetShareLimits(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "")
		return
	}
	writeText(w, http.StatusOK, "Ok.")
}

// handleTopPrio implements POST /api/v2/torrents/topPrio — called by
// Sonarr/Radarr on add when "Recent/Older Priority" is explicitly set to
// "First" (confirmed against their real source, MoveTorrentToTopInQueue's
// caller). A no-op: AcerviNode has no download queue/priority-ordering
// concept — every tracked download is fetched as soon as
// max_concurrent_downloads allows — so there's nothing to actually
// reorder. See handleSetShareLimits's doc comment for why a no-op response
// is the right behavior here rather than an error.
func (s *Server) handleTopPrio(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "")
		return
	}
	writeText(w, http.StatusOK, "Ok.")
}

// handleSetForceStart implements POST /api/v2/torrents/setForceStart —
// called by Sonarr/Radarr on add when "Initial State" is explicitly set to
// "Force Start" (confirmed against their real source, SetForceStart's
// caller). A no-op: AcerviNode has no paused/queued-but-not-fetching state
// for a torrent to be force-started out of — every add is already
// unconditionally live the moment the provider accepts it. See
// handleSetShareLimits's doc comment for why a no-op response is the right
// behavior here rather than an error.
func (s *Server) handleSetForceStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "")
		return
	}
	writeText(w, http.StatusOK, "Ok.")
}
