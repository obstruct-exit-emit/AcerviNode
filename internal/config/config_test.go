package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoad_DefaultsWithNoFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 7846 {
		t.Errorf("Port = %d, want 7846", cfg.Port)
	}
	if cfg.DataDir != "./data" {
		t.Errorf("DataDir = %q, want ./data", cfg.DataDir)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.APIKey == "" {
		t.Error("APIKey should be generated when unset, got empty string")
	}
	if cfg.DownloadDir != "./downloads" {
		t.Errorf("DownloadDir = %q, want ./downloads", cfg.DownloadDir)
	}
	if cfg.ImportIntervalSeconds != 10 {
		t.Errorf("ImportIntervalSeconds = %d, want 10", cfg.ImportIntervalSeconds)
	}
	if cfg.ImportMaxRetries != 5 {
		t.Errorf("ImportMaxRetries = %d, want 5", cfg.ImportMaxRetries)
	}
}

func TestLoad_GeneratedAPIKeysAreUnique(t *testing.T) {
	cfg1, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg1.APIKey == cfg2.APIKey {
		t.Error("expected two independently generated API keys to differ")
	}
}

func TestLoad_FromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
port: 9000
data_dir: /tmp/acervinode-data
api_key: from-file
log_level: debug
providers:
  torbox:
    api_key: torbox-key-from-file
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 9000 {
		t.Errorf("Port = %d, want 9000", cfg.Port)
	}
	if cfg.DataDir != "/tmp/acervinode-data" {
		t.Errorf("DataDir = %q, want /tmp/acervinode-data", cfg.DataDir)
	}
	if cfg.APIKey != "from-file" {
		t.Errorf("APIKey = %q, want from-file", cfg.APIKey)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if got := cfg.Providers["torbox"].APIKey; got != "torbox-key-from-file" {
		t.Errorf("Providers[torbox].APIKey = %q, want torbox-key-from-file", got)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "port: 9000\n")

	t.Setenv("ACERVINODE_PORT", "7000")
	t.Setenv("ACERVINODE_LOG_LEVEL", "warn")
	t.Setenv("ACERVINODE_PROVIDERS_TORBOX_API_KEY", "torbox-key-from-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 7000 {
		t.Errorf("Port = %d, want 7000 (env override)", cfg.Port)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (env override)", cfg.LogLevel)
	}
	if got := cfg.Providers["torbox"].APIKey; got != "torbox-key-from-env" {
		t.Errorf("Providers[torbox].APIKey = %q, want torbox-key-from-env", got)
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "port: 0\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid port, got nil")
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "log_level: verbose\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid log_level, got nil")
	}
}

func TestLoad_InvalidImportInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "import_interval_seconds: 0\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid import_interval_seconds, got nil")
	}
}

func TestLoad_DownloadDirEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_DOWNLOAD_DIR", "/data/downloads")
	t.Setenv("ACERVINODE_IMPORT_INTERVAL_SECONDS", "30")
	t.Setenv("ACERVINODE_IMPORT_MAX_RETRIES", "3")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DownloadDir != "/data/downloads" {
		t.Errorf("DownloadDir = %q, want /data/downloads", cfg.DownloadDir)
	}
	if cfg.ImportIntervalSeconds != 30 {
		t.Errorf("ImportIntervalSeconds = %d, want 30", cfg.ImportIntervalSeconds)
	}
	if cfg.ImportMaxRetries != 3 {
		t.Errorf("ImportMaxRetries = %d, want 3", cfg.ImportMaxRetries)
	}
}

func TestLoad_InvalidImportMaxRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "import_max_retries: 0\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid import_max_retries, got nil")
	}
}

// TestLoad_CleanupAfterDaysDefaultsToDisabled proves cleanup_after_days
// defaults to 0 (disabled) rather than some always-on positive default —
// unlike every other numeric setting in this config, 0 is a meaningful,
// safe "off" value here, not something Validate rejects.
func TestLoad_CleanupAfterDaysDefaultsToDisabled(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CleanupAfterDays != 0 {
		t.Errorf("CleanupAfterDays default = %d, want 0 (disabled)", cfg.CleanupAfterDays)
	}
}

func TestLoad_CleanupAfterDaysEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_CLEANUP_AFTER_DAYS", "14")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CleanupAfterDays != 14 {
		t.Errorf("CleanupAfterDays = %d, want 14", cfg.CleanupAfterDays)
	}
}

func TestLoad_InvalidNegativeCleanupAfterDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "cleanup_after_days: -1\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for negative cleanup_after_days, got nil")
	}
}

func TestLoad_DownloadDirModeDefaultsToWorldWritable(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DownloadDirMode != "0777" {
		t.Errorf("DownloadDirMode default = %q, want \"0777\"", cfg.DownloadDirMode)
	}
}

func TestLoad_DownloadDirModeEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_DOWNLOAD_DIR_MODE", "0750")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DownloadDirMode != "0750" {
		t.Errorf("DownloadDirMode = %q, want \"0750\"", cfg.DownloadDirMode)
	}
}

func TestLoad_InvalidDownloadDirMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "download_dir_mode: \"not-octal\"\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid download_dir_mode, got nil")
	}
}

func TestParseDirMode(t *testing.T) {
	tests := []struct {
		in      string
		want    os.FileMode
		wantErr bool
	}{
		{"0777", 0o777, false},
		{"777", 0o777, false}, // no leading zero required
		{"0755", 0o755, false},
		{"0000", 0, false},
		{"", 0, true},
		{"not-octal", 0, true},
		{"0888", 0, true},  // 8/9 aren't valid octal digits
		{"01000", 0, true}, // above 0777 — setuid/setgid/sticky out of scope
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDirMode(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDirMode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseDirMode(%q) = %o, want %o", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoad_FastPollIntervalDefaultsTo3Seconds(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FastPollIntervalSeconds != 3 {
		t.Errorf("FastPollIntervalSeconds default = %d, want 3", cfg.FastPollIntervalSeconds)
	}
}

func TestLoad_FastPollIntervalEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_FAST_POLL_INTERVAL_SECONDS", "7")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FastPollIntervalSeconds != 7 {
		t.Errorf("FastPollIntervalSeconds = %d, want 7", cfg.FastPollIntervalSeconds)
	}
}

func TestLoad_InvalidFastPollInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "fast_poll_interval_seconds: 0\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for fast_poll_interval_seconds < 1, got nil")
	}
}

// TestLoad_TLSPortDefaultsTo8443 proves TLSPort gets a real default even
// though tls_enabled defaults to false — so simply flipping tls_enabled on
// later (without also having to set tls_port) lands on a sane port instead
// of 0.
func TestLoad_TLSPortDefaultsTo8443(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TLSEnabled {
		t.Error("TLSEnabled default = true, want false")
	}
	if cfg.TLSPort != 8443 {
		t.Errorf("TLSPort default = %d, want 8443", cfg.TLSPort)
	}
}

func TestLoad_TLSEnvOverrides(t *testing.T) {
	t.Setenv("ACERVINODE_TLS_ENABLED", "true")
	t.Setenv("ACERVINODE_TLS_PORT", "9443")
	t.Setenv("ACERVINODE_TLS_CERT_FILE", "/etc/acervinode/cert.pem")
	t.Setenv("ACERVINODE_TLS_KEY_FILE", "/etc/acervinode/key.pem")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.TLSEnabled {
		t.Error("TLSEnabled = false, want true (env override)")
	}
	if cfg.TLSPort != 9443 {
		t.Errorf("TLSPort = %d, want 9443", cfg.TLSPort)
	}
	if cfg.TLSCertFile != "/etc/acervinode/cert.pem" {
		t.Errorf("TLSCertFile = %q, want /etc/acervinode/cert.pem", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/etc/acervinode/key.pem" {
		t.Errorf("TLSKeyFile = %q, want /etc/acervinode/key.pem", cfg.TLSKeyFile)
	}
}

func TestLoad_InvalidTLSPortWhenEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "tls_enabled: true\ntls_port: 0\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid tls_port when tls_enabled, got nil")
	}
}

// TestLoad_TLSPortIgnoredWhenDisabled proves an out-of-range tls_port
// doesn't block startup while tls_enabled is false — nothing binds to it,
// so it's not worth validating until it would actually matter.
func TestLoad_TLSPortIgnoredWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "tls_enabled: false\ntls_port: 0\n")

	if _, err := Load(path); err != nil {
		t.Errorf("Load() error = %v, want nil (tls_port unchecked while disabled)", err)
	}
}

func TestLoad_TLSPortMustDifferFromPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "port: 7846\ntls_enabled: true\ntls_port: 7846\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error when tls_port equals port, got nil")
	}
}

func TestLoad_TLSCertAndKeyMustBeSetTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "tls_cert_file: /etc/acervinode/cert.pem\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for tls_cert_file without tls_key_file, got nil")
	}
}

