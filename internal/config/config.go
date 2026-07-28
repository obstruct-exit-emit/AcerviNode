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
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

func defaults() *Config {
	return &Config{
		Port:                  7846,
		DataDir:               "./data",
		LogLevel:              "info",
		Providers:             map[string]ProviderConfig{},
		DownloadDir:           "./downloads",
		ImportIntervalSeconds: 10,
		ImportMaxRetries:      5,
		CategoryPaths:         map[string]string{},
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
	return nil
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
