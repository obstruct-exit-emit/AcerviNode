// Package api implements AcerviNode's own versioned REST API (/api/v1) —
// health, version, provider status, settings, and download
// listing/management — the same API the embedded web UI (web/) is built on.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// ProviderStatus summarizes one configured debrid provider for
// GET /api/v1/providers.
type ProviderStatus struct {
	Name           string `json:"name"`
	TorrentCapable bool   `json:"torrent_capable"`
	UsenetCapable  bool   `json:"usenet_capable"`
	WebDLCapable   bool   `json:"webdl_capable"`
}

// BackupInfo is one database snapshot, for GET /api/v1/settings/backups.
type BackupInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	TakenAt   time.Time `json:"taken_at"`
}

// StatusInfo is internal/importer's own health snapshot for
// GET /api/v1/status — meant for an external monitor (Uptime Kuma,
// Healthchecks.io, ...) to poll and alert on, distinct from both
// AccountStatus (the provider's own account state, e.g. TorBox's
// cooldown_until) and ProviderStatus (what's configured). This is about
// whether AcerviNode's own background polling is alive and making progress.
type StatusInfo struct {
	// LastTickAt is when the tick loop last ran, regardless of what it found
	// once inside — proves the loop itself hasn't stalled or crashed, as
	// opposed to one specific kind failing to list (see KindStatus). Omitted
	// if the importer has never ticked yet.
	LastTickAt *time.Time `json:"last_tick_at,omitempty"`
	// Kinds is always keyed "torrent"/"usenet"/"webdl", regardless of
	// whether a provider is actually configured for each — an unconfigured
	// kind just reports zero values throughout.
	Kinds map[string]KindStatus `json:"kinds"`
	// Providers is one entry per provider/kind pair actually polled.
	//
	// Kinds above aggregates across providers, which answers "is this kind
	// working at all" but cannot answer "which provider is struggling":
	// with two configured, one failing every list while the other succeeds
	// still leaves the kind looking healthy, because the healthy one keeps
	// the timestamp moving. Added rather than replacing Kinds so existing
	// monitors keep working.
	Providers []ProviderKindStatus `json:"providers"`
}

// ProviderKindStatus is one provider's handling of one kind within
// StatusInfo.
type ProviderKindStatus struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	// LastSuccessfulListAt is when this provider last answered a bulk
	// listing for this kind without erroring. Omitted if it never has.
	LastSuccessfulListAt *time.Time `json:"last_successful_list_at,omitempty"`
	// RateLimitedUntil is set while this provider is backing off for this
	// kind. Per provider, not per kind: one provider being limited does not
	// stall another.
	RateLimitedUntil *time.Time `json:"rate_limited_until,omitempty"`
	// ListingAnomalousSince is set while this provider/kind's listing is
	// being disbelieved by the mass-vanish guard — successful, but empty
	// enough that acting on it would flag every tracked download as gone.
	// Present only while that holds.
	//
	// Worth monitoring on: it is the one state where everything else here
	// looks healthy (lists succeeding, no rate limit) while nothing is
	// actually reconciling. Cleared either by the listing recovering or,
	// after a grace period, by the guard concluding the listing was right
	// all along — see database.massVanishMaxDuration.
	ListingAnomalousSince *time.Time `json:"listing_anomalous_since,omitempty"`
}

// KindStatus is one kind's (torrent/usenet/webdl) own health signals within
// StatusInfo.
type KindStatus struct {
	// LastSuccessfulListAt is when this kind's provider last answered a bulk
	// listing call without erroring. Omitted if it never has.
	LastSuccessfulListAt *time.Time `json:"last_successful_list_at,omitempty"`
	// RateLimitedUntil is set when this kind is currently backing off after
	// a provider rate-limit (429) response — see internal/importer's
	// rate-limit backoff. Omitted when not currently rate-limited.
	RateLimitedUntil *time.Time `json:"rate_limited_until,omitempty"`
	// ErrorCount is how many downloads of this kind currently sit in
	// AcerviNode's own StateError.
	ErrorCount int `json:"error_count"`
}

