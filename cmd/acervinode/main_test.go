package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
	registry, settings := setupProviders(cfg, configPath)
	if !settings.ProviderConfigured("torbox") {
		t.Fatal("TorBoxConfigured() = false, want true (key was set via env)")
	}

	handler := buildHandler(db, registry, settings)

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
	registry, settings := setupProviders(cfg, configPath)
	if settings.ProviderConfigured("torbox") {
		t.Fatal("TorBoxConfigured() = true, want false (no key set)")
	}

	handler := buildHandler(db, registry, settings)
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

// waitForOK polls url until it returns 200 or the deadline passes — run()
// starts its listener(s) in a goroutine, so a fresh test has no other signal
// for "the server is actually up yet."
func waitForOK(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned 200: %v", url, lastErr)
}

// TestRun_TLSListensAlongsideHTTP proves tls_enabled adds a second HTTPS
// listener without disturbing the existing plain-HTTP one — dual-listen,
// never a replacement, is the whole point of this design (see
// docs/providers.md's TLS section): existing *arr integrations pointed at
// http://... keep working completely unchanged regardless of whether this
// is turned on.
func TestRun_TLSListensAlongsideHTTP(t *testing.T) {
	t.Setenv("ACERVINODE_DATA_DIR", t.TempDir())
	t.Setenv("ACERVINODE_DOWNLOAD_DIR", t.TempDir())
	httpPort := freePort(t)
	tlsPort := freePort(t)
	t.Setenv("ACERVINODE_PORT", httpPort)
	t.Setenv("ACERVINODE_TLS_ENABLED", "true")
	t.Setenv("ACERVINODE_TLS_PORT", tlsPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(ctx)
	}()

	waitForOK(t, http.DefaultClient, "http://127.0.0.1:"+httpPort+"/api/v1/health")

	// Self-signed — a plain client would reject it, same as a browser would
	// before the user clicks through the warning; this test only cares that
	// the second listener actually serves the same handler over TLS.
	insecureClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	waitForOK(t, insecureClient, "https://127.0.0.1:"+tlsPort+"/api/v1/health")

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

// TestRun_RestartEndpointShutsDownProcess proves POST
// /api/v1/settings/system/restart ends run() the same way an actual
// SIGTERM/SIGINT or a cancelled context already does — see
// liveSettings.SetRestartTrigger, which wires signal.NotifyContext's own
// stop function in as this endpoint's trigger.
func TestRun_RestartEndpointShutsDownProcess(t *testing.T) {
	t.Setenv("ACERVINODE_DATA_DIR", t.TempDir())
	t.Setenv("ACERVINODE_DOWNLOAD_DIR", t.TempDir())
	httpPort := freePort(t)
	t.Setenv("ACERVINODE_PORT", httpPort)
	t.Setenv("ACERVINODE_API_KEY", "test-restart-endpoint-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(ctx)
	}()

	baseURL := "http://127.0.0.1:" + httpPort
	waitForOK(t, http.DefaultClient, baseURL+"/api/v1/health")

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/settings/system/restart", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-restart-endpoint-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart status = %d, want 200", resp.StatusCode)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after the restart endpoint was called")
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

// TestSetupProviders_BuildsRegistryFromConfig covers the wiring that used to
// hardcode "torbox" three times: the registry is now built by walking the
// provider types AcerviNode knows how to construct, and a config entry
// naming something it doesn't know is skipped rather than silently
// producing a provider that can never work.
func TestSetupProviders_BuildsRegistryFromConfig(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"torbox":   {APIKey: "some-key"},
			"nonsense": {APIKey: "x"},
		},
		ProviderRequestTimeoutSeconds: 30,
	}
	registry, _ := setupProviders(cfg, filepath.Join(t.TempDir(), "config.yaml"))

	// Every provider type this build knows is registered, configured or
	// not — the wrapper is the slot a key gets set into. The unrecognised
	// entry is the one that must not appear.
	names := registry.Names()
	if len(names) != len(knownProviders) {
		t.Errorf("Names() = %v, want one entry per known provider type (%d)", names, len(knownProviders))
	}
	for _, n := range names {
		if n == "nonsense" {
			t.Errorf("Names() = %v, want no entry for an unrecognised config name", names)
		}
	}
	// torbox is the only one with a key, so it must be the default even
	// though another provider sorts ahead of it.
	if registry.Default() != "torbox" {
		t.Errorf("Default() = %q, want torbox — the only *configured* provider", registry.Default())
	}
	if p := registry.Torrent("torbox"); p == nil || !p.Configured() {
		t.Error("torbox's torrent wrapper should be configured from the key in config")
	}
	if registry.Torrent("nonsense") != nil {
		t.Error("Torrent(\"nonsense\") returned a wrapper for an unrecognised provider")
	}
}

// A provider with no key yet still has to be registered: the wrapper is the
// slot a key gets set into later through the settings API, so it must exist
// before one is configured.
func TestSetupProviders_RegistersKnownProviderWithNoKey(t *testing.T) {
	cfg := &config.Config{ProviderRequestTimeoutSeconds: 30}
	registry, _ := setupProviders(cfg, filepath.Join(t.TempDir(), "config.yaml"))

	p := registry.Torrent("torbox")
	if p == nil {
		t.Fatal("torbox is not registered when no key is configured")
	}
	if p.Configured() {
		t.Error("Configured() = true with no key in config")
	}
}
