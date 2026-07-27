// Package api implements AcerviNode's own versioned REST API (/api/v1) —
// health, version, provider status, settings, and download
// listing/management — the same API the embedded web UI (web/) is built on.
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

// Settings lets the API read and change provider configuration live,
// without a restart — see internal/debrid's Dynamic*Provider types, which
// is what makes an in-place swap possible, and settings.go for the HTTP
// surface built on this interface. Narrowly scoped to TorBox, the only
// provider that exists today; generalize when a second one is added (see
// docs/providers.md).
type Settings interface {
	TorBoxConfigured() bool
	SetTorBoxAPIKey(ctx context.Context, apiKey string) error
}

// Server is AcerviNode's native API.
type Server struct {
	apiKey          string
	version         string
	db              *database.DB
	torrentProvider deleter // nil if no torrent-capable provider is configured
	usenetProvider  deleter // nil if no usenet-capable provider is configured
	settings        Settings
	mux             *http.ServeMux
}

// NewServer builds the native API server. version is a free-form build
// identifier. torrentProvider/usenetProvider may be nil.
func NewServer(apiKey, version string, db *database.DB, torrentProvider deleter, usenetProvider deleter, settings Settings) *Server {
	s := &Server{
		apiKey: apiKey, version: version, db: db,
		torrentProvider: torrentProvider, usenetProvider: usenetProvider,
		settings: settings,
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
	s.mux.HandleFunc("GET /api/v1/settings/providers", s.requireAuth(s.handleGetProviderSettings))
	s.mux.HandleFunc("PUT /api/v1/settings/providers/torbox", s.requireAuth(s.handleSetTorBoxAPIKey))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": s.version})
}

// handleProviders reports providers as currently configured — computed live
// from Settings, not a snapshot from startup, so it reflects a key added or
// changed through the settings endpoints without needing a restart.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	providers := []ProviderStatus{}
	if s.settings.TorBoxConfigured() {
		providers = append(providers, ProviderStatus{Name: "torbox", TorrentCapable: true, UsenetCapable: true})
	}
	writeJSON(w, providers)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
