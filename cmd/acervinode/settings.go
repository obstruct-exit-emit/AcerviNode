package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/acervinode/acervinode/internal/api"
	"github.com/acervinode/acervinode/internal/backup"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
	"github.com/acervinode/acervinode/internal/importer"
	"github.com/acervinode/acervinode/internal/qbittorrent"
	"github.com/acervinode/acervinode/internal/sabnzbd"
	"github.com/acervinode/acervinode/internal/tlscert"
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
	mu         sync.Mutex
	cfg        *config.Config
	configPath string
	// registry is every configured provider. The TorBox-specific methods
	// below look their wrappers up by name, which is honest while those
	// methods are themselves TorBox-specific — generalising both to
	// "whichever provider the caller means" is its own piece of work.
	registry       *debrid.Registry
	levelVar       *slog.LevelVar
	imp            *importer.Importer
	backups        *backup.Runner
	qbt            *qbittorrent.Server
	sab            *sabnzbd.Server
	restartTrigger func()
}

// SetLevelVar wires in the live log-level control built in run() — see
// UpdateGeneral.
func (s *liveSettings) SetLevelVar(levelVar *slog.LevelVar) {
	s.mu.Lock()
	s.levelVar = levelVar
	s.mu.Unlock()
}

// SetRestartTrigger wires in run()'s signal.NotifyContext stop function —
// calling it marks the same context Done that an actual SIGTERM/SIGINT
// would, so RequestRestart needs no shutdown logic of its own; it reuses
// run()'s existing select/Shutdown path.
func (s *liveSettings) SetRestartTrigger(trigger func()) {
	s.mu.Lock()
	s.restartTrigger = trigger
	s.mu.Unlock()
}

// SetImporter wires in the Importer built in run(), once it exists — see
// UpdateGeneral, which calls its SetConfig to apply download_dir/
// import_interval_seconds/import_max_retries changes live. Also pushes
// whatever category path overrides, max_concurrent_downloads,
// import_fetch_timeout_seconds, cleanup_after_days, download_dir_mode,
// fast_poll_interval_seconds, the file-filtering settings,
// stuck_download_timeout_minutes, and cleanup_error_after_days config.yaml
// already had at startup, so a value set through the UI on a previous run
// is live again immediately, without waiting for another settings call.
func (s *liveSettings) SetImporter(imp *importer.Importer) {
	s.mu.Lock()
	s.imp = imp
	categoryPaths := copyCategoryPaths(s.cfg.CategoryPaths)
	maxConcurrent := s.cfg.MaxConcurrentDownloads
	fetchTimeout := time.Duration(s.cfg.ImportFetchTimeoutSeconds) * time.Second
	cleanupAfterDays := s.cfg.CleanupAfterDays
	// ParseDirMode was already validated by config.Load/Validate before
	// this could ever be reached — an error here would mean config.yaml was
	// hand-edited to something invalid after that, or a bug in Validate
	// itself; falling back to the compiled-in default rather than panicking
	// keeps a fresh install-adjacent, easily-recoverable failure mode.
	dirMode, err := config.ParseDirMode(s.cfg.DownloadDirMode)
	if err != nil {
		slog.Error("settings: invalid persisted download_dir_mode, falling back to default", "value", s.cfg.DownloadDirMode, "error", err)
		dirMode = 0o777
	}
	fastPollInterval := time.Duration(s.cfg.FastPollIntervalSeconds) * time.Second
	minFetchFileSizeBytes := s.cfg.MinFetchFileSizeBytes
	maxFetchFileSizeBytes := s.cfg.MaxFetchFileSizeBytes
	includeFileRegex := compileOptionalRegex(s.cfg.IncludeFileRegex)
	excludeFileRegex := compileOptionalRegex(s.cfg.ExcludeFileRegex)
	stuckDownloadTimeout := time.Duration(s.cfg.StuckDownloadTimeoutMinutes) * time.Minute
	cleanupErrorAfterDays := s.cfg.CleanupErrorAfterDays
	s.mu.Unlock()
	imp.SetCategoryPaths(categoryPaths)
	imp.SetMaxConcurrent(maxConcurrent)
	imp.SetFetchTimeout(fetchTimeout)
	imp.SetCleanupAfterDays(cleanupAfterDays)
	imp.SetDirMode(dirMode)
	imp.SetFastPollInterval(fastPollInterval)
	imp.SetFileFilters(minFetchFileSizeBytes, maxFetchFileSizeBytes, includeFileRegex, excludeFileRegex)
	imp.SetStuckDownloadTimeout(stuckDownloadTimeout)
	imp.SetCleanupErrorAfterDays(cleanupErrorAfterDays)
}

