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

	torrentDyn, usenetDyn, settings := setupProviders(cfg, configPath)
	slog.Info("torbox provider", "configured", torrentDyn.Configured())
	// Logged so a config.yaml without an explicit api_key is still usable —
	// otherwise a randomly generated key (see internal/config) would be
	// invisible to whoever needs to type it into Sonarr. Set api_key
	// explicitly once you've picked it, so it survives restarts.
	slog.Info("api key for the native API and both compat shims", "api_key", cfg.APIKey)

	handler := buildHandler(cfg, db, torrentDyn, usenetDyn, settings)

	// Completed Download Handling: fetches provider_completed downloads to
	// local disk so *arr apps' import step has real files to find. Shares
	// the same dynamic provider instances as everything else above.
	imp := importer.New(db, torrentDyn, usenetDyn, cfg.DownloadDir)
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

// buildHandler assembles the native API and both compat shims, all under one
// *http.ServeMux on one port. Both shims are always mounted, regardless of
// whether a provider is configured yet — torrentProvider/usenetProvider are
// the Dynamic*Provider wrappers, which answer every call with
// debrid.ErrNoProvider until a key is set (via config.yaml at startup, or
// later through the settings API), rather than the routes not existing at
// all. Split out from run() so tests can exercise the full routing tree
// without binding a real socket.
func buildHandler(cfg *config.Config, db *database.DB, torrentProvider debrid.TorrentProvider, usenetProvider debrid.UsenetProvider, settings api.Settings) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", api.NewServer(cfg.APIKey, version, db, torrentProvider, usenetProvider, settings))
	mux.Handle("/api/v2/", qbittorrent.NewServer(torrentProvider, db, cfg.APIKey))
	// SABnzbd's real API is a single fixed endpoint, not a subtree.
	mux.Handle("/api", sabnzbd.NewServer(usenetProvider, db, cfg.APIKey))

	// The embedded web UI is the lowest-priority route — it only ever
	// receives requests the API/compat-shim patterns above didn't claim.
	if uiHandler, err := web.Handler(); err != nil {
		slog.Error("failed to build embedded web UI handler", "error", err)
	} else {
		mux.Handle("/", uiHandler)
	}

	return mux
}

// newTorBoxProviders is the one place a concrete provider package (torbox) is
// referenced outside its own package — adding Real-Debrid later means adding
// a case here (and in liveSettings), nothing else (see docs/providers.md).
func newTorBoxProviders(apiKey string) (debrid.TorrentProvider, debrid.UsenetProvider) {
	return torbox.NewProvider(apiKey), torbox.NewUsenetProvider(apiKey)
}

// setupProviders builds the Dynamic*Provider wrappers that are the single
// shared source of truth for "which TorBox credentials are currently
// active" — every consumer (both compat shims, the native API, and the
// importer) holds the same instances, so a key set later through the
// settings API takes effect for all of them at once, live, with no restart.
// Pre-populated from cfg if a key is already there. Split out from run() so
// tests can build the same wiring without going through the full startup
// sequence.
func setupProviders(cfg *config.Config, configPath string) (*debrid.DynamicTorrentProvider, *debrid.DynamicUsenetProvider, *liveSettings) {
	torrentDyn := debrid.NewDynamicTorrentProvider("torbox")
	usenetDyn := debrid.NewDynamicUsenetProvider("torbox")
	if torboxCfg, ok := cfg.Providers["torbox"]; ok && torboxCfg.APIKey != "" {
		tp, up := newTorBoxProviders(torboxCfg.APIKey)
		torrentDyn.Set(tp)
		usenetDyn.Set(up)
	}
	settings := &liveSettings{cfg: cfg, configPath: configPath, torrentDyn: torrentDyn, usenetDyn: usenetDyn}
	return torrentDyn, usenetDyn, settings
}
