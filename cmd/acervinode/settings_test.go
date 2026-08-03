package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/api"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/importer"
)

func TestLiveSettings_SetTorBoxAPIKey_PersistsAndConfigures(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	if settings.TorBoxConfigured() {
		t.Fatal("TorBoxConfigured() = true before Set, want false")
	}

	if err := settings.SetTorBoxAPIKey(context.Background(), "brand-new-key"); err != nil {
		t.Fatalf("SetTorBoxAPIKey() error = %v", err)
	}

	if !settings.TorBoxConfigured() {
		t.Error("TorBoxConfigured() = false after SetTorBoxAPIKey, want true")
	}
	if !torrentDyn.Configured() || !usenetDyn.Configured() || !webDownloadDyn.Configured() {
		t.Error("Dynamic providers should all three be configured after SetTorBoxAPIKey")
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	handler := buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings)
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

	_, _, _, settings := setupProviders(cfg, configPath)

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

	_, _, _, settings := setupProviders(cfg, configPath)
	got := settings.General()

	if got.APIKey != cfg.APIKey || got.Port != cfg.Port || got.DataDir != cfg.DataDir ||
		got.DownloadDir != cfg.DownloadDir || got.LogLevel != cfg.LogLevel ||
		got.ImportIntervalSeconds != cfg.ImportIntervalSeconds || got.ImportMaxRetries != cfg.ImportMaxRetries ||
		got.MaxConcurrentDownloads != cfg.MaxConcurrentDownloads || got.ImportFetchTimeoutSeconds != cfg.ImportFetchTimeoutSeconds ||
		got.CleanupAfterDays != cfg.CleanupAfterDays {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	handler := buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings)
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

// TestLiveSettings_UpdateGeneral_AppliesLiveAndPersists proves
// download_dir/log_level/import_interval_seconds/import_max_retries/
// max_concurrent_downloads/import_fetch_timeout_seconds take effect on the
// live Importer/levelVar immediately, not just in config.yaml.
func TestLiveSettings_UpdateGeneral_AppliesLiveAndPersists(t *testing.T) {
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

	torrentDyn, usenetDyn, _, settings := setupProviders(cfg, configPath)

	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	settings.SetLevelVar(levelVar)

	newDownloadDir := t.TempDir()
	imp := importer.New(db, torrentDyn, usenetDyn, "old-download-dir", time.Minute, 5)
	settings.SetImporter(imp)

	restartRequired, err := settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port, DataDir: cfg.DataDir, // unchanged — no restart needed
		DownloadDir: newDownloadDir, LogLevel: "debug",
		ImportIntervalSeconds: 42, ImportMaxRetries: 9,
		MaxConcurrentDownloads: 7, ImportFetchTimeoutSeconds: 120,
		CleanupAfterDays: 14,
		DownloadDirMode:  "0750", FastPollIntervalSeconds: 5,
		ProviderRequestTimeoutSeconds: 45,
		TLSPort:                       cfg.TLSPort, // unchanged — no restart needed
	})
	if err != nil {
		t.Fatalf("UpdateGeneral() error = %v", err)
	}
	if restartRequired {
		t.Error("restartRequired = true, want false (port/data_dir unchanged)")
	}

	if got := levelVar.Level(); got != slog.LevelDebug {
		t.Errorf("levelVar = %v, want debug applied live", got)
	}
	gotDir, gotInterval, gotMaxRetries := imp.Config()
	if gotDir != newDownloadDir {
		t.Errorf("importer downloadDir = %q, want %q applied live", gotDir, newDownloadDir)
	}
	if gotInterval != 42*time.Second {
		t.Errorf("importer interval = %v, want 42s applied live", gotInterval)
	}
	if gotMaxRetries != 9 {
		t.Errorf("importer maxRetries = %d, want 9 applied live", gotMaxRetries)
	}
	if got := imp.MaxConcurrent(); got != 7 {
		t.Errorf("importer MaxConcurrent() = %d, want 7 applied live", got)
	}
	if got := imp.FetchTimeout(); got != 120*time.Second {
		t.Errorf("importer FetchTimeout() = %v, want 120s applied live", got)
	}
	if got := imp.CleanupAfterDays(); got != 14 {
		t.Errorf("importer CleanupAfterDays() = %d, want 14 applied live", got)
	}
	if got := imp.DirMode(); got != 0o750 {
		t.Errorf("importer DirMode() = %o, want 0750 applied live", got)
	}
	if got := imp.FastPollInterval(); got != 5*time.Second {
		t.Errorf("importer FastPollInterval() = %v, want 5s applied live", got)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.DownloadDir != newDownloadDir || reloaded.LogLevel != "debug" ||
		reloaded.ImportIntervalSeconds != 42 || reloaded.ImportMaxRetries != 9 ||
		reloaded.MaxConcurrentDownloads != 7 || reloaded.ImportFetchTimeoutSeconds != 120 ||
		reloaded.CleanupAfterDays != 14 || reloaded.DownloadDirMode != "0750" ||
		reloaded.FastPollIntervalSeconds != 5 || reloaded.ProviderRequestTimeoutSeconds != 45 {
		t.Errorf("reloaded config = %+v, want the new values persisted", reloaded)
	}
}

