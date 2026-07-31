package qbittorrent

import "net/http"

// These are plain, fixed version strings — *arr apps check webapiVersion
// against a minimum they support, and AcerviNode only needs to clear that
// bar, not be a real qBittorrent build.
const (
	fakeAppVersion    = "v4.6.0"
	fakeWebAPIVersion = "2.9.3"
)

func (s *Server) handleAppVersion(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusOK, fakeAppVersion)
}

func (s *Server) handleWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusOK, fakeWebAPIVersion)
}

// preferencesResponse is the subset of qBittorrent's real
// GET /api/v2/app/preferences response that Sonarr/Radarr's own
// QBittorrentPreferences model actually deserializes (confirmed against
// Sonarr's real source — QBittorrentProxyV2.GetConfig, called by
// TestConnection, the very first step of a download client's "Test"). Every
// *arr app calls this before anything else on Test — AcerviNode not
// implementing it at all (a 404) made every "Test" fail immediately,
// regardless of everything else being correctly wired. Found live.
//
// save_path is the only field with a real AcerviNode equivalent
// (download_dir — see settingsSource.DownloadDir); the rest describe
// seeding/ratio/queueing behavior AcerviNode has no concept of (TorBox
// handles seeding, not AcerviNode), so they're reported as fixed "disabled"
// values — false/off, not enforced, matching how AcerviNode never actually
// applies a ratio or seeding-time limit to anything.
type preferencesResponse struct {
	SavePath                      string  `json:"save_path"`
	MaxRatioEnabled               bool    `json:"max_ratio_enabled"`
	MaxRatio                      float64 `json:"max_ratio"`
	MaxSeedingTimeEnabled         bool    `json:"max_seeding_time_enabled"`
	MaxSeedingTime                int64   `json:"max_seeding_time"`
	MaxInactiveSeedingTimeEnabled bool    `json:"max_inactive_seeding_time_enabled"`
	MaxInactiveSeedingTime        int64   `json:"max_inactive_seeding_time"`
	MaxRatioAct                   int     `json:"max_ratio_act"`
	QueueingEnabled               bool    `json:"queueing_enabled"`
	DHT                           bool    `json:"dht"`
}

func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, preferencesResponse{
		SavePath: s.settings.DownloadDir(),
		DHT:      true, // no-op for AcerviNode either way, but avoids Sonarr treating DHT-only magnets as unsupported
	})
}