// deleter is the subset of debrid.TorrentProvider/debrid.UsenetProvider that
// DELETE /api/v1/downloads/{id} needs — same narrow-interface approach as
// internal/importer's provider.
type deleter interface {
	Delete(ctx context.Context, id debrid.ProviderDownloadID, deleteFiles bool) error
}

// downloadProvider is everything the native API does with the provider a
// *specific, already-tracked* download belongs to: delete it, list its
// files, and hand out links. All three adder interfaces below satisfy it,
// which is what lets one lookup (providerFor) serve every one of those
// paths instead of each re-deriving the provider from the download's kind.
//
// Name is what makes that lookup able to check it has the right provider
// and not merely one that handles the same kind — see providerFor.
type downloadProvider interface {
	deleter
	Name() string
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
	RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error)
}

// torrentAdder is what POST /api/v1/downloads/torrent needs from a
// torrent-capable debrid provider — enough to add a torrent directly
// (magnet or an uploaded .torrent file), mirroring internal/qbittorrent's
// own add flow, plus Delete so one field on Server covers everything the
// native API does with a torrent provider. Files/RequestDownloadLink back
// GET /api/v1/downloads/{id}'s file list and the manual download-link
// endpoint (see handleGetFileLink) — queried live from the provider, the
// same as internal/qbittorrent's own handleFiles, rather than a local cache.
type torrentAdder interface {
	deleter
	Name() string
	AddMagnet(ctx context.Context, magnetURI string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error)
	AddTorrentFile(ctx context.Context, filename string, data []byte, opts debrid.AddOptions) (debrid.ProviderDownloadID, error)
	Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error)
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
	RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error)
	// CheckCached backs GET /api/v1/downloads/torrent/check-cached — lets the
	// "+ Add" form show whether a magnet is already cached before committing
	// to adding it.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
	// TorrentInfo backs GET /api/v1/downloads/torrent/info — previews a
	// torrent's metadata (name/size/files/seeds/peers) by hash alone, before
	// ever adding it. Every concrete provider passed into NewServer is
	// *debrid.DynamicTorrentProvider, which always has this method (see its
	// own doc comment for how it handles a provider that doesn't actually
	// support the underlying capability) — so this is safe to require here
	// rather than a runtime type assertion at the handler layer.
	TorrentInfo(ctx context.Context, hash string) (debrid.TorrentInfo, error)
}

// usenetAdder is torrentAdder's usenet counterpart, backing
// POST /api/v1/downloads/usenet.
type usenetAdder interface {
	deleter
	Name() string
	AddNZBURL(ctx context.Context, link string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error)
	AddNZBFile(ctx context.Context, filename string, data []byte, opts debrid.AddOptions) (debrid.ProviderDownloadID, error)
	Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error)
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
	RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error)
	// CheckCached backs GET /api/v1/downloads/usenet/check-cached — see
	// torrentAdder.CheckCached's own doc comment for the shared reasoning.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
}

// webDownloadAdder is torrentAdder/usenetAdder's Web Downloads counterpart,
// backing POST /api/v1/downloads/webdl — link-only, no file-upload variant
// (TorBox's own Web Downloads service has none either).
type webDownloadAdder interface {
	deleter
	Name() string
	AddLink(ctx context.Context, link string, opts debrid.AddOptions) (debrid.ProviderDownloadID, error)
	Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error)
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
	RequestZipDownloadLink(ctx context.Context, id debrid.ProviderDownloadID) (string, error)
	// CheckCached backs GET /api/v1/downloads/webdl/check-cached — see
	// torrentAdder.CheckCached's own doc comment for the shared reasoning.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
}

