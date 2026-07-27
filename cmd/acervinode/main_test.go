package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
)

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	t.Setenv("ACERVINODE_DATA_DIR", t.TempDir())
	t.Setenv("ACERVINODE_DOWNLOAD_DIR", t.TempDir())
	t.Setenv("ACERVINODE_PORT", freePort(t))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- run(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after context cancellation")
	}
}

// TestBuildHandler_RoutesBothCompatShimsAndNativeAPI proves the full wiring
// works end to end: with a TorBox provider configured, both compat shims
// mount alongside the native /api/v1 routes on one handler.
func TestBuildHandler_RoutesBothCompatShimsAndNativeAPI(t *testing.T) {
	t.Setenv("ACERVINODE_PROVIDERS_TORBOX_API_KEY", "test-torbox-key")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	if !settings.TorBoxConfigured() {
		t.Fatal("TorBoxConfigured() = false, want true (key was set via env)")
	}

	handler := buildHandler(db, torrentDyn, usenetDyn, settings)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Native API health check.
	resp, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/api/v1/health status = %d, want 200", resp.StatusCode)
	}

	// Native API reports the provider as configured.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/providers", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/providers error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "torbox") {
		t.Errorf("/api/v1/providers body = %q, want it to mention torbox", body)
	}

	// qBittorrent shim mounted.
	resp, err = http.Get(ts.URL + "/api/v2/app/webapiVersion")
	if err != nil {
		t.Fatalf("GET /api/v2/app/webapiVersion error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/api/v2/app/webapiVersion status = %d, want 200", resp.StatusCode)
	}

	// SABnzbd shim mounted.
	resp, err = http.Get(ts.URL + "/api?mode=version&apikey=" + cfg.APIKey)
	if err != nil {
		t.Fatalf("GET /api?mode=version error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api?mode=version status = %d, want 200", resp.StatusCode)
	}
	var ver map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&ver); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	if ver["version"] == "" {
		t.Error("sabnzbd version response missing version field")
	}

	// Embedded web UI mounted at the root.
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `<div id="root">`) {
		t.Errorf("GET / status=%d body=%q, want the built index.html", resp.StatusCode, body)
	}
}

// TestBuildHandler_NoProviderConfigured proves both compat shims are still
// mounted (protocol-level probes like "Test" work) even with no TorBox key
// set yet — they just answer provider-dependent calls with a clean error via
// debrid.ErrNoProvider (see internal/debrid's Dynamic*Provider) rather than
// the route not existing at all. That's what makes configuring TorBox later
// through the settings API (rather than only at startup) possible.
func TestBuildHandler_NoProviderConfigured(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer db.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	if settings.TorBoxConfigured() {
		t.Fatal("TorBoxConfigured() = true, want false (no key set)")
	}

	handler := buildHandler(db, torrentDyn, usenetDyn, settings)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// The qBittorrent shim is mounted and answers its protocol-level probe
	// normally — this doesn't touch the provider at all.
	resp, err := http.Get(ts.URL + "/api/v2/app/webapiVersion")
	if err != nil {
		t.Fatalf("GET /api/v2/app/webapiVersion error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) == "" {
		t.Errorf("webapiVersion status=%d body=%q, want a real (unconditional) response", resp.StatusCode, body)
	}

	// The native API correctly reports nothing configured.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/providers", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/providers error = %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Errorf("/api/v1/providers body = %q, want [] (nothing configured yet)", got)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	return port
}