// SetShimServers wires in the compat shim servers built in buildHandler,
// once they exist — see Categories/AddCategory.
//
// Also re-seeds both shims' category stores from the persisted
// category_paths keys — those stores are purely in-memory bookkeeping (see
// qbittorrent/sabnzbd's own categoryStore), so without this, a category
// registered via SetCategoryPath (the fix for Radarr's SABnzbd "category
// missing" validation — see docs/sabnzbd-api.md#categories) would vanish on
// every restart, even though its path override survived in config.yaml —
// putting the user right back where they started until something else
// happened to re-declare it. This is also what makes every well-known
// *arr-app default category name show up here with zero extra code: see
// SeedDefaultCategoriesOnce, called once at startup (main.go's run(), before
// this), which folds defaultArrCategories into CategoryPaths itself exactly
// as if a user had registered each one by hand — this loop then re-seeds
// them into both shims' in-memory stores the same as anything else in
// CategoryPaths, no separate special-casing needed here anymore.
func (s *liveSettings) SetShimServers(qbt *qbittorrent.Server, sab *sabnzbd.Server) {
	s.mu.Lock()
	s.qbt = qbt
	s.sab = sab
	categories := copyCategoryPaths(s.cfg.CategoryPaths)
	s.mu.Unlock()

	for category := range categories {
		qbt.AddCategory(category)
		sab.AddCategory(category)
	}
}

// SeedDefaultCategoriesOnce folds every well-known *arr-app default category
// name (defaultArrCategories) into CategoryPaths, with an empty path
// override — exactly as if a user had registered each one by hand via
// SetCategoryPath — but only the very first time this has ever run for this
// instance (guarded by DefaultCategoriesSeeded). Every later startup is a
// no-op.
//
// This used to be unconditional, every single startup, applied straight to
// both compat shims' in-memory category stores — meaning a default a user
// explicitly deleted would always silently come back on the next restart,
// with no way to actually get rid of one. Persisting them into CategoryPaths
// exactly once makes them indistinguishable from anything else a user has
// registered from here on: editable, deletable, and — once deleted — gone
// for good, the same "seed once, never resurrect" shape as
// database.discoverySeeded uses for Manual-download discovery.
//
// Called once from main.go's run(), before buildHandler/SetShimServers, so
// whatever this seeds is already in CategoryPaths by the time
// SetShimServers does its own normal re-seed of both shims from it — no
// separate seeding path needed there anymore.
func (s *liveSettings) SeedDefaultCategoriesOnce() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.DefaultCategoriesSeeded {
		return nil
	}

	if s.cfg.CategoryPaths == nil {
		s.cfg.CategoryPaths = map[string]string{}
	}
	for _, category := range defaultArrCategories {
		if _, exists := s.cfg.CategoryPaths[category]; !exists {
			s.cfg.CategoryPaths[category] = ""
		}
	}
	s.cfg.DefaultCategoriesSeeded = true

	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

// ProviderConfigured reports whether the named provider currently holds
// credentials. A provider that isn't registered reports false rather than
// erroring: the settings API already 404s an unknown name before reaching
// here, and "not configured" is the honest answer for anything else.
func (s *liveSettings) ProviderConfigured(name string) bool {
	if t := s.registry.Torrent(name); t != nil {
		return t.Configured()
	}
	if u := s.registry.Usenet(name); u != nil {
		return u.Configured()
	}
	if w := s.registry.WebDL(name); w != nil {
		return w.Configured()
	}
	return false
}

// SetProviderAPIKey applies a key to the named provider live and persists
// it. An empty key clears the credentials, leaving the provider registered
// so it can be configured again without a restart.
//
// Building the concrete providers is per-provider-type work, which is why
// this goes through knownProviders rather than doing it inline: adding a
// second debrid service means an entry there and nothing here.
func (s *liveSettings) SetProviderAPIKey(_ context.Context, name, apiKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The entry's *type* decides how to build it, which is not always its
	// name — two accounts on one service are two entries sharing a type.
	construct, known := knownProviders[s.cfg.Providers[name].ResolvedType(name)]
	if !known {
		return fmt.Errorf("unknown provider %s", name)
	}

	var torrentProvider debrid.TorrentProvider
	var usenetProvider debrid.UsenetProvider
	var webDownloadProvider debrid.WebDownloadProvider
	if apiKey != "" {
		torrentProvider, usenetProvider, webDownloadProvider = construct(name, apiKey, time.Duration(s.cfg.ProviderRequestTimeoutSeconds)*time.Second)
	}
	if t := s.registry.Torrent(name); t != nil {
		t.Set(torrentProvider)
	}
	if u := s.registry.Usenet(name); u != nil {
		u.Set(usenetProvider)
	}
	if w := s.registry.WebDL(name); w != nil {
		w.Set(webDownloadProvider)
	}

	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]config.ProviderConfig{}
	}
	if apiKey == "" {
		// Cleared in place, not deleted. Removing the entry threw away
		// everything else it held: a second account lost its type and so
		// vanished entirely on the next restart, and any disabled kinds
		// came silently back on — while the running process still showed
		// them off, so the UI and config.yaml disagreed until a restart
		// resolved it the wrong way.
		//
		// Clearing a key means "unconfigure this", which is what the entry
		// with no key now says. Deleting the entry is what
		// DELETE /api/v1/settings/providers/{name} is for.
		entry := s.cfg.Providers[name]
		entry.APIKey = ""
		s.cfg.Providers[name] = entry
	} else {
		// Updated in place rather than replaced, so the entry keeps its
		// Type. Writing a fresh ProviderConfig here dropped it, which broke
		// exactly the case the field exists for: a second account on one
		// service (type: torbox under some other name) survived the change
		// in memory but lost its type in config.yaml, so the next restart
		// resolved the type from the entry's own name, found no such
		// provider, and the account silently vanished.
		entry := s.cfg.Providers[name]
		entry.APIKey = apiKey
		s.cfg.Providers[name] = entry
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	// Same reasoning as AddProvider: pasting a key that another entry
	// already uses is easiest to do here, and easiest to miss.
	warnOnDuplicateProviderKeys(s.cfg)
	return nil
}

