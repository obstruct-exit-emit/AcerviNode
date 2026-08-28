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
	"github.com/acervinode/acervinode/internal/backup"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
	"github.com/acervinode/acervinode/internal/debrid/alldebrid"
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

	// Snapshots of both stores. The database holds the download history;
	// the config file holds the provider keys, the API key and every login
	// account, so a snapshot of one without the other restores half of
	// what was lost.
	backupRunner := backup.New(
		db,
		cfg.ResolvedBackupDir(),
		configPath,
		time.Duration(cfg.BackupIntervalHours)*time.Hour,
		cfg.BackupKeep,
	)
	settings.SetBackupRunner(backupRunner)
	go backupRunner.Run(ctx)

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

	qbtServer := qbittorrent.NewServer(registry, db, settings)
	sabServer := sabnzbd.NewServer(registry, db, settings)
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
// newTorBoxProviders ignores name: torbox.Provider reports a fixed "torbox"
// of its own, and everything that routes by provider goes through the
// registry's wrapper, which carries the entry name. Taken anyway so every
// constructor has one shape.
func newTorBoxProviders(_, apiKey string, requestTimeout time.Duration) (debrid.TorrentProvider, debrid.UsenetProvider, debrid.WebDownloadProvider) {
	opt := torbox.WithRequestTimeout(requestTimeout)
	return torbox.NewProvider(apiKey, opt), torbox.NewUsenetProvider(apiKey, opt), torbox.NewWebDownloadProvider(apiKey, opt)
}

// newAllDebridProviders builds AllDebrid's torrent and web-download
// providers. Usenet is nil: AllDebrid has no usenet service at all, so it
// registers no wrapper for that kind rather than one that always errors —
// see debrid.Registry.
func newAllDebridProviders(name, apiKey string, requestTimeout time.Duration) (debrid.TorrentProvider, debrid.UsenetProvider, debrid.WebDownloadProvider) {
	return alldebrid.NewProvider(name, apiKey, alldebrid.WithRequestTimeout(requestTimeout)),
		nil,
		alldebrid.NewWebDownloadProvider(name, apiKey, alldebrid.WithRequestTimeout(requestTimeout))
}

// providerConstructor builds one provider's three per-kind implementations
// from an API key. A provider that doesn't support a kind returns nil for
// it — nothing here assumes every provider does all three.
type providerConstructor func(name, apiKey string, requestTimeout time.Duration) (debrid.TorrentProvider, debrid.UsenetProvider, debrid.WebDownloadProvider)

// knownProviders is every provider type AcerviNode can actually construct,
// keyed by the name used in config.Providers. Adding a second debrid
// service is an entry here plus its implementation of the debrid
// interfaces — deliberately the only place a provider name is written down
// in wiring, rather than repeated at each Dynamic*Provider construction as
// it used to be.
var knownProviders = map[string]providerConstructor{
	"torbox":    newTorBoxProviders,
	"alldebrid": newAllDebridProviders,
}

// providerCapabilities is which kinds a provider implementation actually
// supports. Not every service does all three — AllDebrid has no usenet at
// all — and a provider that doesn't support a kind registers no wrapper for
// it rather than one that always errors, so it simply never appears for
// that kind.
type providerCapabilities struct {
	torrent bool
	usenet  bool
	webdl   bool
}

// knownProviderCapabilities records what each implementation can do. Kept
// beside knownProviders so adding a service means touching one place.
var knownProviderCapabilities = map[string]providerCapabilities{
	"torbox": {torrent: true, usenet: true, webdl: true},
	// AllDebrid has no usenet service at all, so it registers no usenet
	// wrapper and simply never appears for that kind — see debrid.Registry.
	// Its hoster-link support is real but works differently from a service
	// that tracks a web download object; see
	// internal/debrid/alldebrid.WebDownloadProvider for how that maps.
	"alldebrid": {torrent: true, webdl: true},
}

// registerProviderEntry builds one provider entry's wrappers and registers
// them. Shared by startup and by the settings API, which can add an entry at
// runtime — so both paths produce identical wiring rather than one of them
// reimplementing it slightly differently.
//
// An empty apiKey still registers: the wrapper *is* the slot a key gets set
// into later, so it has to exist before one is configured.
// registerProviderEntry registers one provider entry for the kinds it both
// supports and is enabled for. enabledKind reports the latter; pass nil to
// enable everything supported, which is what a caller with no config in hand
// means.
func registerProviderEntry(registry *debrid.Registry, name, typeName, apiKey string, timeout time.Duration, enabledKind func(kind string) bool) error {
	construct, known := knownProviders[typeName]
	if !known {
		return fmt.Errorf("unknown provider type %q", typeName)
	}
	if enabledKind == nil {
		enabledKind = func(string) bool { return true }
	}

	var tp debrid.TorrentProvider
	var up debrid.UsenetProvider
	var wp debrid.WebDownloadProvider
	if apiKey != "" {
		tp, up, wp = construct(name, apiKey, timeout)
	}
	// A provider that doesn't support a kind contributes no wrapper for it,
	// so it simply never appears for that kind — see debrid.Registry.
	var t *debrid.DynamicTorrentProvider
	var u *debrid.DynamicUsenetProvider
	var w *debrid.DynamicWebDownloadProvider
	caps := knownProviderCapabilities[typeName]
	if caps.torrent && enabledKind("torrent") {
		t = debrid.NewDynamicTorrentProvider(name)
		t.Set(tp)
	}
	if caps.usenet && enabledKind("usenet") {
		u = debrid.NewDynamicUsenetProvider(name)
		u.Set(up)
	}
	if caps.webdl && enabledKind("webdl") {
		w = debrid.NewDynamicWebDownloadProvider(name)
		w.Set(wp)
	}
	// SetKinds rather than Register: this is also the path a live toggle
	// takes, where a kind switched off has to actually be removed rather
	// than left in place.
	registry.SetKinds(name, t, u, w)
	return nil
}

