// Package sabnzbd emulates enough of SABnzbd's real API (/api?mode=...) that
// Sonarr, Radarr, and other *arr apps configured with a "SABnzbd" download
// client work against AcerviNode unmodified. See docs/sabnzbd-api.md for the
// endpoint list and the state-string mapping.
package sabnzbd

import (
	"encoding/json"
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// apiKeySource is the minimal interface needed to check the "apikey"
// parameter against the live AcerviNode API key — matching cmd/acervinode's
// liveSettings, whose APIKey() reflects a key regenerated through the
// settings API with no restart needed (see internal/api.Settings).
type apiKeySource interface {
	APIKey() string
}

// Server is an http.Handler implementing the SABnzbd API surface AcerviNode
// needs. It talks to a debrid.UsenetProvider for everything download-related
// and to the shared downloads table for everything local.
type Server struct {
	provider debrid.UsenetProvider
	db       *database.DB
	// apiKey is checked against the "apikey" query/form parameter on every
	// request — SABnzbd's real auth model has no login step (see
	// docs/configuration.md).
	apiKey apiKeySource

	mux        *http.ServeMux
	categories *categoryStore
}

// NewServer builds a SABnzbd-compat Server backed by provider and db.
func NewServer(provider debrid.UsenetProvider, db *database.DB, apiKey apiKeySource) *Server {
	s := &Server{
		provider:   provider,
		db:         db,
		apiKey:     apiKey,
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

	if r.FormValue("apikey") != s.apiKey.APIKey() {
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
