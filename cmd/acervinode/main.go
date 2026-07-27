// Command acervinode is a debrid download client: it emulates the qBittorrent
// and SABnzbd APIs so *arr apps can use it as a normal download client while it
// resolves everything through a debrid provider instead.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/acervinode/acervinode/internal/api"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
	"github.com/acervinode/acervinode/internal/debrid/torbox"
	"github.com/acervinode/acervinode/internal/importer"
	"github.com/acervinode/acervinode/internal/qbittorrent"
	"github.com/acervinode/acervinode/internal/sabnzbd"
	"github.com/acervinode/acervinode/web"
)

// version is stamped at build time via -ldflags "-X main.version=...", the
// same tag pushed to trigger .github/workflows/release.yml. A plain
// `go build` (or `go run`) without that flag keeps this default.
var version = "0.0.0-dev"

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("acervinode exited with error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	configPath := os.Getenv("ACERVINODE_CONFIG")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(cfg.DownloadDir, 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}
	db, err := database.Open(cfg.DataDir + "/acervinode.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	handler, providers := buildHandler(cfg, db)
	for _, p := range providers {
		slog.Info("provider configured", "name", p.Name, "torrents", p.TorrentCapable, "usenet", p.UsenetCapable)
	}
	// Logged so a config.yaml without an explicit api_key is still usable —
	// otherwise a randomly generated key (see internal/config) would be
	// invisible to whoever needs to type it into Sonarr. Set api_key
	// explicitly once you've picked it, so it survives restarts.
	slog.Info("api key for the native API and both compat shims", "api_key", cfg.APIKey)

	// Completed Download Handling: fetches provider_completed downloads to
	// local disk so *arr apps' import step has real files to find. Built
	// from its own buildProviders() call (a second, independent provider
	// instance from buildHandler's) — simple and harmless, since a provider
	// client is just a thin, stateless HTTP wrapper.
	torrentProvider, usenetProvider, _ := buildProviders(cfg)
	imp := importer.New(db, torrentProvider, usenetProvider, cfg.DownloadDir)
	go imp.Run(ctx, time.Duration(cfg.ImportIntervalSeconds)*time.Second)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: handler,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("acervinode starting", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	slog.Info("acervinode shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// buildHandler assembles the native API plus whichever compat shims the
// configured providers support, all under one *http.ServeMux on one port.
// Split out from run() so tests can exercise the full routing tree without
// binding a real socket.
func buildHandler(cfg *config.Config, db *database.DB) (http.Handler, []api.ProviderStatus) {
	torrentProvider, usenetProvider, providers := buildProviders(cfg)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", api.NewServer(cfg.APIKey, version, providers, db, torrentProvider, usenetProvider))

	if torrentProvider != nil {
		mux.Handle("/api/v2/", qbittorrent.NewServer(torrentProvider, db, cfg.APIKey))
	}
	if usenetProvider != nil {
		// SABnzbd's real API is a single fixed endpoint, not a subtree.
		mux.Handle("/api", sabnzbd.NewServer(usenetProvider, db, cfg.APIKey))
	}

	// The embedded web UI is the lowest-priority route — it only ever
	// receives requests the API/compat-shim patterns above didn't claim.
	if uiHandler, err := web.Handler(); err != nil {
		slog.Error("failed to build embedded web UI handler", "error", err)
	} else {
		mux.Handle("/", uiHandler)
	}

	return mux, providers
}

// buildProviders is the one place a concrete provider package (torbox) is
// referenced outside its own package — adding Real-Debrid later means adding
// a case here, nothing else (see docs/providers.md). A provider that only
// implements debrid.TorrentProvider simply leaves usenetProvider nil, and
// buildHandler above skips mounting the SABnzbd shim rather than erroring.
func buildProviders(cfg *config.Config) (debrid.TorrentProvider, debrid.UsenetProvider, []api.ProviderStatus) {
	torboxCfg, ok := cfg.Providers["torbox"]
	if !ok || torboxCfg.APIKey == "" {
		slog.Warn("no torbox api key configured — compat shims will not be mounted")
		return nil, nil, nil
	}

	return torbox.NewProvider(torboxCfg.APIKey),
		torbox.NewUsenetProvider(torboxCfg.APIKey),
		[]api.ProviderStatus{{Name: "torbox", TorrentCapable: true, UsenetCapable: true}}
}
