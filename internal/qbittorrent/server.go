// Package qbittorrent emulates enough of qBittorrent's real Web API
// (/api/v2/...) that Sonarr, Radarr, and other *arr apps configured with a
// "qBittorrent" download client work against AcerviNode unmodified. See
// docs/qbittorrent-api.md for the endpoint list and the state-string mapping.
package qbittorrent

import (
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// apiKeySource is the minimal interface needed to check a login attempt's
// password against the live AcerviNode API key — matching cmd/acervinode's
// liveSettings, whose APIKey() reflects a key regenerated through the
// settings API with no restart needed (see internal/api.Settings).
type apiKeySource interface {
	APIKey() string
}

// Server is an http.Handler implementing the qBittorrent Web API surface
// AcerviNode needs. It talks to a debrid.TorrentProvider for everything
// download-related and to the shared downloads table for everything local
// (category, save path, the AcerviNode-side state machine).
type Server struct {
	provider debrid.TorrentProvider
	db       *database.DB
	// apiKey is the password accepted by /api/v2/auth/login — any username is
	// accepted, matching how the SABnzbd shim treats its own apikey param as
	// the single shared secret (see docs/configuration.md).
	apiKey apiKeySource

	mux        *http.ServeMux
	sessions   *sessionStore
	categories *categoryStore
}

// NewServer builds a qBittorrent-compat Server backed by provider and db.
func NewServer(provider debrid.TorrentProvider, db *database.DB, apiKey apiKeySource) *Server {
	s := &Server{
		provider:   provider,
		db:         db,
		apiKey:     apiKey,
		sessions:   newSessionStore(),
		categories: newCategoryStore(),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v2/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v2/auth/logout", s.handleLogout)

	s.mux.HandleFunc("GET /api/v2/app/version", s.handleAppVersion)
	s.mux.HandleFunc("GET /api/v2/app/webapiVersion", s.handleWebAPIVersion)

	s.mux.HandleFunc("POST /api/v2/torrents/add", s.requireAuth(s.handleAdd))
	s.mux.HandleFunc("GET /api/v2/torrents/info", s.requireAuth(s.handleInfo))
	s.mux.HandleFunc("GET /api/v2/torrents/properties", s.requireAuth(s.handleProperties))
	s.mux.HandleFunc("GET /api/v2/torrents/files", s.requireAuth(s.handleFiles))
	s.mux.HandleFunc("POST /api/v2/torrents/delete", s.requireAuth(s.handleDelete))

	s.mux.HandleFunc("GET /api/v2/torrents/categories", s.requireAuth(s.handleCategories))
	s.mux.HandleFunc("POST /api/v2/torrents/createCategory", s.requireAuth(s.handleCreateCategory))
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.WriteHeader(status)
	w.Write([]byte(body))
}
