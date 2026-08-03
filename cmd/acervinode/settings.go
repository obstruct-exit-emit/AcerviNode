package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/acervinode/acervinode/internal/api"
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
// import_fetch_timeout_seconds, cleanup_after_days, download_dir_mode, and
// fast_poll_interval_seconds config.yaml already had at startup, so a value
// set through the UI on a previous run is live again immediately, without
// waiting for another settings call.
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
	s.mu.Unlock()
	imp.SetCategoryPaths(categoryPaths)
	imp.SetMaxConcurrent(maxConcurrent)
	imp.SetFetchTimeout(fetchTimeout)
	imp.SetCleanupAfterDays(cleanupAfterDays)
	imp.SetDirMode(dirMode)
	imp.SetFastPollInterval(fastPollInterval)
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
// happened to re-declare it.
//
// Also seeds every well-known *arr-app default category name
// (defaultArrCategories) unconditionally, every startup — unlike the
// config.yaml-backed ones above, these aren't something a user configured;
// they're what closes the gap for a user who never customizes an *arr app's
// own default category field at all, with zero AcerviNode-side setup.
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
	for _, category := range defaultArrCategories {
		qbt.AddCategory(category)
		sab.AddCategory(category)
	}
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
		DownloadDirMode:           s.cfg.DownloadDirMode,
		FastPollIntervalSeconds:   s.cfg.FastPollIntervalSeconds,
		TLSEnabled:                s.cfg.TLSEnabled,
		TLSPort:                   s.cfg.TLSPort,
		TLSCertFile:               s.cfg.TLSCertFile,
		TLSKeyFile:                s.cfg.TLSKeyFile,
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
	candidate.DownloadDirMode = update.DownloadDirMode
	candidate.FastPollIntervalSeconds = update.FastPollIntervalSeconds
	candidate.TLSEnabled = update.TLSEnabled
	candidate.TLSPort = update.TLSPort
	candidate.TLSCertFile = update.TLSCertFile
	candidate.TLSKeyFile = update.TLSKeyFile
	if err := candidate.Validate(); err != nil {
		return false, err
	}

	restartRequired := candidate.Port != s.cfg.Port || candidate.DataDir != s.cfg.DataDir ||
		candidate.TLSEnabled != s.cfg.TLSEnabled || candidate.TLSPort != s.cfg.TLSPort ||
		candidate.TLSCertFile != s.cfg.TLSCertFile || candidate.TLSKeyFile != s.cfg.TLSKeyFile

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

// AccountStatus reports the configured TorBox account's own plan/usage —
// see debrid.DynamicTorrentProvider.Account, which this delegates to
// directly (torrentDyn already holds whichever concrete provider is
// currently active, live-swapped the same way as everything else here).
func (s *liveSettings) AccountStatus(ctx context.Context) (debrid.AccountStatus, error) {
	return s.torrentDyn.Account(ctx)
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

	counts, err := imp.ErrorCounts(ctx)
	if err != nil {
		return api.StatusInfo{}, fmt.Errorf("status: count error downloads: %w", err)
	}
	for kind, n := range counts {
		ks := kinds[string(kind)]
		ks.ErrorCount = n
		kinds[string(kind)] = ks
	}

	status := api.StatusInfo{Kinds: kinds}
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
