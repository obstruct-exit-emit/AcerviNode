package api

import (
	"encoding/json"
	"net/http"
)

type providerSettingResponse struct {
	Name string `json:"name"`
	// Type is which service this entry actually is. Equal to Name for a
	// first account; different when one service holds several, which is
	// what lets the UI offer "another TorBox account" from TorBox's own
	// card without asking which service it is.
	Type           string `json:"type"`
	Configured     bool   `json:"configured"`
	TorrentCapable bool   `json:"torrent_capable"`
	UsenetCapable  bool   `json:"usenet_capable"`
	WebDLCapable   bool   `json:"webdl_capable"`
	// Default marks which provider a new download goes to when nothing says
	// otherwise — see config.Config.DefaultProvider.
	Default bool `json:"default"`
}

// handleGetProviderSettings implements GET /api/v1/settings/providers. It
// never returns an actual API key — only whether one is set — matching
// docs/configuration.md's stance that secrets are write-only through this
// API once configured.
//
// Lists every *registered* provider, including ones holding no credentials
// yet: this is the settings surface, so a provider you could configure has
// to be visible before you configure it. That is deliberately the opposite
// of GET /api/v1/providers, which answers "what can I actually use right
// now" and omits them.
func (s *Server) handleGetProviderSettings(w http.ResponseWriter, r *http.Request) {
	defaultName := s.settings.DefaultProvider()
	out := []providerSettingResponse{}
	for _, name := range s.registry.Names() {
		out = append(out, providerSettingResponse{
			Name:           name,
			Type:           s.settings.ProviderType(name),
			Configured:     s.settings.ProviderConfigured(name),
			TorrentCapable: s.registry.Torrent(name) != nil,
			UsenetCapable:  s.registry.Usenet(name) != nil,
			WebDLCapable:   s.registry.WebDL(name) != nil,
			Default:        name == defaultName,
		})
	}
	writeJSON(w, out)
}

type setAPIKeyRequest struct {
	APIKey string `json:"api_key"`
}

// handleSetProviderAPIKey implements PUT /api/v1/settings/providers/{name}.
// Takes effect immediately (see internal/debrid's Dynamic*Provider) and is
// persisted to config.yaml so it survives a restart — see cmd/acervinode's
// liveSettings.
//
// An empty api_key clears that provider's credentials rather than being
// rejected: it is how a provider gets switched off without hand-editing
// config.yaml. The provider stays registered, so it can be configured again
// later without a restart.
func (s *Server) handleSetProviderAPIKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.providerRegistered(name) {
		http.Error(w, "unknown provider "+name, http.StatusNotFound)
		return
	}
	var req setAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.settings.SetProviderAPIKey(r.Context(), name, req.APIKey); err != nil {
		http.Error(w, "failed to apply api key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestProviderConnection implements
// POST /api/v1/settings/providers/{name}/test — a real, live connectivity
// and auth check against that provider with its currently configured key,
// not just "is a key set" (see handleGetProviderSettings above).
func (s *Server) handleTestProviderConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.providerRegistered(name) {
		http.Error(w, "unknown provider "+name, http.StatusNotFound)
		return
	}
	latencyMs, err := s.settings.TestProviderConnection(r.Context(), name)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "latency_ms": latencyMs})
}

type setDefaultProviderRequest struct {
	Provider string `json:"provider"`
}

// handleSetDefaultProvider implements
// PUT /api/v1/settings/providers/default — which provider a new download
// goes to when nothing says otherwise (see config.Config.DefaultProvider).
//
// Deliberately allows naming a provider that holds no credentials yet:
// setting the default before pasting its key is a reasonable order to do
// things in, and the add endpoints already fail clearly when the provider
// they resolve to can't answer.
func (s *Server) handleSetDefaultProvider(w http.ResponseWriter, r *http.Request) {
	var req setDefaultProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !s.providerRegistered(req.Provider) {
		http.Error(w, "unknown provider "+req.Provider, http.StatusNotFound)
		return
	}
	if err := s.settings.SetDefaultProvider(req.Provider); err != nil {
		http.Error(w, "failed to set default provider: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// providerRegistered reports whether name is a provider this build knows
// about at all — checked by every handler above before touching it, so a
// typo gets a 404 rather than silently doing nothing.
func (s *Server) providerRegistered(name string) bool {
	for _, n := range s.registry.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// handleGetProviderAccountStatus implements
// GET /api/v1/settings/providers/{name}/account — that provider's own
// account state (plan, expiry, any restriction it has applied).
//
// Per provider rather than one call for the instance: each account has its
// own plan and its own restrictions, and showing one provider's cooldown
// under a heading that could mean either would be worse than showing
// nothing. Always HTTP 200 with available:false on failure, matching
// GET /api/v1/settings/account — a provider being unreachable is a state to
// display, not an error to handle.
func (s *Server) handleGetProviderAccountStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.providerRegistered(name) {
		http.Error(w, "unknown provider "+name, http.StatusNotFound)
		return
	}
	status, err := s.settings.AccountStatus(r.Context(), name)
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

// handleGetProviderTypes implements GET /api/v1/settings/provider-types —
// the implementations this build can construct, for the "add a provider"
// picker. A provider's *name* is free text; its type is not.
func (s *Server) handleGetProviderTypes(w http.ResponseWriter, r *http.Request) {
	types := s.settings.ProviderTypes()
	if types == nil {
		types = []string{}
	}
	writeJSON(w, types)
}

type addProviderRequest struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// APIKey is optional: adding the entry and pasting its key can
	// reasonably be two steps.
	APIKey string `json:"api_key"`
}

// handleAddProvider implements POST /api/v1/settings/providers — registers
// a new provider entry live and persists it.
//
// The name/type split is what allows two accounts on one service: entries
// "torbox" and "torbox-work" both of type "torbox" are independent
// providers, each with its own credentials, listing cache and rate-limit
// backoff. An omitted type means "same as the name", so adding a first
// account needs only a name.
func (s *Server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	var req addProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name must not be empty", http.StatusBadRequest)
		return
	}
	if s.providerRegistered(req.Name) {
		http.Error(w, "provider "+req.Name+" already exists", http.StatusConflict)
		return
	}
	if err := s.settings.AddProvider(r.Context(), req.Name, req.Type, req.APIKey); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleRemoveProvider implements DELETE /api/v1/settings/providers/{name}.
//
// Downloads already tracked against the provider are deliberately left
// alone: they are records of real things, and removing a provider is a
// configuration change rather than an instruction to discard history. They
// simply stop resolving to a provider, which every caller already handles
// by declining to act — see providerFor.
func (s *Server) handleRemoveProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.providerRegistered(name) {
		http.Error(w, "unknown provider "+name, http.StatusNotFound)
		return
	}
	if err := s.settings.RemoveProvider(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
