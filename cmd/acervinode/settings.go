package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/acervinode/acervinode/internal/api"
	"github.com/acervinode/acervinode/internal/config"
	"github.com/acervinode/acervinode/internal/debrid"
)

// liveSettings implements api.Settings: it's what lets a TorBox API key set
// through the web UI take effect immediately (via the Dynamic*Provider
// wrappers already shared with every other consumer — see run() in main.go)
// and persist across a restart (by rewriting config.yaml).
type liveSettings struct {
	mu         sync.Mutex
	cfg        *config.Config
	configPath string
	torrentDyn *debrid.DynamicTorrentProvider
	usenetDyn  *debrid.DynamicUsenetProvider
}

func (s *liveSettings) TorBoxConfigured() bool {
	return s.torrentDyn.Configured()
}

func (s *liveSettings) SetTorBoxAPIKey(_ context.Context, apiKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	torrentProvider, usenetProvider := newTorBoxProviders(apiKey)
	s.torrentDyn.Set(torrentProvider)
	s.usenetDyn.Set(usenetProvider)

	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]config.ProviderConfig{}
	}
	s.cfg.Providers["torbox"] = config.ProviderConfig{APIKey: apiKey}
	if err := s.cfg.Save(s.configPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

// APIKey returns AcerviNode's own current API key. This is what every
// authenticated route (native API and both compat shims) checks a request
// against instead of a value captured at startup — see their apiKeySource
// interfaces — so RegenerateAPIKey takes effect everywhere at once.
func (s *liveSettings) APIKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.APIKey
}

// RegenerateAPIKey replaces the current API key with a fresh random one,
// applies it immediately, and persists it to config.yaml — the same
// live-swap-then-save pattern as SetTorBoxAPIKey.
func (s *liveSettings) RegenerateAPIKey(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key, err := config.NewAPIKey()
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	s.cfg.APIKey = key
	if err := s.cfg.Save(s.configPath); err != nil {
		return "", fmt.Errorf("persist config: %w", err)
	}
	return key, nil
}

// General reports the rest of AcerviNode's current configuration for the
// settings UI — see api.GeneralInfo.
func (s *liveSettings) General() api.GeneralInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return api.GeneralInfo{
		APIKey:                s.cfg.APIKey,
		Port:                  s.cfg.Port,
		DataDir:               s.cfg.DataDir,
		DownloadDir:           s.cfg.DownloadDir,
		LogLevel:              s.cfg.LogLevel,
		ImportIntervalSeconds: s.cfg.ImportIntervalSeconds,
		ImportMaxRetries:      s.cfg.ImportMaxRetries,
	}
}