// GeneralInfo is AcerviNode's own runtime configuration, as reported to the
// settings UI — everything except provider credentials (see Settings /
// TorBoxConfigured for those). Includes APIKey itself: unlike a provider
// secret, this is the key the caller already had to present to reach this
// endpoint, so returning it back is how the UI (and a human copying it into
// Sonarr/Radarr) can see it without digging through server logs or
// config.yaml.
type GeneralInfo struct {
	APIKey                    string `json:"api_key"`
	Port                      int    `json:"port"`
	DataDir                   string `json:"data_dir"`
	DownloadDir               string `json:"download_dir"`
	LogLevel                  string `json:"log_level"`
	ImportIntervalSeconds     int    `json:"import_interval_seconds"`
	ImportMaxRetries          int    `json:"import_max_retries"`
	MaxConcurrentDownloads    int    `json:"max_concurrent_downloads"`
	ImportFetchTimeoutSeconds int    `json:"import_fetch_timeout_seconds"`
	CleanupAfterDays          int    `json:"cleanup_after_days"`
	// DownloadDirMode is the octal permission string (e.g. "0777") every
	// download directory internal/importer creates gets — see
	// config.Config.DownloadDirMode's own doc comment for why the default
	// is world-writable.
	DownloadDirMode string `json:"download_dir_mode"`
	// FastPollIntervalSeconds is internal/importer's own active-download
	// poll cadence — see config.Config.FastPollIntervalSeconds's own doc
	// comment.
	FastPollIntervalSeconds int `json:"fast_poll_interval_seconds"`
	// ProviderRequestTimeoutSeconds bounds a single call to the debrid
	// provider's own API — see config.Config.ProviderRequestTimeoutSeconds's
	// own doc comment.
	ProviderRequestTimeoutSeconds int `json:"provider_request_timeout_seconds"`
	// TLSEnabled/TLSPort/TLSCertFile/TLSKeyFile mirror config.Config's own
	// TLS fields exactly — see docs/providers.md's TLS section. Cert/key
	// file overrides are config/env-only (no editable UI field), the same
	// treatment DataDir already gets, but are still reported here for
	// transparency/scripting, the same way DataDir is.
	TLSEnabled  bool   `json:"tls_enabled"`
	TLSPort     int    `json:"tls_port"`
	TLSCertFile string `json:"tls_cert_file"`
	TLSKeyFile  string `json:"tls_key_file"`
	// MinFetchFileSizeBytes/MaxFetchFileSizeBytes/IncludeFileRegex/
	// ExcludeFileRegex mirror config.Config's own file-filtering fields
	// exactly — see config.Config.MinFetchFileSizeBytes's own doc comment.
	MinFetchFileSizeBytes int64  `json:"min_fetch_file_size_bytes"`
	MaxFetchFileSizeBytes int64  `json:"max_fetch_file_size_bytes"`
	IncludeFileRegex      string `json:"include_file_regex"`
	ExcludeFileRegex      string `json:"exclude_file_regex"`
	// StuckDownloadTimeoutMinutes/CleanupErrorAfterDays mirror
	// config.Config's own watchdog/error-cleanup fields exactly — see
	// config.Config.StuckDownloadTimeoutMinutes's own doc comment.
	StuckDownloadTimeoutMinutes int `json:"stuck_download_timeout_minutes"`
	CleanupErrorAfterDays       int `json:"cleanup_error_after_days"`
	// BackupIntervalHours/BackupKeep mirror config's own fields — see
	// internal/backup.
	BackupIntervalHours int `json:"backup_interval_hours"`
	BackupKeep          int `json:"backup_keep"`
}