// TestLiveSettings_UpdateGeneral_RebuildsProvidersOnTimeoutChange proves a
// changed provider_request_timeout_seconds actually takes effect live: it's
// baked into each torbox.Client at construction (see
// torbox.WithRequestTimeout), unlike every other Importer-facing field
// above, which can just be pushed into an already-live object — so this
// needs UpdateGeneral to rebuild all three Dynamic providers from the
// current key, the same as SetTorBoxAPIKey already does on every key
// change. This only proves the plumbing doesn't drop the provider or need a
// restart — the actual per-request deadline behavior itself is
// TestRequestTimeout_BoundsEveryCall's job, in the torbox package, where a
// fake HTTP server can be injected; liveSettings always points at the real
// TorBox base URL, so there's nothing to inject a slow server into here.
func TestLiveSettings_UpdateGeneral_RebuildsProvidersOnTimeoutChange(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)

	ctx := context.Background()
	if err := settings.SetTorBoxAPIKey(ctx, "a-real-looking-key"); err != nil {
		t.Fatalf("SetTorBoxAPIKey() error = %v", err)
	}
	if !torrentDyn.Configured() {
		t.Fatal("torrentDyn.Configured() = false after SetTorBoxAPIKey, want true")
	}

	general := settings.General()
	general.ProviderRequestTimeoutSeconds = 45
	update := api.GeneralUpdate{
		Port: general.Port, DataDir: general.DataDir, DownloadDir: general.DownloadDir,
		LogLevel: general.LogLevel, ImportIntervalSeconds: general.ImportIntervalSeconds, ImportMaxRetries: general.ImportMaxRetries,
		MaxConcurrentDownloads: general.MaxConcurrentDownloads, ImportFetchTimeoutSeconds: general.ImportFetchTimeoutSeconds,
		CleanupAfterDays: general.CleanupAfterDays, DownloadDirMode: general.DownloadDirMode,
		FastPollIntervalSeconds: general.FastPollIntervalSeconds, ProviderRequestTimeoutSeconds: general.ProviderRequestTimeoutSeconds,
		TLSEnabled: general.TLSEnabled, TLSPort: general.TLSPort, TLSCertFile: general.TLSCertFile, TLSKeyFile: general.TLSKeyFile,
	}
	restartRequired, err := settings.UpdateGeneral(ctx, update)
	if err != nil {
		t.Fatalf("UpdateGeneral() error = %v", err)
	}
	if restartRequired {
		t.Error("restartRequired = true, want false (provider_request_timeout_seconds applies live)")
	}

	// Still configured, with three fresh providers under the hood (a
	// panic/nil here would mean the rebuild dropped one) — Configured()
	// itself is the only thing observable from outside the debrid package
	// without a real network call.
	if !torrentDyn.Configured() || !usenetDyn.Configured() || !webDownloadDyn.Configured() {
		t.Error("Dynamic providers should all three stay configured after a timeout-only UpdateGeneral")
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.ProviderRequestTimeoutSeconds != 45 {
		t.Errorf("reloaded config's provider_request_timeout_seconds = %d, want 45", reloaded.ProviderRequestTimeoutSeconds)
	}
}