// TestProviderConnection makes one real call to the named provider (List,
// the same call both compat shims and internal/importer already make
// routinely) with its currently configured key and times it — a genuine
// connectivity+auth check, not just "is a key set" (see ProviderConfigured).
//
// Uses whichever kind that provider supports, so a provider that doesn't do
// torrents is still testable.
func (s *liveSettings) TestProviderConnection(ctx context.Context, name string) (int64, error) {
	start := time.Now()
	var err error
	switch {
	case s.registry.Torrent(name) != nil:
		t := s.registry.Torrent(name)
		if !t.Configured() {
			return 0, fmt.Errorf("%s is not configured", name)
		}
		_, err = t.List(ctx)
	case s.registry.Usenet(name) != nil:
		u := s.registry.Usenet(name)
		if !u.Configured() {
			return 0, fmt.Errorf("%s is not configured", name)
		}
		_, err = u.List(ctx)
	case s.registry.WebDL(name) != nil:
		w := s.registry.WebDL(name)
		if !w.Configured() {
			return 0, fmt.Errorf("%s is not configured", name)
		}
		_, err = w.List(ctx)
	default:
		return 0, fmt.Errorf("unknown provider %s", name)
	}
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return latencyMs, err
	}
	return latencyMs, nil
}

// DefaultProvider is which provider a new download goes to when nothing
// says otherwise. Reads through the registry rather than the config field,
// so an unset (or since-removed) name reports the fallback that is actually
// in effect instead of an empty string.
func (s *liveSettings) DefaultProvider() string {
	return s.registry.Default()
}

// SetDefaultProvider changes it live and persists it.
func (s *liveSettings) SetDefaultProvider(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.registry.SetDefault(name)
	s.cfg.DefaultProvider = name
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
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

// DownloadDir returns AcerviNode's current fallback download directory — see
// internal/qbittorrent's settingsSource, which reports it as the qBittorrent
// Web API's own save_path preference (GET /api/v2/app/preferences). Sonarr's
// download-client Test() reads this first, before any of its other checks
// (QBittorrentProxyV2.GetConfig) — AcerviNode never implemented this
// endpoint at all, which is why "Test" failed outright over plain HTTP,
// found live.
func (s *liveSettings) DownloadDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.DownloadDir
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
		APIKey:                        s.cfg.APIKey,
		Port:                          s.cfg.Port,
		DataDir:                       s.cfg.DataDir,
		DownloadDir:                   s.cfg.DownloadDir,
		LogLevel:                      s.cfg.LogLevel,
		ImportIntervalSeconds:         s.cfg.ImportIntervalSeconds,
		ImportMaxRetries:              s.cfg.ImportMaxRetries,
		MaxConcurrentDownloads:        s.cfg.MaxConcurrentDownloads,
		ImportFetchTimeoutSeconds:     s.cfg.ImportFetchTimeoutSeconds,
		CleanupAfterDays:              s.cfg.CleanupAfterDays,
		DownloadDirMode:               s.cfg.DownloadDirMode,
		FastPollIntervalSeconds:       s.cfg.FastPollIntervalSeconds,
		ProviderRequestTimeoutSeconds: s.cfg.ProviderRequestTimeoutSeconds,
		TLSEnabled:                    s.cfg.TLSEnabled,
		TLSPort:                       s.cfg.TLSPort,
		TLSCertFile:                   s.cfg.TLSCertFile,
		TLSKeyFile:                    s.cfg.TLSKeyFile,
		MinFetchFileSizeBytes:         s.cfg.MinFetchFileSizeBytes,
		MaxFetchFileSizeBytes:         s.cfg.MaxFetchFileSizeBytes,
		IncludeFileRegex:              s.cfg.IncludeFileRegex,
		ExcludeFileRegex:              s.cfg.ExcludeFileRegex,
		StuckDownloadTimeoutMinutes:   s.cfg.StuckDownloadTimeoutMinutes,
		CleanupErrorAfterDays:         s.cfg.CleanupErrorAfterDays,
		BackupIntervalHours:           s.cfg.BackupIntervalHours,
		BackupKeep:                    s.cfg.BackupKeep,
	}
}

