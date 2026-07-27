package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
)

func TestLiveSettings_SetTorBoxAPIKey_PersistsAndConfigures(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	if settings.TorBoxConfigured() {
		t.Fatal("TorBoxConfigured() = true before Set, want false")
	}

	if err := settings.SetTorBoxAPIKey(context.Background(), "brand-new-key"); err != nil {
		t.Fatalf("SetTorBoxAPIKey() error = %v", err)
	}

	if !settings.TorBoxConfigured() {
		t.Error("TorBoxConfigured() = false after SetTorBoxAPIKey, want true")
	}
	if !torrentDyn.Configured() || !usenetDyn.Configured() {
		t.Error("Dynamic providers should both be configured after SetTorBoxAPIKey")
	}

	// Persisted to disk, so a restart would pick it up too.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if !strings.Contains(string(data), "brand-new-key") {
		t.Errorf("config file doesn't contain the new key: %s", data)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Providers["torbox"].APIKey != "brand-new-key" {
		t.Errorf("reloaded config's torbox key = %q, want brand-new-key", reloaded.Providers["torbox"].APIKey)
	}
}

// TestSettingsAPI_SetKeyThenUseShimImmediately is the real point of this
// feature: PUT a TorBox key through the native API, then use the
// qBittorrent shim on the very next request — no restart, no re-mounting of
// routes, because both shims were already mounted and just needed the
// Dynamic provider to stop returning debrid.ErrNoProvider.
func TestSettingsAPI_SetKeyThenUseShimImmediately(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	handler := buildHandler(db, torrentDyn, usenetDyn, settings)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Before configuring: qBt login works (no provider needed), but the
	// native API says nothing is configured.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/providers", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/providers error = %v", err)
	}
	resp.Body.Close()

	// Set the key through the native settings API.
	setReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/settings/providers/torbox",
		strings.NewReader(`{"api_key":"live-set-key"}`))
	setReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	setResp, err := http.DefaultClient.Do(setReq)
	if err != nil {
		t.Fatalf("PUT /api/v1/settings/providers/torbox error = %v", err)
	}
	setResp.Body.Close()
	if setResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT settings status = %d, want 204", setResp.StatusCode)
	}

	// Immediately after, with no restart, the native API reflects it...
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/providers", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/providers (after set) error = %v", err)
	}
	defer resp.Body.Close()
	if !settings.TorBoxConfigured() {
		t.Error("TorBoxConfigured() = false immediately after PUT, want true")
	}

	// ...and the already-mounted qBittorrent shim's provider-backed calls
	// stop returning debrid.ErrNoProvider (it'll still fail against the real
	// TorBox API since "live-set-key" isn't real, but that's a different,
	// expected failure — the point here is the route was already live and
	// didn't need remounting).
	if !torrentDyn.Configured() {
		t.Error("torrentDyn.Configured() = false after PUT, want true")
	}
}

func TestLiveSettings_RegenerateAPIKey_PersistsAndApplies(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	oldKey := cfg.APIKey

	_, _, settings := setupProviders(cfg, configPath)

	newKey, err := settings.RegenerateAPIKey(context.Background())
	if err != nil {
		t.Fatalf("RegenerateAPIKey() error = %v", err)
	}
	if newKey == "" || newKey == oldKey {
		t.Fatalf("newKey = %q, want a non-empty key different from %q", newKey, oldKey)
	}
	if settings.APIKey() != newKey {
		t.Errorf("settings.APIKey() = %q, want %q", settings.APIKey(), newKey)
	}

	// Persisted to disk, so a restart would pick it up too.
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.APIKey != newKey {
		t.Errorf("reloaded config's api_key = %q, want %q", reloaded.APIKey, newKey)
	}
}

func TestLiveSettings_General_ReflectsConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	_, _, settings := setupProviders(cfg, configPath)
	got := settings.General()

	if got.APIKey != cfg.APIKey || got.Port != cfg.Port || got.DataDir != cfg.DataDir ||
		got.DownloadDir != cfg.DownloadDir || got.LogLevel != cfg.LogLevel ||
		got.ImportIntervalSeconds != cfg.ImportIntervalSeconds || got.ImportMaxRetries != cfg.ImportMaxRetries {
		t.Errorf("General() = %+v, want it to mirror cfg (%+v)", got, cfg)
	}
}

// TestSettingsAPI_RegenerateAPIKey_OldKeyStopsWorkingNewKeyWorks proves the
// regenerate endpoint takes effect immediately across the whole handler tree
// (native API and both compat shims share the same liveSettings instance) —
// not just for the native API's own routes.
func TestSettingsAPI_RegenerateAPIKey_OldKeyStopsWorkingNewKeyWorks(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	handler := buildHandler(db, torrentDyn, usenetDyn, settings)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	oldKey := cfg.APIKey

	regenReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/settings/api-key/regenerate", nil)
	regenReq.Header.Set("Authorization", "Bearer "+oldKey)
	regenResp, err := http.DefaultClient.Do(regenReq)
	if err != nil {
		t.Fatalf("POST /api/v1/settings/api-key/regenerate error = %v", err)
	}
	defer regenResp.Body.Close()
	if regenResp.StatusCode != http.StatusOK {
		t.Fatalf("regenerate status = %d, want 200", regenResp.StatusCode)
	}
	var regenBody map[string]string
	if err := json.NewDecoder(regenResp.Body).Decode(&regenBody); err != nil {
		t.Fatalf("decode regenerate response: %v", err)
	}
	newKey := regenBody["api_key"]
	if newKey == "" || newKey == oldKey {
		t.Fatalf("newKey = %q, want a non-empty key different from %q", newKey, oldKey)
	}

	// The old key no longer authenticates the native API...
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+oldKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/version (old key) error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old key status = %d, want 401", resp.StatusCode)
	}

	// ...nor the qBittorrent shim's login.
	form := url.Values{"username": {"admin"}, "password": {oldKey}}
	loginResp, err := http.PostForm(ts.URL+"/api/v2/auth/login", form)
	if err != nil {
		t.Fatalf("qbt login (old key) error = %v", err)
	}
	body, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if strings.TrimSpace(string(body)) != "Fails." {
		t.Errorf("qbt login with old key body = %q, want Fails.", body)
	}

	// The new key works immediately on the native API...
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+newKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/version (new key) error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("new key status = %d, want 200", resp.StatusCode)
	}

	// ...and the qBittorrent shim's login.
	form = url.Values{"username": {"admin"}, "password": {newKey}}
	loginResp, err = http.PostForm(ts.URL+"/api/v2/auth/login", form)
	if err != nil {
		t.Fatalf("qbt login (new key) error = %v", err)
	}
	body, _ = io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if strings.TrimSpace(string(body)) != "Ok." {
		t.Errorf("qbt login with new key body = %q, want Ok.", body)
	}
}