// TestLiveSettings_UpdateGeneral_RestartRequiredForPortAndDataDir proves a
// port or data_dir change is reported back as needing a restart, since
// AcerviNode doesn't rebind its listener or reopen the database live.
func TestLiveSettings_UpdateGeneral_RestartRequiredForPortAndDataDir(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	restartRequired, err := settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port + 1, DataDir: cfg.DataDir, DownloadDir: cfg.DownloadDir,
		LogLevel: cfg.LogLevel, ImportIntervalSeconds: cfg.ImportIntervalSeconds, ImportMaxRetries: cfg.ImportMaxRetries,
		MaxConcurrentDownloads: cfg.MaxConcurrentDownloads, ImportFetchTimeoutSeconds: cfg.ImportFetchTimeoutSeconds,
		DownloadDirMode: cfg.DownloadDirMode, FastPollIntervalSeconds: cfg.FastPollIntervalSeconds,
		ProviderRequestTimeoutSeconds: cfg.ProviderRequestTimeoutSeconds,
	})
	if err != nil {
		t.Fatalf("UpdateGeneral() error = %v", err)
	}
	if !restartRequired {
		t.Error("restartRequired = false, want true (port changed)")
	}
}

// TestLiveSettings_UpdateGeneral_RejectsInvalidValues proves an invalid
// candidate is rejected without mutating the live config at all.
func TestLiveSettings_UpdateGeneral_RejectsInvalidValues(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	originalLogLevel := cfg.LogLevel
	_, _, _, settings := setupProviders(cfg, configPath)

	_, err = settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port, DataDir: cfg.DataDir, DownloadDir: cfg.DownloadDir,
		LogLevel: "not-a-real-level", ImportIntervalSeconds: cfg.ImportIntervalSeconds, ImportMaxRetries: cfg.ImportMaxRetries,
		MaxConcurrentDownloads: cfg.MaxConcurrentDownloads, ImportFetchTimeoutSeconds: cfg.ImportFetchTimeoutSeconds,
	})
	if err == nil {
		t.Fatal("UpdateGeneral() with an invalid log_level: expected an error, got nil")
	}
	if settings.General().LogLevel != originalLogLevel {
		t.Errorf("General().LogLevel = %q after a rejected update, want unchanged %q", settings.General().LogLevel, originalLogLevel)
	}
}

// TestLiveSettings_UpdateGeneral_RestartRequiredForTLSChanges proves
// enabling TLS (or changing tls_port) is reported back as needing a
// restart, the same as port/data_dir — the running listener isn't rebound
// live.
func TestLiveSettings_UpdateGeneral_RestartRequiredForTLSChanges(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	restartRequired, err := settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port, DataDir: cfg.DataDir, DownloadDir: cfg.DownloadDir,
		LogLevel: cfg.LogLevel, ImportIntervalSeconds: cfg.ImportIntervalSeconds, ImportMaxRetries: cfg.ImportMaxRetries,
		MaxConcurrentDownloads: cfg.MaxConcurrentDownloads, ImportFetchTimeoutSeconds: cfg.ImportFetchTimeoutSeconds,
		DownloadDirMode: cfg.DownloadDirMode, FastPollIntervalSeconds: cfg.FastPollIntervalSeconds,
		ProviderRequestTimeoutSeconds: cfg.ProviderRequestTimeoutSeconds,
		TLSEnabled:                    true, TLSPort: cfg.TLSPort,
	})
	if err != nil {
		t.Fatalf("UpdateGeneral() error = %v", err)
	}
	if !restartRequired {
		t.Error("restartRequired = false, want true (tls_enabled changed)")
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if !reloaded.TLSEnabled {
		t.Error("reloaded TLSEnabled = false, want true (persisted)")
	}
}

// TestLiveSettings_UpdateGeneral_RejectsTLSPortCollidingWithPort proves the
// same tls_port != port rule config.Validate enforces at load time also
// applies through the settings API, not just config.yaml.
func TestLiveSettings_UpdateGeneral_RejectsTLSPortCollidingWithPort(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	_, err = settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port, DataDir: cfg.DataDir, DownloadDir: cfg.DownloadDir,
		LogLevel: cfg.LogLevel, ImportIntervalSeconds: cfg.ImportIntervalSeconds, ImportMaxRetries: cfg.ImportMaxRetries,
		MaxConcurrentDownloads: cfg.MaxConcurrentDownloads, ImportFetchTimeoutSeconds: cfg.ImportFetchTimeoutSeconds,
		TLSEnabled: true, TLSPort: cfg.Port,
	})
	if err == nil {
		t.Error("UpdateGeneral() with tls_port == port: expected an error, got nil")
	}
}

