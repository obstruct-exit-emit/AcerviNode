package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_SkipSampleFilesDefaultsOn -- sample skipping is on unless switched
// off. It is safe to default on because it never drops a download's main file
// (see importer.dropSamples).
func TestLoad_SkipSampleFilesDefaultsOn(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.SkipSampleFiles {
		t.Error("SkipSampleFiles default = false, want true")
	}
}

// TestLoad_SkipSampleFilesOnForAnExistingConfig -- every install that already
// has a config.yaml predates the key. Upgrading must turn it on there too,
// not only on fresh installs.
func TestLoad_SkipSampleFilesOnForAnExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 7846\nlog_level: info\nexclude_file_regex: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.SkipSampleFiles {
		t.Error("SkipSampleFiles = false for a config without the key, want the default true")
	}
}

// TestLoad_SkipSampleFilesCanBeSwitchedOff -- in the file and by environment.
func TestLoad_SkipSampleFilesCanBeSwitchedOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("skip_sample_files: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SkipSampleFiles {
		t.Error("SkipSampleFiles = true, want false as the file says")
	}

	t.Setenv("ACERVINODE_SKIP_SAMPLE_FILES", "false")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SkipSampleFiles {
		t.Error("SkipSampleFiles = true, want false from ACERVINODE_SKIP_SAMPLE_FILES")
	}
}