func TestLoad_FileFilteringAndWatchdogSettingsDefaultToDisabled(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MinFetchFileSizeBytes != 0 {
		t.Errorf("MinFetchFileSizeBytes default = %d, want 0 (disabled)", cfg.MinFetchFileSizeBytes)
	}
	if cfg.IncludeFileRegex != "" {
		t.Errorf("IncludeFileRegex default = %q, want empty (disabled)", cfg.IncludeFileRegex)
	}
	if cfg.ExcludeFileRegex != "" {
		t.Errorf("ExcludeFileRegex default = %q, want empty (disabled)", cfg.ExcludeFileRegex)
	}
	if cfg.StuckDownloadTimeoutMinutes != 0 {
		t.Errorf("StuckDownloadTimeoutMinutes default = %d, want 0 (disabled)", cfg.StuckDownloadTimeoutMinutes)
	}
	if cfg.CleanupErrorAfterDays != 0 {
		t.Errorf("CleanupErrorAfterDays default = %d, want 0 (disabled)", cfg.CleanupErrorAfterDays)
	}
}

func TestLoad_MinFetchFileSizeBytesEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_MIN_FETCH_FILE_SIZE_BYTES", "5242880")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MinFetchFileSizeBytes != 5242880 {
		t.Errorf("MinFetchFileSizeBytes = %d, want 5242880", cfg.MinFetchFileSizeBytes)
	}
}

func TestLoad_IncludeExcludeFileRegexEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_INCLUDE_FILE_REGEX", `\.mkv$`)
	t.Setenv("ACERVINODE_EXCLUDE_FILE_REGEX", "sample")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.IncludeFileRegex != `\.mkv$` {
		t.Errorf("IncludeFileRegex = %q, want %q", cfg.IncludeFileRegex, `\.mkv$`)
	}
	if cfg.ExcludeFileRegex != "sample" {
		t.Errorf("ExcludeFileRegex = %q, want %q", cfg.ExcludeFileRegex, "sample")
	}
}

func TestLoad_StuckDownloadTimeoutMinutesEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_STUCK_DOWNLOAD_TIMEOUT_MINUTES", "180")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StuckDownloadTimeoutMinutes != 180 {
		t.Errorf("StuckDownloadTimeoutMinutes = %d, want 180", cfg.StuckDownloadTimeoutMinutes)
	}
}

func TestLoad_CleanupErrorAfterDaysEnvOverride(t *testing.T) {
	t.Setenv("ACERVINODE_CLEANUP_ERROR_AFTER_DAYS", "3")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CleanupErrorAfterDays != 3 {
		t.Errorf("CleanupErrorAfterDays = %d, want 3", cfg.CleanupErrorAfterDays)
	}
}

func TestLoad_InvalidNegativeMinFetchFileSizeBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "min_fetch_file_size_bytes: -1\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for negative min_fetch_file_size_bytes, got nil")
	}
}

func TestLoad_InvalidIncludeFileRegex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "include_file_regex: \"[\"\n") // unclosed character class

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid include_file_regex, got nil")
	}
}

func TestLoad_InvalidExcludeFileRegex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "exclude_file_regex: \"(\"\n") // unclosed group

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for invalid exclude_file_regex, got nil")
	}
}

func TestLoad_InvalidNegativeStuckDownloadTimeoutMinutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "stuck_download_timeout_minutes: -1\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for negative stuck_download_timeout_minutes, got nil")
	}
}

func TestLoad_InvalidNegativeCleanupErrorAfterDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "cleanup_error_after_days: -1\n")

	if _, err := Load(path); err == nil {
		t.Error("Load() expected error for negative cleanup_error_after_days, got nil")
	}
}

func TestSave_RoundTripsThroughLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.Providers["torbox"] = ProviderConfig{APIKey: "new-torbox-key"}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Save() error = %v", err)
	}
	if got := reloaded.Providers["torbox"].APIKey; got != "new-torbox-key" {
		t.Errorf("Providers[torbox].APIKey = %q, want new-torbox-key", got)
	}
	if reloaded.Port != cfg.Port || reloaded.APIKey != cfg.APIKey {
		t.Errorf("reloaded config = %+v, want it to match saved %+v", reloaded, cfg)
	}
}

func TestSave_FilePermissionsAreRestrictive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows doesn't enforce POSIX permission bits — os.WriteFile's mode argument is a no-op there")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config file permissions = %o, want no group/other access", perm)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
}
