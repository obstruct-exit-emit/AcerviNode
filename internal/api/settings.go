package api

import (
	"encoding/json"
	"net/http"
)

type providerSettingResponse struct {
	Configured bool `json:"configured"`
}

// handleGetProviderSettings implements GET /api/v1/settings/providers. It
// never returns the actual API key — only whether one is set — matching
// docs/configuration.md's stance that secrets are write-only through this
// API once configured.
func (s *Server) handleGetProviderSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]providerSettingResponse{
		"torbox": {Configured: s.settings.TorBoxConfigured()},
	})
}

type setAPIKeyRequest struct {
	APIKey string `json:"api_key"`
}

// handleSetTorBoxAPIKey implements PUT /api/v1/settings/providers/torbox.
// Takes effect immediately (see internal/debrid's Dynamic*Provider) and is
// persisted to config.yaml so it survives a restart — see
// cmd/acervinode's liveSettings.
func (s *Server) handleSetTorBoxAPIKey(w http.ResponseWriter, r *http.Request) {
	var req setAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.APIKey == "" {
		http.Error(w, "api_key must not be empty", http.StatusBadRequest)
		return
	}
	if err := s.settings.SetTorBoxAPIKey(r.Context(), req.APIKey); err != nil {
		http.Error(w, "failed to apply api key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestTorBoxConnection implements
// POST /api/v1/settings/providers/torbox/test — a real, live connectivity
// and auth check against TorBox with the currently configured key, not just
// "is a key set" (see handleGetProviderSettings above).
func (s *Server) handleTestTorBoxConnection(w http.ResponseWriter, r *http.Request) {
	latencyMs, err := s.settings.TestTorBoxConnection(r.Context())
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "latency_ms": latencyMs})
}

// handleGetGeneralSettings implements GET /api/v1/settings/general — unlike
// the provider settings above, this does return the actual API key: it's the
// same key the caller already had to present to reach this endpoint, so
// there's nothing to protect by hiding it back from them (see GeneralInfo).
func (s *Server) handleGetGeneralSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settings.General())
}

// handleUpdateGeneralSettings implements PUT /api/v1/settings/general.
// DownloadDir/LogLevel/ImportIntervalSeconds/ImportMaxRetries apply
// immediately; Port/DataDir are persisted but only take effect after a
// restart — see GeneralUpdate and the response's restart_required field.
func (s *Server) handleUpdateGeneralSettings(w http.ResponseWriter, r *http.Request) {
	var req GeneralUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	restartRequired, err := s.settings.UpdateGeneral(r.Context(), req)
	if err != nil {
		http.Error(w, "invalid settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"restart_required": restartRequired})
}

type categoriesResponse struct {
	Torrent []string          `json:"torrent"`
	Usenet  []string          `json:"usenet"`
	Paths   map[string]string `json:"paths"`
}

// handleGetCategories implements GET /api/v1/settings/categories.
func (s *Server) handleGetCategories(w http.ResponseWriter, r *http.Request) {
	torrent, usenet := s.settings.Categories()
	writeJSON(w, categoriesResponse{Torrent: torrent, Usenet: usenet, Paths: s.settings.CategoryPaths()})
}

type addCategoryRequest struct {
	Protocol string `json:"protocol"`
	Name     string `json:"name"`
}

// handleAddCategory implements POST /api/v1/settings/categories — manually
// registers a category, the same way an *arr app declaring one does.
func (s *Server) handleAddCategory(w http.ResponseWriter, r *http.Request) {
	var req addCategoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.settings.AddCategory(req.Protocol, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type setCategoryPathRequest struct {
	Category string `json:"category"`
	Path     string `json:"path"`
}

// handleSetCategoryPath implements PUT /api/v1/settings/categories/path — sets
// (or, with an empty path, clears) a per-category override directory that
// internal/importer uses instead of download_dir/<category> for downloads in
// that category. Applies immediately, no restart needed.
func (s *Server) handleSetCategoryPath(w http.ResponseWriter, r *http.Request) {
	var req setCategoryPathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.settings.SetCategoryPath(r.Context(), req.Category, req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRegenerateAPIKey implements POST /api/v1/settings/api-key/regenerate.
// The new key applies immediately (every route checks settings.APIKey()
// live) and is persisted to config.yaml — see cmd/acervinode's liveSettings.
// Every other client using the old key (other browser tabs, *arr apps
// configured against the compat shims) stops authenticating until it's
// updated with the new one.
func (s *Server) handleRegenerateAPIKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.settings.RegenerateAPIKey(r.Context())
	if err != nil {
		http.Error(w, "failed to regenerate api key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"api_key": key})
}
