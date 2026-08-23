// Package sabnzbd emulates enough of SABnzbd's real API (/api?mode=...) that
// Sonarr, Radarr, and other *arr apps configured with a "SABnzbd" download
// client work against AcerviNode unmodified. See docs/sabnzbd-api.md for the
// endpoint list and the state-string mapping.
package sabnzbd

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
	"log/slog"
)

// settingsSource is the minimal interface needed to check the "apikey"
// parameter against the live AcerviNode API key — matching cmd/acervinode's
// liveSettings, whose APIKey() reflects a key regenerated through the
// settings API with no restart needed (see internal/api.Settings) — plus
// whatever else a handler here needs from live settings (see
// handleDelete's DeleteLocalFiles).
type settingsSource interface {
	APIKey() string
	// DeleteLocalFiles removes a download's local files from disk, if any —
	// see handleDelete. Delegates to internal/importer, the only place that
	// knows how to resolve a download's actual destination directory live.
	DeleteLocalFiles(d *database.Download) error
}

// defaultUsenet is the provider a new download goes to, or nil if nothing
// registered supports usenet.
func (s *Server) defaultUsenet() *debrid.DynamicUsenetProvider {
	return s.registry.DefaultUsenet()
}

// usenetFor resolves the provider d belongs to, or nil if that provider
// isn't available for usenet. A provider_download_id means nothing to a
// different account, so acting on it would at best fail and at worst hit an
// unrelated download that happens to share the id. A row with no provider
// recorded falls back to the default — older rows predate the column being
// populated, and nothing writes an empty provider today.
func (s *Server) usenetFor(d *database.Download) *debrid.DynamicUsenetProvider {
	name := d.Provider
	if name == "" {
		name = s.registry.DefaultNameFor(debrid.KindUsenet)
	}
	p := s.registry.Usenet(name)
	if p == nil {
		slog.Warn("sabnzbd: no provider available for download",
			"id", d.ID, "download_provider", d.Provider, "resolved_name", name)
	}
	return p
}

// Server is an http.Handler implementing the SABnzbd API surface AcerviNode
// needs. It talks to a debrid.UsenetProvider for everything download-related
// and to the shared downloads table for everything local.
type Server struct {
	// registry is every configured provider. Adds go to the default; work
	// on an existing download resolves that download's own provider — see
	// defaultUsenet and usenetFor.
	registry *debrid.Registry
	db       *database.DB
	// settings.APIKey() is checked against the "apikey" query/form parameter
	// on every request — SABnzbd's real auth model has no login step (see
	// docs/configuration.md).
	settings settingsSource

	mux        *http.ServeMux
	categories *categoryStore
}

// NewServer builds a SABnzbd-compat Server backed by provider and db.
func NewServer(registry *debrid.Registry, db *database.DB, settings settingsSource) *Server {
	s := &Server{
		registry:   registry,
		db:         db,
		settings:   settings,
		categories: newCategoryStore(),
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/api", s.handleAPI)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleAPI dispatches every request by its "mode" parameter, matching
// SABnzbd's real single-endpoint API shape.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// ParseMultipartForm populates r.Form from the query string and any
	// urlencoded/multipart body regardless of whether the multipart parse
	// itself succeeds, so a plain GET or urlencoded POST still works even
	// though we ignore its error here.
	_ = r.ParseMultipartForm(64 << 20)

	// Constant-time, matching the native API's own auth check (see
	// internal/api/auth.go) — a plain != comparison here would be the one
	// auth entry point in the app not following that convention.
	if subtle.ConstantTimeCompare([]byte(r.FormValue("apikey")), []byte(s.settings.APIKey())) != 1 {
		writeJSON(w, map[string]any{"status": false, "error": "API Key Incorrect"})
		return
	}

	switch r.FormValue("mode") {
	case "version":
		s.handleVersion(w, r)
	case "get_config":
		s.handleGetConfig(w, r)
	case "fullstatus":
		s.handleFullStatus(w, r)
	case "addurl":
		s.handleAddURL(w, r)
	case "addfile":
		s.handleAddFile(w, r)
	case "queue":
		s.handleQueue(w, r)
	case "history":
		s.handleHistory(w, r)
	default:
		writeJSON(w, map[string]any{"status": false, "error": "Unknown mode"})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
