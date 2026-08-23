package api

import (
	"encoding/json"
	"net/http"
)

// handleGetGeneralSettings implements GET /api/v1/settings/general — unlike
// the provider settings above, this does return the actual API key: it's the
// same key the caller already had to present to reach this endpoint, so
// there's nothing to protect by hiding it back from them (see GeneralInfo).
func (s *Server) handleGetGeneralSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settings.General())
}

// handleUpdateGeneralSettings implements PUT /api/v1/settings/general.
// Everything except Port/DataDir applies immediately; those two are
// persisted but only take effect after a restart — see GeneralUpdate and the
// response's restart_required field.
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

// handleRestartServer implements POST /api/v1/settings/system/restart —
// gracefully shuts down and exits so a restart-required setting (port,
// tls_enabled/tls_port, ...) already saved to config.yaml takes effect on
// the next start. supervised reflects whether a process supervisor is
// actually watching this process (see Settings.SupervisedBySystemd) — the
// UI uses it to say something truthful instead of a confident "restarting…"
// when nothing will actually bring the process back.
func (s *Server) handleRestartServer(w http.ResponseWriter, r *http.Request) {
	supervised := s.settings.SupervisedBySystemd()
	if err := s.settings.RequestRestart(r.Context()); err != nil {
		http.Error(w, "failed to restart: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"restarting": true, "supervised": supervised})
}

// handleRegenerateCertificate implements POST /api/v1/settings/tls/regenerate
// — forces a fresh self-signed certificate (see Settings.RegenerateCertificate)
// when the existing one's SANs no longer match how the instance is reached.
// Always requires a restart afterward: the running HTTPS listener already
// has the old cert loaded in memory.
func (s *Server) handleRegenerateCertificate(w http.ResponseWriter, r *http.Request) {
	if err := s.settings.RegenerateCertificate(r.Context()); err != nil {
		http.Error(w, "failed to regenerate certificate: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"restart_required": true})
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

// handleRemoveCategory implements DELETE /api/v1/settings/categories/{category}
// — forgets the category entirely (path override and registration with both
// compat shims), unlike handleSetCategoryPath with an empty path, which only
// clears the override. See Settings.RemoveCategory.
func (s *Server) handleRemoveCategory(w http.ResponseWriter, r *http.Request) {
	category := r.PathValue("category")
	if err := s.settings.RemoveCategory(r.Context(), category); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type accountStatusResponse struct {
	Available            bool   `json:"available"`
	Error                string `json:"error,omitempty"`
	PlanName             string `json:"plan_name,omitempty"`
	IsSubscribed         bool   `json:"is_subscribed,omitempty"`
	PremiumExpiresAt     string `json:"premium_expires_at,omitempty"`
	TotalBytesDownloaded int64  `json:"total_bytes_downloaded,omitempty"`
	// CooldownUntil surfaces a real provider-imposed restriction that
	// otherwise looks like "everything's stopped updating" with no visible
	// explanation — see debrid.AccountStatus.CooldownUntil's own doc
	// comment for what was (and wasn't) confirmed about it. Empty when not
	// currently restricted, or for a provider that doesn't report this.
	CooldownUntil string `json:"cooldown_until,omitempty"`
}

// handleGetAccountStatus implements GET /api/v1/settings/account — a live
// call to the configured provider, not a cached snapshot. Not configured
// yet, or a provider that doesn't support account status at all, are both
// routine ("available": false, with a reason), not a hard failure — the
// settings UI just doesn't show the section rather than erroring the whole
// page.
func (s *Server) handleGetAccountStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.settings.AccountStatus(r.Context())
	if err != nil {
		writeJSON(w, accountStatusResponse{Available: false, Error: err.Error()})
		return
	}
	writeJSON(w, accountStatusResponse{
		Available:            true,
		PlanName:             status.PlanName,
		IsSubscribed:         status.IsSubscribed,
		PremiumExpiresAt:     status.PremiumExpiresAt,
		TotalBytesDownloaded: status.TotalBytesDownloaded,
		CooldownUntil:        status.CooldownUntil,
	})
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
