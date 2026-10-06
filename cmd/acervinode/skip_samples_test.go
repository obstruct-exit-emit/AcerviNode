package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/api"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/importer"
)

// TestLiveSettings_SkipSampleFiles -- on at startup by default, switchable
// live from Settings, persisted, and reported back in GeneralInfo.
func TestLiveSettings_SkipSampleFiles(t *testing.T) {
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
	registry, settings := setupProviders(cfg, configPath)

	imp := importer.New(db, registry, t.TempDir(), time.Minute, 5)
	settings.SetImporter(imp)
	if !imp.SkipSamples() {
		t.Fatal("SkipSamples() = false at startup, want the default true applied")
	}
	if !settings.General().SkipSampleFiles {
		t.Error("GeneralInfo.SkipSampleFiles = false, want true reported")
	}

	update := func(skip bool) {
		t.Helper()
		g := settings.General()
		if _, err := settings.UpdateGeneral(context.Background(), api.GeneralUpdate{
			Port: g.Port, DataDir: g.DataDir, DownloadDir: g.DownloadDir, LogLevel: g.LogLevel,
			ImportIntervalSeconds: g.ImportIntervalSeconds, ImportMaxRetries: g.ImportMaxRetries,
			MaxConcurrentDownloads: g.MaxConcurrentDownloads, ImportFetchTimeoutSeconds: g.ImportFetchTimeoutSeconds,
			DownloadDirMode: g.DownloadDirMode, FastPollIntervalSeconds: g.FastPollIntervalSeconds,
			ProviderRequestTimeoutSeconds: g.ProviderRequestTimeoutSeconds, TLSPort: g.TLSPort,
			BackupIntervalHours: g.BackupIntervalHours, BackupKeep: g.BackupKeep,
			SkipSampleFiles: skip,
		}); err != nil {
			t.Fatalf("UpdateGeneral(skip=%v) error = %v", skip, err)
		}
	}

	update(false)
	if imp.SkipSamples() {
		t.Error("SkipSamples() = true after switching off, want it applied live")
	}
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.SkipSampleFiles {
		t.Error("persisted SkipSampleFiles = true, want false saved")
	}

	update(true)
	if !imp.SkipSamples() {
		t.Error("SkipSamples() = false after switching back on, want it applied live")
	}
}