// GeneralUpdate is a candidate change to AcerviNode's general configuration
// (everything in GeneralInfo except the API key, which has its own
// regenerate flow). Port/DataDir take effect only after a restart — binding
// a new port or reopening the database live is out of scope for now — while
// every other field applies immediately; see Settings.UpdateGeneral's
// RestartRequired return value.
type GeneralUpdate struct {
	Port                          int    `json:"port"`
	DataDir                       string `json:"data_dir"`
	DownloadDir                   string `json:"download_dir"`
	LogLevel                      string `json:"log_level"`
	ImportIntervalSeconds         int    `json:"import_interval_seconds"`
	ImportMaxRetries              int    `json:"import_max_retries"`
	MaxConcurrentDownloads        int    `json:"max_concurrent_downloads"`
	ImportFetchTimeoutSeconds     int    `json:"import_fetch_timeout_seconds"`
	CleanupAfterDays              int    `json:"cleanup_after_days"`
	DownloadDirMode               string `json:"download_dir_mode"`
	FastPollIntervalSeconds       int    `json:"fast_poll_interval_seconds"`
	ProviderRequestTimeoutSeconds int    `json:"provider_request_timeout_seconds"`
	TLSEnabled                    bool   `json:"tls_enabled"`
	TLSPort                       int    `json:"tls_port"`
	TLSCertFile                   string `json:"tls_cert_file"`
	TLSKeyFile                    string `json:"tls_key_file"`
	MinFetchFileSizeBytes         int64  `json:"min_fetch_file_size_bytes"`
	MaxFetchFileSizeBytes         int64  `json:"max_fetch_file_size_bytes"`
	IncludeFileRegex              string `json:"include_file_regex"`
	ExcludeFileRegex              string `json:"exclude_file_regex"`
	StuckDownloadTimeoutMinutes   int    `json:"stuck_download_timeout_minutes"`
	CleanupErrorAfterDays         int    `json:"cleanup_error_after_days"`
	BackupIntervalHours           int    `json:"backup_interval_hours"`
	BackupKeep                    int    `json:"backup_keep"`
}

