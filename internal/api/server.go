// Package api implements AcerviNode's own versioned REST API (/api/v1) —
// health, version, provider status, and download listing/management, the
// same API the embedded web UI (internal/webui) is built on.
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// ProviderStatus summarizes one configured debrid provider for
// GET /api/v1/providers.
type ProviderStatus struct {
	Name           string `json:"name"`
	TorrentCapable bool   `json:"torrent_capable"`
	UsenetCapable  bool   `json:"usenet_capable"`
}

// deleter is the subset of debrid.TorrentProvider/debrid.UsenetProvider that
// DELETE /api/v1/downloads/{id} needs — same narrow-interface approach as
// internal/importer's fileResolver.
type deleter interface {
	Delete(ctx context.Context, id debrid.ProviderDownloadID, deleteFiles bool) error
}

// Server is AcerviNode's native API.
type Server struct {
	apiKey          string
	version         string
	providers       []ProviderStatus
	db              *database.DB
	torrentProvider deleter // nil if no torrent-capable provider is configured
	usenetProvider  deleter // nil if no usenet-capable provider is configured
	mux             *http.ServeMux
}

// NewServer builds the native API server. version is a free-form build
// identifier; providers describes whatever was actually wired up in
// cmd/acervinode/main.go. torrentProvider/usenetProvider may be nil.
func NewServer(apiKey, version string, providers []ProviderStatus, db *database.DB, torrentProvider deleter, usenetProvider deleter) *Server {
	if providers == nil {
		// A nil slice marshals to JSON null, not []; every caller of this
		// list-shaped endpoint (including the embedded UI) expects an array.
		providers = []ProviderStatus{}
	}
	s := &Server{
		apiKey: apiKey, version: version, providers: providers, db: db,
		torrentProvider: torrentProvider, usenetProvider: usenetProvider,
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Health is intentionally unauthenticated — it's what a container
	// orchestrator or systemd healthcheck hits, and shouldn't need a key.
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/version", s.requireAuth(s.handleVersion))
	s.mux.HandleFunc("GET /api/v1/providers", s.requireAuth(s.handleProviders))
	s.mux.HandleFunc("GET /api/v1/downloads", s.requireAuth(s.handleListDownloads))
	s.mux.HandleFunc("GET /api/v1/downloads/{id}", s.requireAuth(s.handleGetDownload))
	s.mux.HandleFunc("DELETE /api/v1/downloads/{id}", s.requireAuth(s.handleDeleteDownload))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": s.version})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.providers)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