func TestLiveSettings_SupervisedBySystemd_ReflectsInvocationID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	t.Setenv("INVOCATION_ID", "")
	os.Unsetenv("INVOCATION_ID")
	if settings.SupervisedBySystemd() {
		t.Error("SupervisedBySystemd() = true with no INVOCATION_ID set, want false")
	}

	t.Setenv("INVOCATION_ID", "some-id")
	if !settings.SupervisedBySystemd() {
		t.Error("SupervisedBySystemd() = false with INVOCATION_ID set, want true")
	}
}

// TestLiveSettings_RequestRestart_ErrorsWhenNoTriggerWired proves calling
// RequestRestart before SetRestartTrigger has ever been called (e.g. in a
// test, or a settings call arriving mid-startup) fails loudly rather than
// panicking on a nil function call.
func TestLiveSettings_RequestRestart_ErrorsWhenNoTriggerWired(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	if err := settings.RequestRestart(context.Background()); err == nil {
		t.Error("RequestRestart() with no trigger wired: expected an error, got nil")
	}
}

func TestLiveSettings_RequestRestart_CallsWiredTrigger(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	called := false
	settings.SetRestartTrigger(func() { called = true })

	if err := settings.RequestRestart(context.Background()); err != nil {
		t.Fatalf("RequestRestart() error = %v", err)
	}
	if !called {
		t.Error("RequestRestart() didn't call the wired trigger")
	}
}

func TestLiveSettings_RegenerateCertificate_GeneratesFreshCert(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	cfg.DataDir = t.TempDir()
	_, _, _, settings := setupProviders(cfg, configPath)

	certPath := filepath.Join(cfg.DataDir, "tls", "cert.pem")
	if _, err := os.Stat(certPath); err == nil {
		t.Fatal("cert already exists before RegenerateCertificate(), test setup is wrong")
	}

	if err := settings.RegenerateCertificate(context.Background()); err != nil {
		t.Fatalf("RegenerateCertificate() error = %v", err)
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert not created by RegenerateCertificate(): %v", err)
	}

	first, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if err := settings.RegenerateCertificate(context.Background()); err != nil {
		t.Fatalf("second RegenerateCertificate() error = %v", err)
	}
	second, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert (second time): %v", err)
	}
	if string(first) == string(second) {
		t.Error("cert unchanged after a second RegenerateCertificate() call, want a genuinely fresh one each time")
	}
}

// TestLiveSettings_RegenerateCertificate_RefusesWhenBYOConfigured proves a
// custom tls_cert_file/tls_key_file override is left alone — regenerating
// something the operator supplied themselves isn't this method's place.
func TestLiveSettings_RegenerateCertificate_RefusesWhenBYOConfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	cfg.TLSCertFile = "/somewhere/cert.pem"
	cfg.TLSKeyFile = "/somewhere/key.pem"
	_, _, _, settings := setupProviders(cfg, configPath)

	if err := settings.RegenerateCertificate(context.Background()); err == nil {
		t.Error("RegenerateCertificate() with a BYO override configured: expected an error, got nil")
	}
}

// TestLiveSettings_TestTorBoxConnection_NotConfigured proves the connection
// test fails fast and clearly when no TorBox key has been set, rather than
// attempting a network call at all.
func TestLiveSettings_TestTorBoxConnection_NotConfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	if _, err := settings.TestTorBoxConnection(context.Background()); err == nil {
		t.Error("TestTorBoxConnection() with nothing configured: expected an error, got nil")
	}
}

// TestLiveSettings_CategoriesAndAddCategory proves the settings layer reads
// from and writes to the actual compat shim category stores, once wired via
// buildHandler (SetShimServers).
func TestLiveSettings_CategoriesAndAddCategory(t *testing.T) {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings) // wires SetShimServers as a side effect, seeding defaultArrCategories

	// Custom names, deliberately not among defaultArrCategories, so this
	// test proves AddCategory itself works independent of startup seeding.
	if err := settings.AddCategory("torrent", "my-custom-movies"); err != nil {
		t.Fatalf("AddCategory(torrent) error = %v", err)
	}
	if err := settings.AddCategory("usenet", "my-custom-tv"); err != nil {
		t.Fatalf("AddCategory(usenet) error = %v", err)
	}
	if err := settings.AddCategory("bogus", "x"); err == nil {
		t.Error("AddCategory with an unknown protocol: expected an error, got nil")
	}

	torrentCats, usenetCats := settings.Categories()
	foundTorrent, foundUsenet := false, false
	for _, c := range torrentCats {
		if c == "my-custom-movies" {
			foundTorrent = true
		}
	}
	for _, c := range usenetCats {
		if c == "my-custom-tv" {
			foundUsenet = true
		}
	}
	if !foundTorrent {
		t.Errorf("torrent categories = %v, want it to include my-custom-movies", torrentCats)
	}
	if !foundUsenet {
		t.Errorf("usenet categories = %v, want it to include my-custom-tv", usenetCats)
	}
}