// Settings lets the API read and change configuration live, without a
// restart — see internal/debrid's Dynamic*Provider types, which is what
// makes an in-place provider swap possible, and settings.go for the HTTP
// surface built on this interface. Provider methods all take the provider
// entry by name — nothing here is scoped to a concrete implementation, which
// is what lets several accounts, on the same service or different ones, be
// configured at once (see docs/providers.md).
type Settings interface {
	// ProviderConfigured reports whether the named provider currently holds
	// credentials. False for a provider that isn't registered at all.
	ProviderConfigured(name string) bool
	// ProviderSupportedKinds reports which kinds the provider's *service*
	// can do at all, regardless of whether they are switched on. Distinct
	// from what the registry holds, which is the intersection of supported
	// and enabled — the difference is exactly what lets the UI show "this
	// service has no usenet" differently from "you turned usenet off".
	ProviderSupportedKinds(name string) map[string]bool
	// SetProviderKinds switches kinds on or off for one provider, applying
	// immediately and persisting. A kind the service doesn't support cannot
	// be switched on.
	SetProviderKinds(ctx context.Context, name string, enabled map[string]bool) error
	// SetProviderAPIKey applies a key to the named provider immediately and
	// persists it. An empty key clears the provider's credentials, leaving
	// it registered but unconfigured.
	SetProviderAPIKey(ctx context.Context, name, apiKey string) error
	// TestProviderConnection makes one real, lightweight call to the named
	// provider with its currently configured key and reports how long it
	// took — a genuine connectivity+auth check, not just "a key is set."
	TestProviderConnection(ctx context.Context, name string) (latencyMs int64, err error)
	// DefaultProvider is which provider a new download goes to when nothing
	// says otherwise; SetDefaultProvider changes and persists it.
	DefaultProvider() string
	SetDefaultProvider(name string) error
	// ProviderTypes lists the provider implementations this build can
	// construct — what AddProvider will accept as a type.
	ProviderTypes() []string
	// ProviderType is which implementation an entry uses. Equal to the name
	// for a first account; different when one service holds several.
	ProviderType(name string) string
	// AddProvider registers a new provider entry live and persists it. name
	// is the entry, providerType the implementation; they differ when one
	// service holds two accounts. An empty providerType means "same as the
	// name".
	AddProvider(ctx context.Context, name, providerType, apiKey string) error
	// RemoveProvider deletes a provider entry live and persists it.
	RemoveProvider(ctx context.Context, name string) error
	// RunBackupNow takes a database snapshot immediately, returning the
	// file written.
	RunBackupNow(ctx context.Context) (string, error)
	// Backups lists the snapshots currently on disk, newest first.
	Backups() ([]BackupInfo, error)
	// APIKey returns AcerviNode's own current API key — the live source of
	// truth every authenticated route (native API and both compat shims)
	// checks against, so a regenerated key takes effect everywhere at once.
	APIKey() string
	// RegenerateAPIKey replaces the current API key with a fresh random one,
	// applies it immediately, and persists it to config.yaml.
	RegenerateAPIKey(ctx context.Context) (string, error)
	// General reports the rest of AcerviNode's current configuration.
	General() GeneralInfo
	// UpdateGeneral validates and applies a candidate configuration change,
	// persists it to config.yaml, and reports whether a restart is needed
	// for all of it to take effect (see GeneralUpdate).
	UpdateGeneral(ctx context.Context, update GeneralUpdate) (restartRequired bool, err error)
	// Categories lists every category name each compat shim currently
	// knows about (populated reactively by *arr apps, or manually via
	// AddCategory).
	Categories() (torrent []string, usenet []string)
	// AddCategory manually registers a category name for the given
	// protocol ("torrent" or "usenet").
	AddCategory(protocol, name string) error
	// CategoryPaths reports the current category-name -> override-directory
	// map, applied by internal/importer in place of download_dir/<category>
	// for downloads in that category (see SetCategoryPath).
	CategoryPaths() map[string]string
	// SetCategoryPath sets (or, if path is empty, clears) category's
	// override destination directory, applies it live, and persists it.
	SetCategoryPath(ctx context.Context, category, path string) error
	// RemoveCategory forgets a category entirely (its path override and its
	// registration with both compat shims) — unlike SetCategoryPath with an
	// empty path, which clears the override but leaves the category
	// registered, this removes it from the known list outright.
	RemoveCategory(ctx context.Context, category string) error
	// AccountStatus reports the configured provider's own account status
	// (plan tier, premium expiry, lifetime usage) — a live call, not a
	// snapshot, so it always reflects the actual current TorBox account. An
	// error here (e.g. not configured yet, or the provider doesn't support
	// this) is routine and shown as "unavailable" rather than a hard failure.
	// AccountStatus reports the named provider's own account state. Each
	// provider has its own plan, expiry and restrictions, so this is asked
	// per provider rather than once for the instance.
	AccountStatus(ctx context.Context, provider string) (debrid.AccountStatus, error)
	// Status reports internal/importer's own health signals (tick liveness,
	// per-kind rate-limit cooldowns, per-kind error counts) for
	// GET /api/v1/status — see StatusInfo.
	Status(ctx context.Context) (StatusInfo, error)

	// --- Auth: optional login accounts on top of the API key, which keeps
	// working unaffected by any of this (see internal/api/auth.go) ---------

	// AuthEnabled reports whether any login account exists — the UI uses
	// this to decide between the login form and the API-key prompt.
	AuthEnabled() bool
	// SetupNeeded reports whether this instance is claimable by its first
	// visitor: no login account and no provider configured yet — a
	// genuinely fresh install. An instance already in real use (a
	// configured provider) is never claimable just because its operator
	// happened not to set up a login account.
	SetupNeeded() bool
	// Setup claims a fresh instance in one step: creates the first
	// (Default, always admin) login account. Refused by the caller
	// (internal/api) if SetupNeeded is false.
	Setup(ctx context.Context, username, passwordHash string) error
	// FindUser looks up a login account's stored password hash and
	// effective role, for handleLogin to verify a plaintext password
	// against — never exposed directly to a client (see ListUsers).
	FindUser(username string) (passwordHash, role string, found bool)
	// ListUsers reports every login account, never a password hash — backs
	// Settings → Security's user list.
	ListUsers() []UserAccount
	// AddUser creates an additional login account. role is RoleAdmin or
	// RoleMember; anything else (including "") becomes RoleMember.
	AddUser(ctx context.Context, username, passwordHash, role string) error
	// RemoveUser deletes a login account; the Default account is refused.
	RemoveUser(ctx context.Context, username string) error
	// SetUserPassword changes one account's password.
	SetUserPassword(ctx context.Context, username, passwordHash string) error
	// SetUserRole promotes/demotes an account between admin and member;
	// the Default account can't be demoted.
	SetUserRole(ctx context.Context, username, role string) error
	// SetDefaultUser promotes an account to the protected default (and
	// admin in the same step).
	SetDefaultUser(ctx context.Context, username string) error

	// --- System: restarting and TLS certificate management, both admin-only
	// (see docs/providers.md's TLS section) ------------------------------

	// SupervisedBySystemd reports whether a process supervisor (systemd) is
	// actually watching this process — RequestRestart works either way, but
	// a caller with no supervisor watching won't come back on its own, so
	// the UI needs to say something different than "restarting…".
	SupervisedBySystemd() bool
	// RequestRestart gracefully shuts down and exits — config changes that
	// need a restart to apply (port, tls_enabled/tls_port, ...) are already
	// persisted by the time this is called, so the next start picks them up
	// automatically. Errors if no restart trigger has been wired up.
	RequestRestart(ctx context.Context) error
	// RegenerateCertificate forces a fresh self-signed TLS certificate,
	// discarding the current one — the fix when its SANs no longer match how
	// the instance is reached. Requires a restart to load the new cert.
	// Refused when a custom tls_cert_file/tls_key_file is configured.
	RegenerateCertificate(ctx context.Context) error

	// DeleteLocalFiles removes d's local files from disk, if any — the
	// local-filesystem half of a "delete and remove files" request (see
	// handleDeleteDownload). Delegates to internal/importer, the only place
	// that knows how to resolve a download's actual destination directory
	// (download_dir, category overrides, an explicit save_path) live.
	DeleteLocalFiles(d *database.Download) error

	// CancelFetch interrupts internal/importer's in-flight fetch for id, if
	// one is currently running, and blocks until it has genuinely stopped
	// — see handleDeleteDownload, the only caller, called before touching
	// local files or the provider: without this, a download deleted while
	// internal/importer was still mid-write for it could leave an orphaned
	// file on disk, since the fetch goroutine had no way to know the row it
	// was writing for had just been deleted. A no-op if nothing's actively
	// fetching id right now.
	CancelFetch(id string)
}