// compileOptionalRegex compiles pattern, or returns nil for an empty one —
// the shape importer.Importer.SetFileFilters wants (nil disables that
// check). Only ever called with a pattern that already passed
// config.Config.Validate's own regexp.Compile check (UpdateGeneral above,
// SetImporter's own startup wiring below), so a compile failure here would
// mean config.yaml was hand-edited to something invalid after that, or a
// bug in Validate itself; falling back to nil (no filtering) rather than
// panicking keeps a fresh install-adjacent, easily-recoverable failure mode
// — the same reasoning SetImporter already applies to download_dir_mode.
func compileOptionalRegex(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		slog.Error("settings: invalid persisted file-filter regex, disabling that filter", "pattern", pattern, "error", err)
		return nil
	}
	return re
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
	candidate.DownloadDirMode = update.DownloadDirMode
	candidate.FastPollIntervalSeconds = update.FastPollIntervalSeconds
	candidate.ProviderRequestTimeoutSeconds = update.ProviderRequestTimeoutSeconds
	candidate.TLSEnabled = update.TLSEnabled
	candidate.TLSPort = update.TLSPort
	candidate.TLSCertFile = update.TLSCertFile
	candidate.TLSKeyFile = update.TLSKeyFile
	candidate.MinFetchFileSizeBytes = update.MinFetchFileSizeBytes
	candidate.MaxFetchFileSizeBytes = update.MaxFetchFileSizeBytes
	candidate.IncludeFileRegex = update.IncludeFileRegex
	candidate.ExcludeFileRegex = update.ExcludeFileRegex
	candidate.StuckDownloadTimeoutMinutes = update.StuckDownloadTimeoutMinutes
	candidate.CleanupErrorAfterDays = update.CleanupErrorAfterDays
	candidate.BackupIntervalHours = update.BackupIntervalHours
	candidate.BackupKeep = update.BackupKeep
	if err := candidate.Validate(); err != nil {
		return false, err
	}

	restartRequired := candidate.Port != s.cfg.Port || candidate.DataDir != s.cfg.DataDir ||
		candidate.TLSEnabled != s.cfg.TLSEnabled || candidate.TLSPort != s.cfg.TLSPort ||
		candidate.TLSCertFile != s.cfg.TLSCertFile || candidate.TLSKeyFile != s.cfg.TLSKeyFile

	// Rebuilding the providers needs the current TorBox key, which is only
	// ever stored in cfg.Providers, not part of this candidate/update at
	// all — captured before *s.cfg is overwritten below.
	requestTimeoutChanged := candidate.ProviderRequestTimeoutSeconds != s.cfg.ProviderRequestTimeoutSeconds

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
		if dirMode, err := config.ParseDirMode(candidate.DownloadDirMode); err == nil {
			s.imp.SetDirMode(dirMode)
		}
		s.imp.SetFastPollInterval(time.Duration(candidate.FastPollIntervalSeconds) * time.Second)
		s.imp.SetFileFilters(candidate.MinFetchFileSizeBytes, candidate.MaxFetchFileSizeBytes, compileOptionalRegex(candidate.IncludeFileRegex), compileOptionalRegex(candidate.ExcludeFileRegex))
		s.imp.SetStuckDownloadTimeout(time.Duration(candidate.StuckDownloadTimeoutMinutes) * time.Minute)
		s.imp.SetCleanupErrorAfterDays(candidate.CleanupErrorAfterDays)
	}
	// Retunes the schedule live, so a changed interval takes effect without
	// waiting out the old one.
	if s.backups != nil {
		s.backups.SetConfig(time.Duration(candidate.BackupIntervalHours)*time.Hour, candidate.BackupKeep)
	}
	// Unlike every other field above, a changed provider request timeout
	// can't just be pushed into an existing live object — it's baked into
	// each provider client at construction (see torbox.WithRequestTimeout).
	// Rebuilding from the current key is the same thing SetProviderAPIKey
	// already does on every key change; this just does it for a
	// timeout-only change too, so it doesn't need its own restart. Every
	// configured provider is rebuilt, not just one, since the timeout is
	// global.
	if requestTimeoutChanged {
		timeout := time.Duration(candidate.ProviderRequestTimeoutSeconds) * time.Second
		for name, pc := range s.cfg.Providers {
			construct, known := knownProviders[pc.ResolvedType(name)]
			if !known || pc.APIKey == "" {
				continue
			}
			torrentProvider, usenetProvider, webDownloadProvider := construct(name, pc.APIKey, timeout)
			if t := s.registry.Torrent(name); t != nil {
				t.Set(torrentProvider)
			}
			if u := s.registry.Usenet(name); u != nil {
				u.Set(usenetProvider)
			}
			if w := s.registry.WebDL(name); w != nil {
				w.Set(webDownloadProvider)
			}
		}
	}

	return restartRequired, nil
}

