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
	"github.com/acervinode/acervinode/internal/tlscert"
	"github.com/acervinode/acervinode/web"
	"sort"
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

	// A *slog.LevelVar (not a fixed level) so log_level can be changed live
	// through the settings API — see liveSettings.UpdateGeneral — without
	// tearing down and rebuilding the logger.
	levelVar := new(slog.LevelVar)
	levelVar.Set(parseLogLevel(cfg.LogLevel))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar})))

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

	registry, settings := setupProviders(cfg, configPath)
	settings.SetLevelVar(levelVar)
	// One-time only (see SeedDefaultCategoriesOnce's own doc comment) —
	// must run before buildHandler/SetShimServers below, so whatever this
	// seeds into CategoryPaths is already there for SetShimServers' own
	// normal re-seed of both compat shims to pick up.
	if err := settings.SeedDefaultCategoriesOnce(); err != nil {
		return fmt.Errorf("seed default categories: %w", err)
	}
	// stop (from signal.NotifyContext above) marks ctx Done exactly the same
	// as an actual signal arriving — wiring it in as the restart trigger
	// means the settings API's restart endpoint needs no shutdown plumbing
	// of its own; it just reuses the select below.
	settings.SetRestartTrigger(stop)
	for _, name := range registry.Names() {
		t := registry.Torrent(name)
		slog.Info("provider", "name", name, "configured", t != nil && t.Configured())
	}
	// Logged so a config.yaml without an explicit api_key is still usable —
	// otherwise a randomly generated key (see internal/config) would be
	// invisible to whoever needs to type it into Sonarr. Set api_key
	// explicitly once you've picked it, so it survives restarts.
	slog.Info("api key for the native API and both compat shims", "api_key", cfg.APIKey)

	handler := buildHandler(db, registry, settings)

	// Completed Download Handling: fetches provider_completed downloads to
	// local disk so *arr apps' import step has real files to find. Shares
	// the same dynamic provider instances as everything else above.
	importInterval := time.Duration(cfg.ImportIntervalSeconds) * time.Second
	imp := importer.New(db, registry, cfg.DownloadDir, importInterval, cfg.ImportMaxRetries)
	settings.SetImporter(imp)
	go imp.Run(ctx)

	// errCh has room for both servers below — either one failing is fatal,
	// but neither should be able to block on sending its error if the other
	// already has.
	errCh := make(chan error, 2)
	startServer := func(name string, srv *http.Server, listenAndServe func() error) {
		go func() {
			if err := listenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server: %w", name, err)
			}
		}()
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: handler,
	}
	servers := []*http.Server{srv}
	slog.Info("acervinode starting", "port", cfg.Port)
	startServer("http", srv, srv.ListenAndServe)

	// TLS is additive, never a replacement for the plain-HTTP listener above
	// — see docs/providers.md's TLS section for why (existing *arr
	// integrations, scripts, and bookmarks pointed at http://... keep working
	// unchanged regardless of whether this is enabled). Generation failure is
	// fatal rather than a silent fall-back to HTTP-only: an operator who
	// explicitly enabled TLS and got silently downgraded would have no
	// indication why the browser's folder-picker still doesn't work.
	if cfg.TLSEnabled {
		certFile, keyFile, err := tlscert.EnsureCertificate(cfg.DataDir, cfg.TLSCertFile, cfg.TLSKeyFile, tlscert.CollectHosts())
		if err != nil {
			return fmt.Errorf("prepare tls certificate: %w", err)
		}
		tlsSrv := &http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.TLSPort),
			Handler: handler,
		}
		servers = append(servers, tlsSrv)
		slog.Info("acervinode starting (tls)", "port", cfg.TLSPort)
		startServer("https", tlsSrv, func() error { return tlsSrv.ListenAndServeTLS(certFile, keyFile) })
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("acervinode shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var shutdownErr error
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}

