package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
)

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	t.Setenv("ACERVINODE_DATA_DIR", t.TempDir())
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

	handler, providers := buildHandler(cfg, db)
	if len(providers) != 1 || providers[0].Name != "torbox" {
		t.Fatalf("providers = %+v, want one torbox entry", providers)
	}

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
}

// TestBuildHandler_NoProviderConfigured proves the compat shims simply don't
// mount (404, not a crash) when no provider is configured.
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

	handler, providers := buildHandler(cfg, db)
	if len(providers) != 0 {
		t.Fatalf("providers = %+v, want none", providers)
	}

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v2/app/webapiVersion")
	if err != nil {
		t.Fatalf("GET /api/v2/app/webapiVersion error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (qbittorrent shim should not be mounted)", resp.StatusCode)
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