// SupervisedBySystemd reports whether this process is running under systemd
// unit supervision — systemd sets INVOCATION_ID for every unit it starts.
// RequestRestart still restarts either way (this isn't a permission check),
// but the caller uses this to tell an admin the truth: without a supervisor
// actually watching this process, a clean exit is just a stop, not a
// restart — nothing brings it back.
func (s *liveSettings) SupervisedBySystemd() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

// RequestRestart triggers the same graceful shutdown run()'s signal handling
// already does — see SetRestartTrigger. Config changes that need a restart
// (port, tls_enabled/tls_port, ...) are already persisted to config.yaml by
// the time this is ever called, so the next process start picks them up
// automatically; this method's only job is ending the current one.
func (s *liveSettings) RequestRestart(_ context.Context) error {
	s.mu.Lock()
	trigger := s.restartTrigger
	s.mu.Unlock()
	if trigger == nil {
		return fmt.Errorf("restart trigger not wired up")
	}
	trigger()
	return nil
}

// RegenerateCertificate forces a fresh self-signed TLS certificate — the fix
// for a cert whose baked-in SANs (IPs/hostname captured at generation time)
// no longer match how the instance is actually reached, e.g. after a DHCP
// lease change. Requires a restart to take effect: the running HTTPS
// listener already has the old cert loaded in memory. Refused when a BYO
// tls_cert_file/tls_key_file override is configured — regenerating something
// the operator supplied themselves isn't this method's place.
func (s *liveSettings) RegenerateCertificate(_ context.Context) error {
	s.mu.Lock()
	dataDir := s.cfg.DataDir
	hasOverride := s.cfg.TLSCertFile != "" || s.cfg.TLSKeyFile != ""
	s.mu.Unlock()

	if hasOverride {
		return fmt.Errorf("a custom tls_cert_file/tls_key_file is configured — remove it to use an auto-generated certificate")
	}
	_, _, err := tlscert.RegenerateCertificate(dataDir, tlscert.CollectHosts())
	return err
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
//
// The category itself is always persisted in CategoryPaths, even with an
// empty path — deliberately not deleted the way it used to be when path ==
// "": internal/importer's own categoryPath already treats an empty value
// identically to an absent key (dir != "" is required either way for an
// override to actually apply), so this costs nothing functionally, but it's
// what makes a bare "just register this category" registration (no override
// wanted) survive a restart, the same as one with a real override always
// did — CategoryPaths is the only thing SetShimServers has to re-seed both
// compat shims' in-memory category stores from at startup (see its own doc
// comment). Also registers category with both shims live, right now, not
// just at the next startup — the only way to pre-declare one from the web
// UI without a real Sonarr/Radarr connection ever having done so first.
// This matters specifically for the SABnzbd shim: real SABnzbd has no API
// to create a category on the fly (unlike qBittorrent's createCategory,
// which Sonarr/Radarr's own Test() already calls automatically for a
// missing one — see internal/qbittorrent's handleCreateCategory), so
// Sonarr/Radarr's SABnzbd TestCategory() genuinely requires the category to
// already exist server-side, exactly like it would against a real SABnzbd
// install — found live: a user configuring a brand new category in
// Radarr's SABnzbd client got rejected outright, since nothing had ever
// told AcerviNode about it yet (see docs/sabnzbd-api.md#categories).
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
	s.cfg.CategoryPaths[category] = path
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}

	if s.imp != nil {
		s.imp.SetCategoryPaths(copyCategoryPaths(s.cfg.CategoryPaths))
	}
	if s.qbt != nil {
		s.qbt.AddCategory(category)
	}
	if s.sab != nil {
		s.sab.AddCategory(category)
	}
	return nil
}