// providerIdentity is what makes two config entries the same account: the
// implementation they use plus the credentials they use it with.
type providerIdentity struct {
	providerType string
	apiKey       string
}

// warnOnDuplicateProviderKeys flags two entries pointing at the same
// account, which is almost always a mistake rather than an intent.
//
// Multiple entries exist so several *different* accounts can be used at
// once. Pointing two at one account instead makes both discover everything
// on it, so every download is adopted twice, and deleting it through one
// entry strands the other's row as "no longer found in the provider's
// account". Observed exactly that while testing two entries against a
// single TorBox key.
//
// A warning rather than a refusal: it is a coherent thing to ask for even
// if it is rarely what someone means, and refusing to start over it would
// be a worse failure than saying so.
func warnOnDuplicateProviderKeys(cfg *config.Config) {
	seen := map[providerIdentity]string{}
	for _, name := range providerEntryNames(cfg) {
		pc, ok := cfg.Providers[name]
		if !ok || pc.APIKey == "" {
			continue
		}
		key := providerIdentity{providerType: pc.ResolvedType(name), apiKey: pc.APIKey}
		if first, dup := seen[key]; dup {
			slog.Warn("two providers are configured with the same credentials, so both will discover the same downloads",
				"provider", name, "duplicate_of", first)
			continue
		}
		seen[key] = name
	}
}

// defaultProviderName picks which provider new downloads go to: the
// explicitly configured one when it is actually registered, otherwise the
// first registered provider that holds credentials, otherwise nothing (and
// Registry keeps its own first-registered fallback).
func defaultProviderName(cfg *config.Config, registry *debrid.Registry) string {
	names := registry.Names()
	registered := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	if cfg.DefaultProvider != "" && registered(cfg.DefaultProvider) {
		return cfg.DefaultProvider
	}
	for _, name := range names {
		if pc, ok := cfg.Providers[name]; ok && pc.APIKey != "" {
			return name
		}
	}
	return ""
}

// knownProviderTypes lists the provider implementations this build can
// construct, sorted so the settings UI's picker is stable rather than
// following Go's randomised map iteration.
func knownProviderTypes() []string {
	names := make([]string, 0, len(knownProviders))
	for name := range knownProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// providerEntryNames lists every provider entry to register, in a stable
// order: each configured entry, plus every known type that has no entry yet
// so a fresh install still has a slot to put a key into. Stable because
// registry order is user-visible (GET /api/v1/providers) and Go's map
// iteration is randomised.
func providerEntryNames(cfg *config.Config) []string {
	seen := map[string]bool{}
	var names []string
	for name := range cfg.Providers {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for name := range knownProviders {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
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

	// Registered by *entry*, not by implementation type, so two accounts on
	// the same service can coexist: entries named "torbox" and
	// "torbox-work" both resolving to type "torbox" are two independent
	// providers, each with its own credentials, listing cache and
	// rate-limit backoff.
	//
	// Every known type is also registered under its own name even with
	// nothing in config, so a fresh install has somewhere to put a key —
	// the wrapper *is* the slot a key gets set into.
	for _, name := range providerEntryNames(cfg) {
		typeName := name
		apiKey := ""
		// An entry with no config at all is a known provider nobody has
		// touched yet: every kind it supports is enabled, since disabling
		// is something you opt into.
		enabled := func(string) bool { return true }
		if pc, ok := cfg.Providers[name]; ok {
			typeName = pc.ResolvedType(name)
			apiKey = pc.APIKey
			enabled = pc.KindEnabled
		}
		if err := registerProviderEntry(registry, name, typeName, apiKey, timeout, enabled); err != nil {
			slog.Warn("ignoring provider entry", "provider", name, "type", typeName, "error", err)
		}
	}

	warnOnDuplicateProviderKeys(cfg)

	// Registry's own fallback is "first registered", which is the wrong
	// answer once more than one provider type is known: every known type is
	// registered whether or not it has a key, so that fallback can land on
	// a provider with no credentials while a configured one sits right next
	// to it, and every add would fail against it. Prefer a configured
	// provider, and only fall back to registration order when nothing is
	// configured at all.
	registry.SetDefault(defaultProviderName(cfg, registry))

	settings := &liveSettings{cfg: cfg, configPath: configPath, registry: registry}
	return registry, settings
}