// Server is AcerviNode's native API.
type Server struct {
	version string
	db      *database.DB
	// registry is every configured provider. Which one a request uses
	// depends on the request: anything acting on an existing download
	// resolves the provider that download belongs to (providerFor), while
	// an add — which has no download yet — goes to the default
	// (defaultTorrent and friends).
	registry *debrid.Registry
	settings Settings
	sessions *sessionStore
	mux      *http.ServeMux
}

// defaultTorrent is the torrent provider a new download goes to, or nil if
// nothing registered supports torrents.
func (s *Server) defaultTorrent() *debrid.DynamicTorrentProvider {
	return s.registry.DefaultTorrent()
}

// defaultUsenet is defaultTorrent's usenet counterpart.
func (s *Server) defaultUsenet() *debrid.DynamicUsenetProvider {
	return s.registry.DefaultUsenet()
}

// defaultWebDL is defaultTorrent's web-download counterpart.
func (s *Server) defaultWebDL() *debrid.DynamicWebDownloadProvider {
	return s.registry.DefaultWebDL()
}

// NewServer builds the native API server. version is a free-form build
// identifier. Any provider may be nil. Auth checks settings.APIKey() live on
// every request rather than a value captured at construction, so a
// regenerated key takes effect immediately — see requireAuth. Login
// sessions live only in this Server's own memory (see sessionStore) — a
// process restart logs everyone out.
func NewServer(version string, db *database.DB, registry *debrid.Registry, settings Settings) *Server {
	s := &Server{
		version:  version,
		db:       db,
		registry: registry,
		settings: settings,
		sessions: newSessionStore(),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Health, and everything auth needs to answer before any credentials
	// exist (or without any at all), are intentionally unauthenticated —
	// health is what a container orchestrator or systemd healthcheck hits
	// and shouldn't need a key; the rest is what the web UI needs to decide
	// between the first-run wizard, the login form, and the (existing)
	// API-key prompt.
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	s.mux.HandleFunc("POST /api/v1/setup", s.handleSetup)

	s.mux.HandleFunc("GET /api/v1/version", s.requireAuth(s.handleVersion))
	s.mux.HandleFunc("GET /api/v1/providers", s.requireAuth(s.handleProviders))
	s.mux.HandleFunc("GET /api/v1/status", s.requireAuth(s.handleStatus))
	// Downloads: any authenticated user (admin or member) can reach these —
	// handleListDownloads/downloadByID are what actually scope a member to
	// Manual downloads only (see their own doc comments and
	// docs/providers.md#roles), not the route registration here.
	s.mux.HandleFunc("GET /api/v1/downloads", s.requireAuth(s.handleListDownloads))
	s.mux.HandleFunc("POST /api/v1/downloads/torrent", s.requireAuth(s.handleAddTorrent))
	s.mux.HandleFunc("POST /api/v1/downloads/usenet", s.requireAuth(s.handleAddUsenet))
	s.mux.HandleFunc("POST /api/v1/downloads/webdl", s.requireAuth(s.handleAddWebDownload))
	// Cached-status/metadata previews, all read-only and side-effect-free —
	// same auth tier as the add endpoints above, since they exist purely to
	// inform a decision the "+ Add" form is about to make.
	s.mux.HandleFunc("GET /api/v1/downloads/torrent/check-cached", s.requireAuth(s.handleCheckCachedTorrent))
	s.mux.HandleFunc("GET /api/v1/downloads/torrent/info", s.requireAuth(s.handleTorrentInfo))
	s.mux.HandleFunc("GET /api/v1/downloads/usenet/check-cached", s.requireAuth(s.handleCheckCachedUsenet))
	s.mux.HandleFunc("GET /api/v1/downloads/webdl/check-cached", s.requireAuth(s.handleCheckCachedWebDownload))
	s.mux.HandleFunc("GET /api/v1/downloads/{id}", s.requireAuth(s.handleGetDownload))
	s.mux.HandleFunc("DELETE /api/v1/downloads/{id}", s.requireAuth(s.handleDeleteDownload))
	s.mux.HandleFunc("POST /api/v1/downloads/{id}/retry", s.requireAuth(s.handleRetryDownload))
	s.mux.HandleFunc("POST /api/v1/downloads/{id}/readd", s.requireAuth(s.handleReAddDownload))
	s.mux.HandleFunc("GET /api/v1/downloads/{id}/files/{fileId}/link", s.requireAuth(s.handleGetFileLink))
	s.mux.HandleFunc("GET /api/v1/downloads/{id}/zip-link", s.requireAuth(s.handleGetZipLink))

	// Settings and user management are admin-only — a member has no
	// business changing system configuration or other accounts.
	s.mux.HandleFunc("GET /api/v1/settings/providers", s.requireAdmin(s.handleGetProviderSettings))
	// "default" is registered ahead of {name} so it can never be read as a
	// provider by that name.
	s.mux.HandleFunc("GET /api/v1/settings/backups", s.requireAdmin(s.handleGetBackups))
	s.mux.HandleFunc("POST /api/v1/settings/backups", s.requireAdmin(s.handleRunBackup))
	s.mux.HandleFunc("POST /api/v1/settings/providers", s.requireAdmin(s.handleAddProvider))
	s.mux.HandleFunc("GET /api/v1/settings/provider-types", s.requireAdmin(s.handleGetProviderTypes))
	s.mux.HandleFunc("DELETE /api/v1/settings/providers/{name}", s.requireAdmin(s.handleRemoveProvider))
	s.mux.HandleFunc("PUT /api/v1/settings/providers/default", s.requireAdmin(s.handleSetDefaultProvider))
	s.mux.HandleFunc("PUT /api/v1/settings/providers/{name}", s.requireAdmin(s.handleSetProviderAPIKey))
	s.mux.HandleFunc("PUT /api/v1/settings/providers/{name}/kinds", s.requireAdmin(s.handleSetProviderKinds))
	s.mux.HandleFunc("POST /api/v1/settings/providers/{name}/test", s.requireAdmin(s.handleTestProviderConnection))
	s.mux.HandleFunc("GET /api/v1/settings/general", s.requireAdmin(s.handleGetGeneralSettings))
	s.mux.HandleFunc("PUT /api/v1/settings/general", s.requireAdmin(s.handleUpdateGeneralSettings))
	s.mux.HandleFunc("POST /api/v1/settings/api-key/regenerate", s.requireAdmin(s.handleRegenerateAPIKey))
	s.mux.HandleFunc("GET /api/v1/settings/categories", s.requireAdmin(s.handleGetCategories))
	s.mux.HandleFunc("POST /api/v1/settings/categories", s.requireAdmin(s.handleAddCategory))
	s.mux.HandleFunc("PUT /api/v1/settings/categories/path", s.requireAdmin(s.handleSetCategoryPath))
	s.mux.HandleFunc("DELETE /api/v1/settings/categories/{category}", s.requireAdmin(s.handleRemoveCategory))
	s.mux.HandleFunc("GET /api/v1/settings/account", s.requireAdmin(s.handleGetAccountStatus))
	s.mux.HandleFunc("GET /api/v1/settings/providers/{name}/account", s.requireAdmin(s.handleGetProviderAccountStatus))
	s.mux.HandleFunc("GET /api/v1/settings/users", s.requireAdmin(s.handleListUsers))
	s.mux.HandleFunc("POST /api/v1/settings/users", s.requireAdmin(s.handleAddUser))
	s.mux.HandleFunc("DELETE /api/v1/settings/users/{username}", s.requireAdmin(s.handleRemoveUser))
	s.mux.HandleFunc("PUT /api/v1/settings/users/{username}/role", s.requireAdmin(s.handleSetUserRole))
	s.mux.HandleFunc("POST /api/v1/settings/users/{username}/default", s.requireAdmin(s.handleMakeDefaultUser))
	// Not requireAdmin: self-service password changes are allowed for any
	// signed-in account, admin or not — see handleSetUserPassword's own
	// admin-or-self check.
	s.mux.HandleFunc("PUT /api/v1/settings/users/{username}/password", s.requireAuth(s.handleSetUserPassword))
	s.mux.HandleFunc("POST /api/v1/settings/system/restart", s.requireAdmin(s.handleRestartServer))
	s.mux.HandleFunc("POST /api/v1/settings/tls/regenerate", s.requireAdmin(s.handleRegenerateCertificate))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": s.version})
}

// handleStatus reports internal/importer's own health signals — see
// StatusInfo. Authenticated (same tier as /providers and /version) rather
// than open like /health, since it surfaces operational detail (error
// counts, timestamps) beyond a bare liveness check; an external monitor is
// expected to poll it with the same API key already used for everything
// else.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.settings.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

// handleProviders reports providers as currently configured — computed live
// from Settings, not a snapshot from startup, so it reflects a key added or
// changed through the settings endpoints without needing a restart.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	providers := []ProviderStatus{}
	for _, name := range s.registry.Names() {
		t, u, w := s.registry.Torrent(name), s.registry.Usenet(name), s.registry.WebDL(name)
		// Registered but with no credentials yet is not "a provider" from a
		// caller's point of view — the wrapper exists so a key can be set
		// into it later, which is an implementation detail.
		if (t == nil || !t.Configured()) && (u == nil || !u.Configured()) && (w == nil || !w.Configured()) {
			continue
		}
		providers = append(providers, ProviderStatus{
			Name:           name,
			TorrentCapable: t != nil && t.Configured(),
			UsenetCapable:  u != nil && u.Configured(),
			WebDLCapable:   w != nil && w.Configured(),
		})
	}
	writeJSON(w, providers)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