// RemoveCategory forgets a category entirely — its path override (if any)
// and its registration with both compat shims — so it stops showing up
// anywhere and, unlike clearing an override's path (SetCategoryPath's own
// "clear it but stay registered" behavior), a deleted pre-seeded default
// (see SeedDefaultCategoriesOnce) genuinely won't come back on its own. If
// Sonarr/Radarr is still actively configured with this category, declaring
// it again (e.g. its own "Test" step, or a later add) re-registers it with
// the compat shims exactly the same as it would against a real install —
// this only forgets what's known right now, it doesn't block it forever.
func (s *liveSettings) RemoveCategory(_ context.Context, category string) error {
	category = strings.TrimSpace(category)
	if category == "" {
		return fmt.Errorf("category must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.cfg.CategoryPaths, category)
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}

	if s.imp != nil {
		s.imp.SetCategoryPaths(copyCategoryPaths(s.cfg.CategoryPaths))
	}
	if s.qbt != nil {
		s.qbt.RemoveCategory(category)
	}
	if s.sab != nil {
		s.sab.RemoveCategory(category)
	}
	return nil
}

// DeleteLocalFiles removes d's local files from disk, if any — delegates
// straight to the Importer, the only place that knows how to resolve a
// download's actual destination directory (download_dir, category
// overrides, an explicit save_path) live. A nil imp (a handful of tests
// build liveSettings without full startup wiring) is a routine no-op, not
// an error — nothing has ever been fetched to disk in that case either.
func (s *liveSettings) DeleteLocalFiles(d *database.Download) error {
	s.mu.Lock()
	imp := s.imp
	s.mu.Unlock()
	if imp == nil {
		return nil
	}
	return imp.RemoveLocalFiles(d)
}

// CancelFetch interrupts the Importer's in-flight fetch for id, if any —
// see api.Settings.CancelFetch's own doc comment. A nil imp (tests, or a
// moment before run() finishes wiring it in) is a routine no-op, same
// treatment as DeleteLocalFiles above.
func (s *liveSettings) CancelFetch(id string) {
	s.mu.Lock()
	imp := s.imp
	s.mu.Unlock()
	if imp == nil {
		return
	}
	imp.CancelFetch(id)
}

// AccountStatus reports the configured TorBox account's own plan/usage —
// see debrid.DynamicTorrentProvider.Account, which this delegates to
// directly (the wrapper already holds whichever concrete provider is
// currently active, live-swapped the same way as everything else here).
// Reported per provider: each account has its own plan, expiry and
// restrictions, so a single instance-wide panel could only ever show one of
// them under a heading that might mean either.
//
// Tries whichever kind the provider supports, so one that doesn't do
// torrents is still reportable — a provider's account is a property of the
// account, not of any particular kind.
func (s *liveSettings) AccountStatus(ctx context.Context, provider string) (debrid.AccountStatus, error) {
	if t := s.registry.Torrent(provider); t != nil {
		return t.Account(ctx)
	}
	if u := s.registry.Usenet(provider); u != nil {
		return u.Account(ctx)
	}
	if w := s.registry.WebDL(provider); w != nil {
		return w.Account(ctx)
	}
	return debrid.AccountStatus{}, debrid.ErrNoProvider
}

// SetBackupRunner wires in the backup scheduler once run() has built it —
// same late-binding as SetImporter, since liveSettings is constructed first.
func (s *liveSettings) SetBackupRunner(r *backup.Runner) {
	s.mu.Lock()
	s.backups = r
	s.mu.Unlock()
}

// RunBackupNow takes a snapshot immediately and returns the file written.
func (s *liveSettings) RunBackupNow(ctx context.Context) (string, error) {
	s.mu.Lock()
	r := s.backups
	s.mu.Unlock()
	if r == nil {
		return "", fmt.Errorf("backups are not running")
	}
	return r.RunOnce(ctx)
}

// Backups lists the snapshots currently on disk, newest first.
func (s *liveSettings) Backups() ([]api.BackupInfo, error) {
	s.mu.Lock()
	r := s.backups
	s.mu.Unlock()
	if r == nil {
		return nil, nil
	}
	snaps, err := r.List()
	if err != nil {
		return nil, err
	}
	out := make([]api.BackupInfo, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, api.BackupInfo{
			Name:      sn.Name,
			SizeBytes: sn.SizeBytes,
			TakenAt:   sn.TakenAt,
		})
	}
	return out, nil
}

// ProviderTypes lists the provider implementations this build can
// construct, for the settings UI's "add a provider" picker — a name is free
// text, but the type has to be one of these.
func (s *liveSettings) ProviderTypes() []string {
	return knownProviderTypes()
}

// ProviderType is which implementation an entry uses. Config only records a
// type when it differs from the name, so an entry without one is its own
// type — which is what a first account looks like.
func (s *liveSettings) ProviderType(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pc, ok := s.cfg.Providers[name]; ok {
		return pc.ResolvedType(name)
	}
	return name
}

// ProviderSupportedKinds reports which kinds this entry's *service* can do,
// independent of whether they are switched on — see
// knownProviderCapabilities, which is the static truth about a provider
// type. The registry only holds the kinds that are both supported and
// enabled, so it cannot answer this on its own.
func (s *liveSettings) ProviderSupportedKinds(name string) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	caps := knownProviderCapabilities[s.cfg.Providers[name].ResolvedType(name)]
	return map[string]bool{
		"torrent": caps.torrent,
		"usenet":  caps.usenet,
		"webdl":   caps.webdl,
	}
}

