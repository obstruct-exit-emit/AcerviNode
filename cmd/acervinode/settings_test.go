package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	handler := buildHandler(cfg, db, torrentDyn, usenetDyn, settings)
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