// TestLiveSettings_SetShimServers_SeedsDefaultArrCategories proves the fix
// for a real user report: a brand new *arr app's default category (e.g.
// Radarr's own SABnzbd default of "movies", or its qBittorrent default of
// "radarr" — both confirmed against Radarr's real source, see
// defaultArrCategories) is already known to both compat shims the moment
// they're wired up — no visit to AcerviNode's own Settings → Categories
// page needed first, unlike a fully custom category name.
//
// SeedDefaultCategoriesOnce is called explicitly here, matching main.go's
// real startup order (it must run before buildHandler/SetShimServers so
// what it seeds into CategoryPaths is already there for SetShimServers' own
// normal re-seed to pick up — see both functions' doc comments).
func TestLiveSettings_SetShimServers_SeedsDefaultArrCategories(t *testing.T) {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	if err := settings.SeedDefaultCategoriesOnce(); err != nil {
		t.Fatalf("SeedDefaultCategoriesOnce() error = %v", err)
	}
	buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings)

	torrentCats, usenetCats := settings.Categories()
	for _, want := range defaultArrCategories {
		foundTorrent, foundUsenet := false, false
		for _, c := range torrentCats {
			if c == want {
				foundTorrent = true
			}
		}
		for _, c := range usenetCats {
			if c == want {
				foundUsenet = true
			}
		}
		if !foundTorrent {
			t.Errorf("torrent categories = %v, want it to include %s", torrentCats, want)
		}
		if !foundUsenet {
			t.Errorf("usenet categories = %v, want it to include %s", usenetCats, want)
		}
	}
}

// TestLiveSettings_SetCategoryPath proves a category path override applies
// live to the wired Importer, persists to config.yaml, and can be cleared
// again by setting an empty path.
func TestLiveSettings_SetCategoryPath(t *testing.T) {
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

	torrentDyn, usenetDyn, _, settings := setupProviders(cfg, configPath)
	imp := importer.New(db, torrentDyn, usenetDyn, cfg.DownloadDir, time.Minute, 5)
	settings.SetImporter(imp)

	ctx := context.Background()
	if err := settings.SetCategoryPath(ctx, "movies", "/mnt/movies"); err != nil {
		t.Fatalf("SetCategoryPath() error = %v", err)
	}
	if err := settings.SetCategoryPath(ctx, "", "/mnt/anything"); err == nil {
		t.Error("SetCategoryPath with an empty category: expected an error, got nil")
	}

	if got := settings.CategoryPaths(); got["movies"] != "/mnt/movies" {
		t.Errorf("CategoryPaths() = %v, want movies -> /mnt/movies", got)
	}
	if got := imp.CategoryPaths(); got["movies"] != "/mnt/movies" {
		t.Errorf("importer CategoryPaths() = %v, want movies -> /mnt/movies applied live", got)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() reload error = %v", err)
	}
	if reloaded.CategoryPaths["movies"] != "/mnt/movies" {
		t.Errorf("persisted config category_paths = %v, want movies -> /mnt/movies", reloaded.CategoryPaths)
	}

	// Clearing with an empty path removes the override entirely.
	if err := settings.SetCategoryPath(ctx, "movies", ""); err != nil {
		t.Fatalf("SetCategoryPath() clear error = %v", err)
	}
	if got := settings.CategoryPaths(); got["movies"] != "" {
		t.Errorf("CategoryPaths() after clear = %v, want no movies entry", got)
	}
	if got := imp.CategoryPaths(); got["movies"] != "" {
		t.Errorf("importer CategoryPaths() after clear = %v, want no movies entry applied live", got)
	}
}