// SetProviderKinds switches kinds on or off for one provider, live and
// persisted. Only the kinds present in enabled are changed.
//
// Refuses to enable a kind the service doesn't have, rather than accepting
// it and quietly doing nothing: the caller asked for something that cannot
// happen, and reporting success would leave them believing usenet is on for
// a provider that has never had a usenet service.
func (s *liveSettings) SetProviderKinds(_ context.Context, name string, enabled map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.cfg.Providers[name]
	if !exists && !s.registryHasEntry(name) {
		return fmt.Errorf("unknown provider %s", name)
	}
	resolved := entry.ResolvedType(name)
	caps := knownProviderCapabilities[resolved]
	supported := map[string]bool{"torrent": caps.torrent, "usenet": caps.usenet, "webdl": caps.webdl}

	// Start from what is currently on, so an omitted kind is left alone.
	want := map[string]bool{}
	for _, kind := range config.ProviderKinds {
		want[kind] = supported[kind] && entry.KindEnabled(kind)
	}
	for kind, on := range enabled {
		if _, known := supported[kind]; !known {
			return fmt.Errorf("unknown kind %q", kind)
		}
		if on && !supported[kind] {
			return fmt.Errorf("%s has no %s service, so it can't be enabled", name, kind)
		}
		want[kind] = on
	}

	var disabled []string
	for _, kind := range config.ProviderKinds {
		if supported[kind] && !want[kind] {
			disabled = append(disabled, kind)
		}
	}

	timeout := time.Duration(s.cfg.ProviderRequestTimeoutSeconds) * time.Second
	if err := registerProviderEntry(s.registry, name, resolved, entry.APIKey, timeout, func(kind string) bool {
		return want[kind]
	}); err != nil {
		return err
	}

	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]config.ProviderConfig{}
	}
	entry.DisabledKinds = disabled
	s.cfg.Providers[name] = entry
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	slog.Info("api: provider kinds changed", "provider", name, "disabled", disabled)
	return nil
}

// registryHasEntry reports whether the registry knows this name at all.
// Callers hold s.mu.
func (s *liveSettings) registryHasEntry(name string) bool {
	for _, n := range s.registry.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// AddProvider registers a new provider entry live and persists it.
//
// name is the entry, providerType is the implementation. They are separate
// so one service can hold two accounts: "torbox" and "torbox-work" both
// with type "torbox" are two independent providers, each with its own
// credentials, listing cache and rate-limit backoff.
//
// An empty providerType means "same as the name", matching how config is
// read — so adding "torbox" needs no type at all, and only a second account
// has to say what it is.
func (s *liveSettings) AddProvider(_ context.Context, name, providerType, apiKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name == "" {
		return fmt.Errorf("provider name must not be empty")
	}
	if _, exists := s.cfg.Providers[name]; exists {
		return fmt.Errorf("provider %s already exists", name)
	}
	resolved := providerType
	if resolved == "" {
		resolved = name
	}
	if _, known := knownProviders[resolved]; !known {
		return fmt.Errorf("unknown provider type %q", resolved)
	}

	timeout := time.Duration(s.cfg.ProviderRequestTimeoutSeconds) * time.Second
	// A brand-new entry starts with every supported kind enabled.
	if err := registerProviderEntry(s.registry, name, resolved, apiKey, timeout, nil); err != nil {
		return err
	}

	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]config.ProviderConfig{}
	}
	entry := config.ProviderConfig{APIKey: apiKey}
	// Only recorded when it actually differs, so the common single-account
	// config stays exactly as it was.
	if resolved != name {
		entry.Type = resolved
	}
	s.cfg.Providers[name] = entry
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	// Checked here as well as at startup: adding a second account through
	// the UI is the single most likely moment to paste the same key twice,
	// and warning only on the next restart means the operator sees two
	// entries discovering identical downloads with no explanation until
	// then.
	warnOnDuplicateProviderKeys(s.cfg)
	// The importer's shared listing caches are retuned on the next config
	// change; a brand-new provider starts on the package default until
	// then, which is a few seconds either way.
	return nil
}

