package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/acervinode/acervinode/internal/api"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/debrid"
	"github.com/acervinode/acervinode/internal/importer"
	"github.com/acervinode/acervinode/internal/qbittorrent"
	"github.com/acervinode/acervinode/internal/sabnzbd"
)

// liveSettings implements api.Settings: it's what lets a TorBox API key, or
// general config, set through the web UI take effect immediately (via the
// Dynamic*Provider wrappers and the Set* methods below, all shared with
// every other consumer — see run() and buildHandler() in main.go) and
// persist across a restart (by rewriting config.yaml).
//
// levelVar/imp/qbt/sab are all set after construction, once run()/
// buildHandler() have built the pieces they point at — setupProviders builds
// liveSettings before any of those exist yet (it's needed to construct them
// in the first place), so these start nil and are wired in via their Set*
// methods. Every method that uses one guards against it still being nil,
// since a handful of tests build a liveSettings without going through the
// full startup sequence.
type liveSettings struct {
	mu             sync.Mutex
	cfg            *config.Config
	configPath     string
	torrentDyn     *debrid.DynamicTorrentProvider
	usenetDyn      *debrid.DynamicUsenetProvider
	webDownloadDyn *debrid.DynamicWebDownloadProvider
	levelVar       *slog.LevelVar
	imp            *importer.Importer
	qbt            *qbittorrent.Server
	sab            *sabnzbd.Server
}

// SetLevelVar wires in the live log-level control built in run() — see
// UpdateGeneral.
func (s *liveSettings) SetLevelVar(levelVar *slog.LevelVar) {
	s.mu.Lock()
	s.levelVar = levelVar
	s.mu.Unlock()
}

// SetImporter wires in the Importer built in run(), once it exists — see
// UpdateGeneral, which calls its SetConfig to apply download_dir/
// import_interval_seconds/import_max_retries changes live. Also pushes
// whatever category path overrides, max_concurrent_downloads,
// import_fetch_timeout_seconds, and cleanup_after_days config.yaml already
// had at startup, so a value set through the UI on a previous run is live
// again immediately, without waiting for another settings call.
func (s *liveSettings) SetImporter(imp *importer.Importer) {
	s.mu.Lock()
	s.imp = imp
	categoryPaths := copyCategoryPaths(s.cfg.CategoryPaths)
	maxConcurrent := s.cfg.MaxConcurrentDownloads
	fetchTimeout := time.Duration(s.cfg.ImportFetchTimeoutSeconds) * time.Second
	cleanupAfterDays := s.cfg.CleanupAfterDays
	s.mu.Unlock()
	imp.SetCategoryPaths(categoryPaths)
	imp.SetMaxConcurrent(maxConcurrent)
	imp.SetFetchTimeout(fetchTimeout)
	imp.SetCleanupAfterDays(cleanupAfterDays)
}

// SetShimServers wires in the compat shim servers built in buildHandler,
// once they exist — see Categories/AddCategory.
func (s *liveSettings) SetShimServers(qbt *qbittorrent.Server, sab *sabnzbd.Server) {
	s.mu.Lock()
	s.qbt = qbt
	s.sab = sab
	s.mu.Unlock()
}

func (s *liveSettings) TorBoxConfigured() bool {
	return s.torrentDyn.Configured()
}

func (s *liveSettings) SetTorBoxAPIKey(_ context.Context, apiKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	torrentProvider, usenetProvider, webDownloadProvider := newTorBoxProviders(apiKey)
	s.torrentDyn.Set(torrentProvider)
	s.usenetDyn.Set(usenetProvider)
	s.webDownloadDyn.Set(webDownloadProvider)

	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]config.ProviderConfig{}
	}
	s.cfg.Providers["torbox"] = config.ProviderConfig{APIKey: apiKey}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

// TestTorBoxConnection makes one real call to TorBox (List, the same call
// both compat shims and internal/importer already make routinely) with the
// currently configured key and times it — a genuine connectivity+auth
// check, not just "is a key set" (see TorBoxConfigured).
func (s *liveSettings) TestTorBoxConnection(ctx context.Context) (int64, error) {
	if !s.torrentDyn.Configured() {
		return 0, fmt.Errorf("torbox is not configured")
	}
	start := time.Now()
	_, err := s.torrentDyn.List(ctx)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return latencyMs, fmt.Errorf("connection test failed: %w", err)
	}
	return latencyMs, nil
}

// APIKey returns AcerviNode's own current API key. This is what every
// authenticated route (native API and both compat shims) checks a request
// against instead of a value captured at startup — see their apiKeySource
// interfaces — so RegenerateAPIKey takes effect everywhere at once.
func (s *liveSettings) APIKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.APIKey
}

// RegenerateAPIKey replaces the current API key with a fresh random one,
// applies it immediately, and persists it to config.yaml — the same
// live-swap-then-save pattern as SetTorBoxAPIKey.
func (s *liveSettings) RegenerateAPIKey(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key, err := config.NewAPIKey()
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	s.cfg.APIKey = key
	if err := s.cfg.Save(s.configPath); err != nil {
		return "", fmt.Errorf("persist config: %w", err)
	}
	return key, nil
}