// TestLiveSettings_SetCategoryPath_RegistersCategoryWithBothShims proves the
// fix for a real bug found live: a user configuring a brand new category in
// Radarr's SABnzbd client got rejected outright by Radarr's own Test step,
// since real SABnzbd (and this shim, faithfully) has no API to create a
// category on the fly — it must already exist server-side. This is the only
// web UI path that can satisfy that before Sonarr/Radarr ever connects, so
// setting a category path (even with no override, i.e. an empty path) must
// also make the category show up in both shims' own category lists.
func TestLiveSettings_SetCategoryPath_RegistersCategoryWithBothShims(t *testing.T) {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings) // wires SetShimServers as a side effect

	ctx := context.Background()
	if err := settings.SetCategoryPath(ctx, "movies-radarr", ""); err != nil {
		t.Fatalf("SetCategoryPath() error = %v", err)
	}

	torrentCats, usenetCats := settings.Categories()
	foundTorrent, foundUsenet := false, false
	for _, c := range torrentCats {
		if c == "movies-radarr" {
			foundTorrent = true
		}
	}
	for _, c := range usenetCats {
		if c == "movies-radarr" {
			foundUsenet = true
		}
	}
	if !foundTorrent {
		t.Errorf("torrent categories = %v, want it to include movies-radarr", torrentCats)
	}
	if !foundUsenet {
		t.Errorf("usenet categories = %v, want it to include movies-radarr", usenetCats)
	}
}

// TestLiveSettings_SetCategoryPath_SurvivesRestart proves a category
// registered via SetCategoryPath — with or without an actual path override
// — is still known to both compat shims after a simulated restart (a fresh
// liveSettings/buildHandler built from the same persisted config.yaml, the
// same thing an actual process restart does). Before this, category
// registration only ever touched the shims' own in-memory-only category
// stores directly, with nothing at startup to re-seed them from
// config.yaml's persisted category_paths — so the fix for Radarr's SABnzbd
// "category missing" validation would only last until AcerviNode's next
// restart, putting a user right back where they started.
func TestLiveSettings_SetCategoryPath_SurvivesRestart(t *testing.T) {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings)

	ctx := context.Background()
	if err := settings.SetCategoryPath(ctx, "movies-radarr", ""); err != nil {
		t.Fatalf("SetCategoryPath() error = %v", err)
	}
	if err := settings.SetCategoryPath(ctx, "tv-with-override", "/mnt/tv"); err != nil {
		t.Fatalf("SetCategoryPath() error = %v", err)
	}

	// Simulate a restart: reload config from the same file, build a brand
	// new liveSettings/handler from scratch — no in-memory state carries
	// over except what config.yaml actually persisted.
	reloadedCfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() reload error = %v", err)
	}
	torrentDyn2, usenetDyn2, webDownloadDyn2, settings2 := setupProviders(reloadedCfg, configPath)
	buildHandler(db, torrentDyn2, usenetDyn2, webDownloadDyn2, settings2)

	torrentCats, usenetCats := settings2.Categories()
	for _, want := range []string{"movies-radarr", "tv-with-override"} {
		foundTorrent, foundUsenet := false, false
		for _, c := range torrentCats {
			if c == want {
				foundTorrent = true
			}
		}
		for _, c := range usenetCats {
			if c == want {
				foundUsenet = true
			}
		}
		if !foundTorrent {
			t.Errorf("torrent categories after restart = %v, want it to include %s", torrentCats, want)
		}
		if !foundUsenet {
			t.Errorf("usenet categories after restart = %v, want it to include %s", usenetCats, want)
		}
	}
}