// RemoveProvider deletes a provider entry live and persists it.
//
// Downloads already tracked against it are deliberately left alone rather
// than deleted: they are the user's records of real things, and removing a
// provider is a configuration change, not an instruction to discard
// history. They stop resolving to a provider, which the API and importer
// already handle by declining to act rather than erroring — see
// providerFor.
func (s *liveSettings) RemoveProvider(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.cfg.Providers[name]; !exists {
		return fmt.Errorf("provider %s is not configured", name)
	}
	s.registry.Unregister(name)
	delete(s.cfg.Providers, name)

	// A removed default hands over to whatever the registry fell back to,
	// so the persisted setting doesn't keep naming something that is gone.
	if s.cfg.DefaultProvider == name {
		s.cfg.DefaultProvider = s.registry.Default()
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

// Status assembles api.StatusInfo from the Importer's own live health
// signals — see api.StatusInfo's doc comment for what each field means. A
// nil imp (tests, or a moment before run() finishes wiring it in) reports an
// empty status rather than erroring, matching DeleteLocalFiles' nil-guard
// convention.
func (s *liveSettings) Status(ctx context.Context) (api.StatusInfo, error) {
	s.mu.Lock()
	imp := s.imp
	s.mu.Unlock()

	kinds := map[string]api.KindStatus{}
	for _, kind := range []database.Kind{database.KindTorrent, database.KindUsenet, database.KindWebDL} {
		kinds[string(kind)] = api.KindStatus{}
	}
	if imp == nil {
		return api.StatusInfo{Kinds: kinds}, nil
	}

	for _, kind := range []database.Kind{database.KindTorrent, database.KindUsenet, database.KindWebDL} {
		ks := kinds[string(kind)]
		if t, ok := imp.LastSuccessfulListAt(kind); ok {
			ks.LastSuccessfulListAt = &t
		}
		if t, ok := imp.RateLimitCooldownUntil(kind); ok {
			ks.RateLimitedUntil = &t
		}
		kinds[string(kind)] = ks
	}

	var providerStatuses []api.ProviderKindStatus
	for _, ps := range imp.ProviderStatuses() {
		entry := api.ProviderKindStatus{Provider: ps.Provider, Kind: ps.Kind}
		if !ps.LastSuccessfulListAt.IsZero() {
			t := ps.LastSuccessfulListAt
			entry.LastSuccessfulListAt = &t
		}
		if !ps.RateLimitedUntil.IsZero() {
			t := ps.RateLimitedUntil
			entry.RateLimitedUntil = &t
		}
		if !ps.ListingAnomalousSince.IsZero() {
			t := ps.ListingAnomalousSince
			entry.ListingAnomalousSince = &t
		}
		providerStatuses = append(providerStatuses, entry)
	}

	counts, err := imp.ErrorCounts(ctx)
	if err != nil {
		return api.StatusInfo{}, fmt.Errorf("status: count error downloads: %w", err)
	}
	for kind, n := range counts {
		ks := kinds[string(kind)]
		ks.ErrorCount = n
		kinds[string(kind)] = ks
	}

	status := api.StatusInfo{Kinds: kinds, Providers: providerStatuses}
	if t, ok := imp.LastTickAt(); ok {
		status.LastTickAt = &t
	}
	return status, nil
}

// --- Auth: optional login accounts (see internal/api/auth.go for the
// password hashing/session logic — this file only ever handles already-
// hashed passwords, matching how internal/config itself never sees a
// plaintext one either) ------------------------------------------------

func (s *liveSettings) AuthEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.AuthSettings().Enabled()
}

// SetupNeeded reports whether this instance is claimable by its first
// visitor: simply, no login account exists yet. Login is mandatory — there
// is no API-key-only way into the web UI — so an instance with a TorBox key
// but no account is still a fresh install as far as the wizard is
// concerned, not an upgrade case to special-case around.
func (s *liveSettings) SetupNeeded() bool {
	return !s.AuthEnabled()
}

// Setup claims a fresh instance: the first login account, always Default
// and admin regardless of what's passed (see config.Config.AddUser).
// Callers (internal/api's handleSetup) are expected to have already
// checked SetupNeeded.
func (s *liveSettings) Setup(_ context.Context, username, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.AddUser(username, passwordHash, config.RoleAdmin); err != nil {
		return err
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

// FindUser looks up a login account's stored hash and effective role, for
// internal/api's handleLogin to verify a plaintext password against.
func (s *liveSettings) FindUser(username string) (passwordHash, role string, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.cfg.AuthSettings().Find(username)
	if u == nil {
		return "", "", false
	}
	return u.PasswordHash, u.EffectiveRole(), true
}

// ListUsers reports every login account — never a password hash, see
// api.UserAccount's own doc comment.
func (s *liveSettings) ListUsers() []api.UserAccount {
	s.mu.Lock()
	users := s.cfg.AuthSettings().Users
	s.mu.Unlock()
	out := make([]api.UserAccount, len(users))
	for i, u := range users {
		out[i] = api.UserAccount{Username: u.Username, Role: u.EffectiveRole(), Default: u.Default}
	}
	return out
}

func (s *liveSettings) AddUser(_ context.Context, username, passwordHash, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.AddUser(username, passwordHash, role); err != nil {
		return err
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

func (s *liveSettings) RemoveUser(_ context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.RemoveUser(username); err != nil {
		return err
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

func (s *liveSettings) SetUserPassword(_ context.Context, username, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.SetUserPassword(username, passwordHash); err != nil {
		return err
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

func (s *liveSettings) SetUserRole(_ context.Context, username, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.SetUserRole(username, role); err != nil {
		return err
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

func (s *liveSettings) SetDefaultUser(_ context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.SetDefaultUser(username); err != nil {
		return err
	}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

func copyCategoryPaths(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
