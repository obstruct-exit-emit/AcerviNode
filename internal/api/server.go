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

// GeneralInfo is AcerviNode's own runtime configuration, as reported to the
// settings UI — everything except provider credentials (see Settings /
// TorBoxConfigured for those). Includes APIKey itself: unlike a provider
// secret, this is the key the caller already had to present to reach this
// endpoint, so returning it back is how the UI (and a human copying it into
// Sonarr/Radarr) can see it without digging through server logs or
// config.yaml.
type GeneralInfo struct {
	APIKey                string `json:"api_key"`
	Port                  int    `json:"port"`
	DataDir               string `json:"data_dir"`
	DownloadDir           string `json:"download_dir"`
	LogLevel              string `json:"log_level"`
	ImportIntervalSeconds int    `json:"import_interval_seconds"`
	ImportMaxRetries      int    `json:"import_max_retries"`
}

// Settings lets the API read and change configuration live, without a
// restart — see internal/debrid's Dynamic*Provider types, which is what
// makes an in-place provider swap possible, and settings.go for the HTTP
// surface built on this interface. Provider methods are narrowly scoped to
// TorBox, the only provider that exists today; generalize when a second one
// is added (see docs/providers.md).
type Settings interface {
	TorBoxConfigured() bool
	SetTorBoxAPIKey(ctx context.Context, apiKey string) error
	// APIKey returns AcerviNode's own current API key — the live source of
	// truth every authenticated route (native API and both compat shims)
	// checks against, so a regenerated key takes effect everywhere at once.
	APIKey() string
	// RegenerateAPIKey replaces the current API key with a fresh random one,
	// applies it immediately, and persists it to config.yaml.
	RegenerateAPIKey(ctx context.Context) (string, error)
	// General reports the rest of AcerviNode's current configuration.
	General() GeneralInfo
}

// Server is AcerviNode's native API.
type Server struct {
	version         string
	db              *database.DB
	torrentProvider deleter // nil if no torrent-capable provider is configured
	usenetProvider  deleter // nil if no usenet-capable provider is configured
	settings        Settings
	mux             *http.ServeMux
}

// NewServer builds the native API server. version is a free-form build
// identifier. torrentProvider/usenetProvider may be nil. Auth checks
// settings.APIKey() live on every request rather than a value captured at
// construction, so a regenerated key takes effect immediately — see
// requireAuth.
func NewServer(version string, db *database.DB, torrentProvider deleter, usenetProvider deleter, settings Settings) *Server {
	s := &Server{
		version: version, db: db,
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
	s.mux.HandleFunc("GET /api/v1/settings/general", s.requireAuth(s.handleGetGeneralSettings))
	s.mux.HandleFunc("POST /api/v1/settings/api-key/regenerate", s.requireAuth(s.handleRegenerateAPIKey))
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