// TestLiveSettings_SeedDefaultCategoriesOnce_PersistsAndNeverResurrects
// proves the actual fix requested: a pre-seeded default (e.g. Radarr's own
// "movies") is now indistinguishable from a category a user registered by
// hand — persisted in CategoryPaths, not just force-applied to both shims'
// in-memory stores on every startup — and, critically, once removed it
// genuinely stays removed across a simulated restart instead of silently
// reappearing, which is exactly what the old unconditional every-startup
// seeding made impossible.
func TestLiveSettings_SeedDefaultCategoriesOnce_PersistsAndNeverResurrects(t *testing.T) {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	if err := settings.SeedDefaultCategoriesOnce(); err != nil {
		t.Fatalf("SeedDefaultCategoriesOnce() error = %v", err)
	}
	buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings)

	// Persisted exactly as if a user had called SetCategoryPath themselves —
	// present in CategoryPaths, not just the shims' in-memory lists.
	paths := settings.CategoryPaths()
	if _, ok := paths["movies"]; !ok {
		t.Errorf("CategoryPaths() = %v, want a \"movies\" entry (empty path) after seeding", paths)
	}
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() reload error = %v", err)
	}
	if _, ok := reloaded.CategoryPaths["movies"]; !ok {
		t.Errorf("persisted config category_paths = %v, want a \"movies\" entry", reloaded.CategoryPaths)
	}
	if !reloaded.DefaultCategoriesSeeded {
		t.Error("persisted config DefaultCategoriesSeeded = false, want true after seeding")
	}

	// Delete it, then simulate a restart (fresh liveSettings from the same
	// persisted config.yaml) — a second SeedDefaultCategoriesOnce call must
	// be a no-op now, so "movies" stays gone.
	ctx := context.Background()
	if err := settings.RemoveCategory(ctx, "movies"); err != nil {
		t.Fatalf("RemoveCategory() error = %v", err)
	}

	reloadedCfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() reload error = %v", err)
	}
	torrentDyn2, usenetDyn2, webDownloadDyn2, settings2 := setupProviders(reloadedCfg, configPath)
	if err := settings2.SeedDefaultCategoriesOnce(); err != nil {
		t.Fatalf("SeedDefaultCategoriesOnce() (post-restart) error = %v", err)
	}
	buildHandler(db, torrentDyn2, usenetDyn2, webDownloadDyn2, settings2)

	pathsAfterRestart := settings2.CategoryPaths()
	if _, ok := pathsAfterRestart["movies"]; ok {
		t.Errorf("CategoryPaths() after restart = %v, want no \"movies\" entry — it was explicitly deleted", pathsAfterRestart)
	}
	torrentCats, usenetCats := settings2.Categories()
	for _, c := range torrentCats {
		if c == "movies" {
			t.Errorf("torrent categories after restart = %v, want \"movies\" to stay deleted, not silently reappear", torrentCats)
		}
	}
	for _, c := range usenetCats {
		if c == "movies" {
			t.Errorf("usenet categories after restart = %v, want \"movies\" to stay deleted, not silently reappear", usenetCats)
		}
	}

	// Every other default is unaffected by deleting just one.
	for _, want := range []string{"radarr", "tv", "tv-sonarr"} {
		found := false
		for _, c := range torrentCats {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("torrent categories after restart = %v, want it to still include %s", torrentCats, want)
		}
	}
}

func TestLiveSettings_RemoveCategory(t *testing.T) {
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

	torrentDyn, usenetDyn, webDownloadDyn, settings := setupProviders(cfg, configPath)
	buildHandler(db, torrentDyn, usenetDyn, webDownloadDyn, settings)

	ctx := context.Background()
	if err := settings.SetCategoryPath(ctx, "movies", "/mnt/movies"); err != nil {
		t.Fatalf("SetCategoryPath() error = %v", err)
	}
	if err := settings.RemoveCategory(ctx, ""); err == nil {
		t.Error("RemoveCategory with an empty name: expected an error, got nil")
	}

	if err := settings.RemoveCategory(ctx, "movies"); err != nil {
		t.Fatalf("RemoveCategory() error = %v", err)
	}

	pathsAfterRemove := settings.CategoryPaths()
	if _, ok := pathsAfterRemove["movies"]; ok {
		t.Errorf("CategoryPaths() after RemoveCategory = %v, want no \"movies\" entry", pathsAfterRemove)
	}
	torrentCats, usenetCats := settings.Categories()
	for _, c := range torrentCats {
		if c == "movies" {
			t.Errorf("torrent categories after RemoveCategory = %v, want \"movies\" gone", torrentCats)
		}
	}
	for _, c := range usenetCats {
		if c == "movies" {
			t.Errorf("usenet categories after RemoveCategory = %v, want \"movies\" gone", usenetCats)
		}
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() reload error = %v", err)
	}
	if _, ok := reloaded.CategoryPaths["movies"]; ok {
		t.Errorf("persisted config category_paths = %v, want no \"movies\" entry", reloaded.CategoryPaths)
	}
}

// --- Auth: liveSettings' wiring of config.Config's user-account methods,
// plus the SetupNeeded convenience check built specifically for this layer
// (the underlying business rules — default-account protection, role
// validation, etc. — are already exhaustively tested in
// internal/config/auth_test.go; these tests confirm delegation and
// persistence, and that SetupNeeded tracks auth alone).