// General reports the rest of AcerviNode's current configuration for the
// settings UI — see api.GeneralInfo.
func (s *liveSettings) General() api.GeneralInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return api.GeneralInfo{
		APIKey:                    s.cfg.APIKey,
		Port:                      s.cfg.Port,
		DataDir:                   s.cfg.DataDir,
		DownloadDir:               s.cfg.DownloadDir,
		LogLevel:                  s.cfg.LogLevel,
		ImportIntervalSeconds:     s.cfg.ImportIntervalSeconds,
		ImportMaxRetries:          s.cfg.ImportMaxRetries,
		MaxConcurrentDownloads:    s.cfg.MaxConcurrentDownloads,
		ImportFetchTimeoutSeconds: s.cfg.ImportFetchTimeoutSeconds,
		CleanupAfterDays:          s.cfg.CleanupAfterDays,
	}
}

// UpdateGeneral validates update against a copy of the current config (so a
// bad request never corrupts the live one), persists the result, and applies
// whatever can be applied without a restart: log_level (via levelVar),
// download_dir/import_interval_seconds/import_max_retries/
// max_concurrent_downloads/import_fetch_timeout_seconds/cleanup_after_days
// (via the Importer's own SetConfig/SetMaxConcurrent/SetFetchTimeout/
// SetCleanupAfterDays — see internal/importer). port/data_dir are persisted
// too, but binding a new port or reopening the database live is out of
// scope, so a change to either is reported back as restart-required.
func (s *liveSettings) UpdateGeneral(_ context.Context, update api.GeneralUpdate) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	candidate := *s.cfg
	candidate.Port = update.Port
	candidate.DataDir = update.DataDir
	candidate.DownloadDir = update.DownloadDir
	candidate.LogLevel = update.LogLevel
	candidate.ImportIntervalSeconds = update.ImportIntervalSeconds
	candidate.ImportMaxRetries = update.ImportMaxRetries
	candidate.MaxConcurrentDownloads = update.MaxConcurrentDownloads
	candidate.ImportFetchTimeoutSeconds = update.ImportFetchTimeoutSeconds
	candidate.CleanupAfterDays = update.CleanupAfterDays
	if err := candidate.Validate(); err != nil {
		return false, err
	}

	restartRequired := candidate.Port != s.cfg.Port || candidate.DataDir != s.cfg.DataDir

	*s.cfg = candidate
	if err := s.cfg.Save(s.configPath); err != nil {
		return restartRequired, fmt.Errorf("persist config: %w", err)
	}

	if s.levelVar != nil {
		s.levelVar.Set(parseLogLevel(candidate.LogLevel))
	}
	if s.imp != nil {
		s.imp.SetConfig(candidate.DownloadDir, time.Duration(candidate.ImportIntervalSeconds)*time.Second, candidate.ImportMaxRetries)
		s.imp.SetMaxConcurrent(candidate.MaxConcurrentDownloads)
		s.imp.SetFetchTimeout(time.Duration(candidate.ImportFetchTimeoutSeconds) * time.Second)
		s.imp.SetCleanupAfterDays(candidate.CleanupAfterDays)
	}

	return restartRequired, nil
}

// Categories lists every category name each compat shim currently knows
// about — see qbittorrent.Server.Categories/sabnzbd.Server.Categories.
func (s *liveSettings) Categories() (torrent []string, usenet []string) {
	s.mu.Lock()
	qbt, sab := s.qbt, s.sab
	s.mu.Unlock()

	if qbt != nil {
		torrent = qbt.Categories()
	}
	if sab != nil {
		usenet = sab.Categories()
	}
	return torrent, usenet
}

// AddCategory manually registers a category name for the given protocol,
// the same way an *arr app declaring one does.
func (s *liveSettings) AddCategory(protocol, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("category name must not be empty")
	}

	s.mu.Lock()
	qbt, sab := s.qbt, s.sab
	s.mu.Unlock()

	switch protocol {
	case "torrent":
		if qbt == nil {
			return fmt.Errorf("torrent compat shim not available")
		}
		qbt.AddCategory(name)
	case "usenet":
		if sab == nil {
			return fmt.Errorf("usenet compat shim not available")
		}
		sab.AddCategory(name)
	default:
		return fmt.Errorf("unknown protocol %q: must be torrent or usenet", protocol)
	}
	return nil
}

// CategoryPaths reports the current category->override-dir map — see
// config.Config.CategoryPaths.
func (s *liveSettings) CategoryPaths() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyCategoryPaths(s.cfg.CategoryPaths)
}

// SetCategoryPath sets or clears (path == "") category's override
// destination directory, applies it live via the Importer, and persists it
// to config.yaml — the same live-swap-then-save pattern as SetTorBoxAPIKey.
func (s *liveSettings) SetCategoryPath(_ context.Context, category, path string) error {
	category = strings.TrimSpace(category)
	if category == "" {
		return fmt.Errorf("category must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.CategoryPaths == nil {
		s.cfg.CategoryPaths = map[string]string{}
	}
	if path == "" {
		delete(s.cfg.CategoryPaths, category)
	} else {
		s.cfg.CategoryPaths[category] = path
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}

	if s.imp != nil {
		s.imp.SetCategoryPaths(copyCategoryPaths(s.cfg.CategoryPaths))
	}
	return nil
}

// AccountStatus reports the configured TorBox account's own plan/usage —
// see debrid.DynamicTorrentProvider.Account, which this delegates to
// directly (torrentDyn already holds whichever concrete provider is
// currently active, live-swapped the same way as everything else here).
func (s *liveSettings) AccountStatus(ctx context.Context) (debrid.AccountStatus, error) {
	return s.torrentDyn.Account(ctx)
}

func copyCategoryPaths(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
