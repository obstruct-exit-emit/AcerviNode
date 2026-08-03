package main

// defaultArrCategories are the category names Sonarr/Radarr/Lidarr/Readarr
// pre-fill their own download-client settings forms with, for a brand new,
// unconfigured client — confirmed directly against each app's real source
// (SabnzbdSettings.cs/QBittorrentSettings.cs constructors), not guessed:
//
//	App     | SABnzbd default | qBittorrent default
//	Radarr  | "movies"        | "radarr"
//	Sonarr  | "tv"            | "tv-sonarr"
//	Lidarr  | "music"         | "lidarr"
//	Readarr | "Readarr"       | "readarr"
//
// Readarr's SABnzbd default really is capitalized "Readarr" while its
// qBittorrent one is lowercase "readarr" — a genuine asymmetry in Readarr's
// own code (its SabnzbdSettings field is even still named MusicCategory, a
// leftover from being forked off Lidarr), not a typo here — category name
// comparisons are case-sensitive (confirmed against Sonarr/Radarr's real
// TestCategory: a plain C# string == check), so preserving the exact casing
// matters.
//
// Pre-seeding both compat shims' category stores with every one of these on
// every startup (see SetShimServers) means a user who just accepts an *arr
// app's own default category hits zero setup friction at all — no visit to
// AcerviNode's own Settings → Categories page needed first, indistinguishable
// from connecting to a real SABnzbd/qBittorrent install that happened to
// already have these categories configured. A custom category name (e.g.
// "radarr" typed into Radarr's *SABnzbd* client, which doesn't default there)
// still needs the one-time manual registration — see
// docs/sabnzbd-api.md#categories for why that step can never be fully
// automatic.
//
// Deliberately not deduplicated against the protocol each name is "native"
// to (e.g. "radarr" is only ever a qBittorrent default, never a SABnzbd
// one) — seeding every name into both shims costs nothing (an unused
// category sitting in a categoryStore is inert bookkeeping) and keeps this
// list simple to read and extend, rather than needing a second per-protocol
// list to stay in sync with it.
var defaultArrCategories = []string{
	"movies", "radarr",
	"tv", "tv-sonarr",
	"music", "lidarr",
	"Readarr", "readarr",
}
