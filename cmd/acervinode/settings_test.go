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

// TestLiveSettings_UpdateGeneral_AppliesLiveAndPersists proves
// download_dir/log_level/import_interval_seconds/import_max_retries take
// effect on the live Importer/levelVar immediately, not just in config.yaml.
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

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)

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

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.DownloadDir != newDownloadDir || reloaded.LogLevel != "debug" ||
		reloaded.ImportIntervalSeconds != 42 || reloaded.ImportMaxRetries != 9 {
		t.Errorf("reloaded config = %+v, want the new values persisted", reloaded)
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
	_, _, settings := setupProviders(cfg, configPath)

	restartRequired, err := settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port + 1, DataDir: cfg.DataDir, DownloadDir: cfg.DownloadDir,
		LogLevel: cfg.LogLevel, ImportIntervalSeconds: cfg.ImportIntervalSeconds, ImportMaxRetries: cfg.ImportMaxRetries,
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
	_, _, settings := setupProviders(cfg, configPath)

	_, err = settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
		Port: cfg.Port, DataDir: cfg.DataDir, DownloadDir: cfg.DownloadDir,
		LogLevel: "not-a-real-level", ImportIntervalSeconds: cfg.ImportIntervalSeconds, ImportMaxRetries: cfg.ImportMaxRetries,
	})
	if err == nil {
		t.Fatal("UpdateGeneral() with an invalid log_level: expected an error, got nil")
	}
	if settings.General().LogLevel != originalLogLevel {
		t.Errorf("General().LogLevel = %q after a rejected update, want unchanged %q", settings.General().LogLevel, originalLogLevel)
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
	_, _, settings := setupProviders(cfg, configPath)

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

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	buildHandler(db, torrentDyn, usenetDyn, settings) // wires SetShimServers as a side effect

	if err := settings.AddCategory("torrent", "movies"); err != nil {
		t.Fatalf("AddCategory(torrent) error = %v", err)
	}
	if err := settings.AddCategory("usenet", "tv"); err != nil {
		t.Fatalf("AddCategory(usenet) error = %v", err)
	}
	if err := settings.AddCategory("bogus", "x"); err == nil {
		t.Error("AddCategory with an unknown protocol: expected an error, got nil")
	}

	torrentCats, usenetCats := settings.Categories()
	// Both shims seed a default "AcerviNode" category, so "movies" is
	// additive, not the only entry — see internal/qbittorrent's
	// defaultCategory.
	if len(torrentCats) != 2 || torrentCats[0] != "AcerviNode" || torrentCats[1] != "movies" {
		t.Errorf("torrent categories = %v, want [AcerviNode movies] (sorted)", torrentCats)
	}
	found := false
	for _, c := range usenetCats {
		if c == "tv" {
			found = true
		}
	}
	if !found {
		t.Errorf("usenet categories = %v, want it to include tv", usenetCats)
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

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
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
