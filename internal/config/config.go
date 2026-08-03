// Package config loads AcerviNode's configuration from config.yaml, with
// ACERVINODE_* environment variables taking precedence over file values.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProviderConfig holds the settings for a single debrid provider, keyed by
// provider name (e.g. "torbox") in Config.Providers.
type ProviderConfig struct {
	APIKey string `yaml:"api_key"`
}

// Config is AcerviNode's full runtime configuration.
type Config struct {
	Port      int                       `yaml:"port"`
	DataDir   string                    `yaml:"data_dir"`
	APIKey    string                    `yaml:"api_key"`
	LogLevel  string                    `yaml:"log_level"`
	Providers map[string]ProviderConfig `yaml:"providers"`

	// DownloadDir is where completed files land when the *arr app that added
	// a download didn't supply its own save_path (see internal/importer).
	DownloadDir string `yaml:"download_dir"`
	// ImportIntervalSeconds controls how often internal/importer ticks: it
	// proactively refreshes every tracked download's status from its
	// provider (rather than waiting on an *arr app to poll one of the compat
	// shims — see refreshStatuses) and checks for provider-completed
	// downloads to fetch to local disk.
	ImportIntervalSeconds int `yaml:"import_interval_seconds"`
	// ImportMaxRetries is how many times internal/importer retries a failed
	// fetch (with exponential backoff between attempts, based on
	// ImportIntervalSeconds) before giving up and moving the download to
	// StateError.
	ImportMaxRetries int `yaml:"import_max_retries"`
	// MaxConcurrentDownloads bounds how many provider_completed downloads
	// internal/importer fetches to local disk at once — without this, every
	// Tick processed its due downloads strictly one at a time, however many
	// there were.
	MaxConcurrentDownloads int `yaml:"max_concurrent_downloads"`
	// ImportFetchTimeoutSeconds is an idle/stall deadline, not a
	// total-transfer one: internal/importer gives up on a file fetch
	// (counted as a failed attempt, subject to the same retry/backoff as
	// any other fetch error) only after this many seconds pass with zero
	// bytes received — covers the connect-and-wait-for-headers phase too,
	// not just the body. A transfer that's slow overall but never actually
	// stops making progress is never affected by this, however long the
	// whole thing takes; only a connection that's actually gone quiet trips
	// it. See internal/importer's idleTimeoutReader.
	ImportFetchTimeoutSeconds int `yaml:"import_fetch_timeout_seconds"`
	// CleanupAfterDays, if > 0, has internal/importer automatically remove a
	// Managed (AddedViaArr) download once it's been ready_for_import for at
	// least this many days: local files deleted, the provider-side download
	// deleted (best-effort), and the row itself removed. Deliberately scoped
	// to ready_for_import only — that's a Managed download an *arr app has
	// already imported elsewhere, so AcerviNode's own copy is redundant
	// storage at that point; a Manual download in provider_completed is
	// never eligible, since that's the ongoing "available, not yet grabbed"
	// state for something the user hasn't downloaded yet — auto-deleting
	// that would delete something before it was ever used. 0 (the default)
	// disables cleanup entirely — the only field in this config with a
	// meaningful "off" value, since every other numeric setting here is
	// always-on and just tunes how it behaves.
	CleanupAfterDays int `yaml:"cleanup_after_days"`
	// DownloadDirMode is the permission mode (octal string, e.g. "0777")
	// internal/importer creates every download directory with — see its own
	// ensureWritableDir. Defaults to "0777" (world-writable): an *arr app
	// almost never runs as the same user/group AcerviNode's own dedicated
	// systemd user does (very commonly a separate Docker container with its
	// own PUID/PGID), and its completed-import step needs write access on
	// whichever directory contains a finished download's files to actually
	// move or hardlink them out — found live from a real Radarr "Access ...
	// is denied" bug (see docs/providers.md#directory-permissions).
	// World-writable is the zero-configuration answer; a user who'd rather
	// not have that (e.g. AcerviNode's own systemd User=/Group= already
	// matches their *arr stack) can tighten this back down, e.g. "0755" or
	// "0775".
	DownloadDirMode string `yaml:"download_dir_mode"`
	// FastPollIntervalSeconds controls internal/importer's fast per-download
	// poll (see its own fastPollInterval doc comment) — how often an
	// actively in-flight Managed download is checked individually via a
	// single targeted per-ID provider call, independent of
	// ImportIntervalSeconds's own full-account listing. Defaults to 3
	// seconds — confirmed live against a real debrid provider (TorBox) to
	// be fast enough to stay responsive without tripping its rate limit,
	// since a targeted per-ID lookup is dramatically cheaper than a bulk
	// listing; going much lower risks rate-limiting once more than a
	// handful of downloads are actively in flight at once.
	FastPollIntervalSeconds int `yaml:"fast_poll_interval_seconds"`

	// ProviderRequestTimeoutSeconds bounds how long a single call to the
	// debrid provider's own API (list, status, add, delete, account — every
	// one of them) may run before being cancelled. A plain total-request
	// deadline, not an idle one like ImportFetchTimeoutSeconds — a provider
	// API response is a small JSON payload, not a multi-gigabyte file, so
	// there's no legitimate "slow but actively trickling for 30+ seconds"
	// case to protect against the way there is for a file download. Default
	// (30s) matches torbox.defaultRequestTimeout, the value this replaced —
	// previously fixed at construction with no way to change it at all; found
	// worth exposing live after a real TorBox outage where account-status
	// calls each took the full 30s to fail.
	ProviderRequestTimeoutSeconds int `yaml:"provider_request_timeout_seconds"`

	// CategoryPaths overrides DownloadDir on a per-category basis: a download
	// in category "movies" mapped to "/mnt/movies" lands directly under that
	// path (still namespaced by the download's own name) instead of under
	// DownloadDir/movies. Only consulted when a download has no explicit
	// save_path of its own (see internal/importer) — an *arr app supplying
	// its own save_path always wins, the same as DownloadDir's fallback role
	// today. Keyed by category name only, not per-protocol: the same
	// category name means the same destination whether it came from the
	// qBittorrent or SABnzbd compat shim.
	CategoryPaths map[string]string `yaml:"category_paths"`

	// DefaultCategoriesSeeded is set once, the first time cmd/acervinode
	// seeds every well-known *arr-app default category name into
	// CategoryPaths (see cmd/acervinode's seedDefaultCategoriesOnce) —
	// never again after that, on any later startup. Without this, a default
	// re-seeded unconditionally on every start could never actually be
	// deleted: the very next restart would silently bring it back, the same
	// "seed once, never resurrect" shape as database.discoverySeeded for
	// Manual-download discovery. Deliberately lives here (config.yaml)
	// rather than the database — CategoryPaths itself already does.
	DefaultCategoriesSeeded bool `yaml:"default_categories_seeded,omitempty"`

	// Auth holds the optional login accounts — see auth.go. No accounts
	// means authentication is disabled entirely, the same API-key-only
	// model AcerviNode always used before this existed.
	Auth AuthSettings `yaml:"auth,omitempty"`

	// TLSEnabled starts a second HTTP server on TLSPort, serving HTTPS with a
	// self-signed certificate (auto-generated on first need — see
	// internal/tlscert) alongside the existing plain-HTTP one on Port, which
	// keeps running completely unchanged either way. Exists so the browser's
	// File System Access API (secure-context-only, HTTPS or localhost) is
	// reachable on an instance only ever visited over a plain LAN IP — see
	// docs/providers.md's TLS section. Requires a restart to take effect.
	TLSEnabled bool `yaml:"tls_enabled"`
	// TLSPort is where the HTTPS listener binds when TLSEnabled — deliberately
	// not Port+1 (that silently collides with whatever else is running the
	// moment Port is changed to something nonstandard); 8443 is a
	// recognizable, Portainer-adjacent convention instead.
	TLSPort int `yaml:"tls_port"`
	// TLSCertFile/TLSKeyFile let an operator supply a real certificate (e.g.
	// from Tailscale's own cert tooling) instead of the auto-generated
	// self-signed one — both or neither, enforced by Validate. Config/env
	// only, deliberately not exposed in the Settings UI's quick-edit form,
	// the same treatment DataDir already gets for the same reason: an
	// advanced, rarely-touched path setting.
	TLSCertFile string `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile  string `yaml:"tls_key_file,omitempty"`
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

func defaults() *Config {
	return &Config{
		Port:                          7846,
		DataDir:                       "./data",
		LogLevel:                      "info",
		Providers:                     map[string]ProviderConfig{},
		DownloadDir:                   "./downloads",
		ImportIntervalSeconds:         10,
		ImportMaxRetries:              5,
		MaxConcurrentDownloads:        3,
		ImportFetchTimeoutSeconds:     600,
		CleanupAfterDays:              0,
		DownloadDirMode:               "0777",
		FastPollIntervalSeconds:       3,
		ProviderRequestTimeoutSeconds: 30,
		CategoryPaths:                 map[string]string{},
		TLSPort:                       8443,
	}
}

// Load reads config from path (if it exists), applies ACERVINODE_* environment
// overrides, fills in an API key if one wasn't set, and validates the result.
// An empty path skips the file read and uses defaults plus env overrides only.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// no config file yet — defaults and env vars only
		default:
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	if cfg.CategoryPaths == nil {
		cfg.CategoryPaths = map[string]string{}
	}

	applyEnv(cfg)

	if cfg.APIKey == "" {
		key, err := NewAPIKey()
		if err != nil {
			return nil, fmt.Errorf("generate api key: %w", err)
		}
		cfg.APIKey = key
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("ACERVINODE_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
		}
	}
	if v := os.Getenv("ACERVINODE_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("ACERVINODE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("ACERVINODE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("ACERVINODE_DOWNLOAD_DIR"); v != "" {
		cfg.DownloadDir = v
	}
	if v := os.Getenv("ACERVINODE_IMPORT_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ImportIntervalSeconds = n
		}
	}
	if v := os.Getenv("ACERVINODE_IMPORT_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ImportMaxRetries = n
		}
	}
	if v := os.Getenv("ACERVINODE_MAX_CONCURRENT_DOWNLOADS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrentDownloads = n
		}
	}
	if v := os.Getenv("ACERVINODE_IMPORT_FETCH_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ImportFetchTimeoutSeconds = n
		}
	}
	if v := os.Getenv("ACERVINODE_CLEANUP_AFTER_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CleanupAfterDays = n
		}
	}
	if v := os.Getenv("ACERVINODE_DOWNLOAD_DIR_MODE"); v != "" {
		cfg.DownloadDirMode = v
	}
	if v := os.Getenv("ACERVINODE_FAST_POLL_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.FastPollIntervalSeconds = n
		}
	}
	if v := os.Getenv("ACERVINODE_PROVIDER_REQUEST_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ProviderRequestTimeoutSeconds = n
		}
	}
	if v := os.Getenv("ACERVINODE_TLS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.TLSEnabled = b
		}
	}
	if v := os.Getenv("ACERVINODE_TLS_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.TLSPort = port
		}
	}
	if v := os.Getenv("ACERVINODE_TLS_CERT_FILE"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("ACERVINODE_TLS_KEY_FILE"); v != "" {
		cfg.TLSKeyFile = v
	}

	// ACERVINODE_PROVIDERS_<NAME>_API_KEY=... overrides/creates a provider entry.
	const prefix = "ACERVINODE_PROVIDERS_"
	const suffix = "_API_KEY"
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix))
		if name == "" {
			continue
		}
		pc := cfg.Providers[name]
		pc.APIKey = value
		cfg.Providers[name] = pc
	}
}

// Validate reports whether c's field values are well-formed — exported so
// the settings API (internal/api, via cmd/acervinode's liveSettings) can
// validate a candidate update before committing it, the same rules Load
// applies at startup.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be between 1 and 65535", c.Port)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log_level %q: must be one of debug, info, warn, error", c.LogLevel)
	}
	if c.DownloadDir == "" {
		return fmt.Errorf("download_dir must not be empty")
	}
	if c.ImportIntervalSeconds < 1 {
		return fmt.Errorf("import_interval_seconds must be at least 1")
	}
	if c.ImportMaxRetries < 1 {
		return fmt.Errorf("import_max_retries must be at least 1")
	}
	if c.MaxConcurrentDownloads < 1 {
		return fmt.Errorf("max_concurrent_downloads must be at least 1")
	}
	if c.ImportFetchTimeoutSeconds < 1 {
		return fmt.Errorf("import_fetch_timeout_seconds must be at least 1")
	}
	if c.CleanupAfterDays < 0 {
		return fmt.Errorf("cleanup_after_days must not be negative")
	}
	if _, err := ParseDirMode(c.DownloadDirMode); err != nil {
		return fmt.Errorf("invalid download_dir_mode %q: %w", c.DownloadDirMode, err)
	}
	if c.FastPollIntervalSeconds < 1 {
		return fmt.Errorf("fast_poll_interval_seconds must be at least 1")
	}
	if c.ProviderRequestTimeoutSeconds < 1 {
		return fmt.Errorf("provider_request_timeout_seconds must be at least 1")
	}
	if c.TLSEnabled {
		if c.TLSPort < 1 || c.TLSPort > 65535 {
			return fmt.Errorf("invalid tls_port %d: must be between 1 and 65535", c.TLSPort)
		}
		if c.TLSPort == c.Port {
			return fmt.Errorf("tls_port must differ from port (both %d)", c.Port)
		}
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("tls_cert_file and tls_key_file must be set together, or not at all")
	}
	return nil
}

// ParseDirMode parses a download_dir_mode string (e.g. "0777", "777",
// "0755") into a plain Unix directory permission — the standard
// rwxrwxrwx bits only (0000-0777); setuid/setgid/sticky are out of scope
// for a plain download directory, so anything above 0777 is rejected
// rather than silently accepted and misinterpreted.
func ParseDirMode(s string) (os.FileMode, error) {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("must be an octal permission string, e.g. \"0777\": %w", err)
	}
	if n > 0o777 {
		return 0, fmt.Errorf("must be between 0000 and 0777")
	}
	return os.FileMode(n), nil
}

// Save writes the full config back to path as YAML (0600 — it contains
// secrets), overwriting whatever was there. Used by the settings API
// (internal/api) so a provider key set through the web UI survives a
// restart, the same as one hand-edited into config.yaml. Comments in an
// existing file are not preserved — yaml.v3's encoder doesn't round-trip
// them — which is an accepted limitation for now, not an oversight.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// NewAPIKey generates a fresh random API key — used both to fill in a first
// run's config.yaml and by the settings API to regenerate one live (see
// cmd/acervinode's liveSettings.RegenerateAPIKey).
func NewAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
