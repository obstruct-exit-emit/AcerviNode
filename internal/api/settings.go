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

// handleGetGeneralSettings implements GET /api/v1/settings/general — unlike
// the provider settings above, this does return the actual API key: it's the
// same key the caller already had to present to reach this endpoint, so
// there's nothing to protect by hiding it back from them (see GeneralInfo).
func (s *Server) handleGetGeneralSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settings.General())
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