// buildHandler assembles the native API and both compat shims, all under one
// *http.ServeMux on one port. Both shims are always mounted, regardless of
// whether a provider is configured yet — torrentProvider/usenetProvider are
// the Dynamic*Provider wrappers, which answer every call with
// debrid.ErrNoProvider until a key is set (via config.yaml at startup, or
// later through the settings API), rather than the routes not existing at
// all. Split out from run() so tests can exercise the full routing tree
// without binding a real socket.
//
// settings takes the concrete *liveSettings (not the api.Settings interface)
// because the compat shim servers built here get wired back into it via
// SetShimServers — the settings API's category endpoints read/write their
// category stores directly (see docs/configuration.md).
//
// torrentProvider/usenetProvider/webDownloadProvider take the concrete
// Dynamic*Provider types, not their debrid.TorrentProvider/UsenetProvider/
// WebDownloadProvider interfaces — every call site really does always pass
// the Dynamic wrapper (see setupProviders), and api.NewServer's own
// torrentAdder needs TorrentInfo/CheckCached, which live on the concrete
// wrapper (TorrentInfo delegates via a runtime type assertion against
// whatever the currently-set inner provider is — see
// DynamicTorrentProvider.TorrentInfo) but aren't part of the narrower
// debrid.TorrentProvider interface itself. Passing the concrete type here
// costs nothing at the other call sites below (qbittorrent.NewServer/
// sabnzbd.NewServer's own interface parameters are still satisfied fine).
func buildHandler(db *database.DB, registry *debrid.Registry, settings *liveSettings) http.Handler {
	mux := http.NewServeMux()

	// The shims still take one provider each; they resolve through the
	// registry in a later increment. With a single provider these are the
	// same objects the native API reaches through it.
	qbtServer := qbittorrent.NewServer(registry.Torrent(registry.Default()), db, settings)
	sabServer := sabnzbd.NewServer(registry.Usenet(registry.Default()), db, settings)
	settings.SetShimServers(qbtServer, sabServer)

	mux.Handle("/api/v1/", api.NewServer(version, db, registry, settings))
	mux.Handle("/api/v2/", qbtServer)
	// SABnzbd's real API is a single fixed endpoint, not a subtree.
	mux.Handle("/api", sabServer)

	// The embedded web UI is the lowest-priority route — it only ever
	// receives requests the API/compat-shim patterns above didn't claim.
	if uiHandler, err := web.Handler(); err != nil {
		slog.Error("failed to build embedded web UI handler", "error", err)
	} else {
		mux.Handle("/", uiHandler)
	}

	return mux
}

// parseLogLevel maps config's log_level string onto a slog.Level — config
// validates the string is one of these four (see config.Config.Validate),
// so the default case here is unreachable in practice, not a silent typo
// fallback.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newTorBoxProviders is the one place a concrete provider package (torbox) is
// referenced outside its own package — adding Real-Debrid later means adding
// a case here (and in liveSettings), nothing else (see docs/providers.md).
// requestTimeout is applied to every one of the three providers' underlying
// *torbox.Client — see torbox.WithRequestTimeout.
func newTorBoxProviders(apiKey string, requestTimeout time.Duration) (debrid.TorrentProvider, debrid.UsenetProvider, debrid.WebDownloadProvider) {
	opt := torbox.WithRequestTimeout(requestTimeout)
	return torbox.NewProvider(apiKey, opt), torbox.NewUsenetProvider(apiKey, opt), torbox.NewWebDownloadProvider(apiKey, opt)
}

// providerConstructor builds one provider's three per-kind implementations
// from an API key. A provider that doesn't support a kind returns nil for
// it — nothing here assumes every provider does all three.
type providerConstructor func(apiKey string, requestTimeout time.Duration) (debrid.TorrentProvider, debrid.UsenetProvider, debrid.WebDownloadProvider)

// knownProviders is every provider type AcerviNode can actually construct,
// keyed by the name used in config.Providers. Adding a second debrid
// service is an entry here plus its implementation of the debrid
// interfaces — deliberately the only place a provider name is written down
// in wiring, rather than repeated at each Dynamic*Provider construction as
// it used to be.
var knownProviders = map[string]providerConstructor{
	"torbox": newTorBoxProviders,
}

// knownProviderNames lists knownProviders in a stable order, so registry
// ordering (and therefore GET /api/v1/providers) doesn't reshuffle between
// runs on Go's randomised map iteration.
func knownProviderNames() []string {
	names := make([]string, 0, len(knownProviders))
	for name := range knownProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// setupProviders builds the provider registry: one Dynamic*Provider per
// provider per kind, which together are the single shared source of truth
// for "which credentials are currently active". Every consumer (both compat
// shims, the native API, and the importer) resolves through the same
// instances, so a key set later through the settings API takes effect for
// all of them at once, live, with no restart.
//
// Every known provider type is registered whether or not it has a key yet:
// the wrapper *is* the slot a key gets set into, so it has to exist before
// one is configured. A provider with no key simply answers
// debrid.ErrNoProvider until it has one.
//
// Split out from run() so tests can build the same wiring without going
// through the full startup sequence.
func setupProviders(cfg *config.Config, configPath string) (*debrid.Registry, *liveSettings) {
	registry := debrid.NewRegistry()
	timeout := time.Duration(cfg.ProviderRequestTimeoutSeconds) * time.Second

	for _, name := range knownProviderNames() {
		t := debrid.NewDynamicTorrentProvider(name)
		u := debrid.NewDynamicUsenetProvider(name)
		w := debrid.NewDynamicWebDownloadProvider(name)
		if pc, ok := cfg.Providers[name]; ok && pc.APIKey != "" {
			tp, up, wp := knownProviders[name](pc.APIKey, timeout)
			t.Set(tp)
			u.Set(up)
			w.Set(wp)
		}
		registry.Register(name, t, u, w)
	}

	// A configured name nothing knows how to build is worth saying out
	// loud — silently ignoring it would look exactly like a provider that
	// simply never works.
	for name := range cfg.Providers {
		if _, known := knownProviders[name]; !known {
			slog.Warn("unknown provider in config, ignoring", "provider", name)
		}
	}

	settings := &liveSettings{cfg: cfg, configPath: configPath, registry: registry}
	return registry, settings
}
