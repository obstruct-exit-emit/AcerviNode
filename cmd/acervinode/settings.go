package main

import (
	"context"
	"fmt"
	"sync"

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