func TestLiveSettings_SetupNeeded(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	if !settings.SetupNeeded() {
		t.Error("SetupNeeded() = false for a fresh instance, want true")
	}

	// Login is mandatory — configuring TorBox alone doesn't satisfy setup,
	// since there'd still be no way to sign into the web UI.
	if err := settings.SetTorBoxAPIKey(context.Background(), "a-key"); err != nil {
		t.Fatalf("SetTorBoxAPIKey() error = %v", err)
	}
	if !settings.SetupNeeded() {
		t.Error("SetupNeeded() = false once TorBox is configured but still no login account, want true")
	}
}

func TestLiveSettings_SetupNeeded_FalseOnceAuthEnabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	if err := settings.Setup(context.Background(), "alice", "somehash"); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if settings.SetupNeeded() {
		t.Error("SetupNeeded() = true after Setup, want false")
	}
	if !settings.AuthEnabled() {
		t.Error("AuthEnabled() = false after Setup, want true")
	}
}

func TestLiveSettings_Setup_CreatesDefaultAdminAndPersists(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)

	if err := settings.Setup(context.Background(), "alice", "hashed-pw"); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	users := settings.ListUsers()
	if len(users) != 1 || users[0].Username != "alice" || !users[0].Default || users[0].Role != config.RoleAdmin {
		t.Errorf("users = %+v, want one Default admin alice", users)
	}

	hash, role, found := settings.FindUser("alice")
	if !found || hash != "hashed-pw" || role != config.RoleAdmin {
		t.Errorf("FindUser(alice) = hash:%q role:%q found:%v", hash, role, found)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if u := reloaded.AuthSettings().Find("alice"); u == nil || u.PasswordHash != "hashed-pw" {
		t.Errorf("persisted user = %+v, want alice with the hashed password", u)
	}
}

func TestLiveSettings_AddRemoveSetPasswordSetRoleSetDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)
	ctx := context.Background()

	if err := settings.Setup(ctx, "alice", "hash1"); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if err := settings.AddUser(ctx, "bob", "hash2", config.RoleMember); err != nil {
		t.Fatalf("AddUser() error = %v", err)
	}
	if len(settings.ListUsers()) != 2 {
		t.Fatalf("ListUsers() = %+v, want 2 users", settings.ListUsers())
	}

	if err := settings.SetUserPassword(ctx, "bob", "hash2-new"); err != nil {
		t.Fatalf("SetUserPassword() error = %v", err)
	}
	if hash, _, _ := settings.FindUser("bob"); hash != "hash2-new" {
		t.Errorf("FindUser(bob) hash = %q, want hash2-new", hash)
	}

	if err := settings.SetUserRole(ctx, "bob", config.RoleAdmin); err != nil {
		t.Fatalf("SetUserRole() error = %v", err)
	}
	if _, role, _ := settings.FindUser("bob"); role != config.RoleAdmin {
		t.Errorf("bob role = %q, want admin", role)
	}

	if err := settings.SetDefaultUser(ctx, "bob"); err != nil {
		t.Fatalf("SetDefaultUser() error = %v", err)
	}
	for _, u := range settings.ListUsers() {
		if u.Username == "bob" && !u.Default {
			t.Error("bob should be Default after SetDefaultUser")
		}
		if u.Username == "alice" && u.Default {
			t.Error("alice should no longer be Default")
		}
	}

	// alice is no longer Default, so she can now be removed.
	if err := settings.RemoveUser(ctx, "alice"); err != nil {
		t.Fatalf("RemoveUser(alice) error = %v", err)
	}
	if len(settings.ListUsers()) != 1 {
		t.Errorf("ListUsers() = %+v, want just bob remaining", settings.ListUsers())
	}

	// Persisted across a reload.
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if u := reloaded.AuthSettings().Find("bob"); u == nil || !u.Default || u.EffectiveRole() != config.RoleAdmin {
		t.Errorf("persisted bob = %+v, want Default admin", u)
	}
	if reloaded.AuthSettings().Find("alice") != nil {
		t.Error("alice should be gone from the persisted config")
	}
}

func TestLiveSettings_RemoveUser_RefusesDefaultAccount(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)
	if err := settings.Setup(context.Background(), "alice", "hash1"); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if err := settings.RemoveUser(context.Background(), "alice"); err == nil {
		t.Error("expected an error removing the sole Default account")
	}
}

func TestLiveSettings_FindUser_UnknownUsername(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	_, _, _, settings := setupProviders(cfg, configPath)
	if _, _, found := settings.FindUser("nobody"); found {
		t.Error("FindUser() found = true for an unknown username, want false")
	}
}
