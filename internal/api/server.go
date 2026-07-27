// Package api implements AcerviNode's own versioned REST API (/api/v1),
// deliberately thin in this vertical slice — health, version, and provider
// status are enough to prove the server is up and configured correctly.
// Richer endpoints (download listing/management, matching what a future UI
// would use) are tracked as Phase 2 on the roadmap.
package api

import (
	"encoding/json"
	"net/http"
)

// ProviderStatus summarizes one configured debrid provider for
// GET /api/v1/providers.
type ProviderStatus struct {
	Name           string `json:"name"`
	TorrentCapable bool   `json:"torrent_capable"`
	UsenetCapable  bool   `json:"usenet_capable"`
}

// Server is AcerviNode's native API.
type Server struct {
	apiKey    string
	version   string
	providers []ProviderStatus
	mux       *http.ServeMux
}

// NewServer builds the native API server. version is a free-form build
// identifier; providers describes whatever was actually wired up in
// cmd/acervinode/main.go.
func NewServer(apiKey, version string, providers []ProviderStatus) *Server {
	s := &Server{apiKey: apiKey, version: version, providers: providers}
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
