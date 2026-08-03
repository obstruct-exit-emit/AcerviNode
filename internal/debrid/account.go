package debrid

import "context"

// AccountStatus is a provider's point-in-time view of the configured
// account itself — plan tier, premium expiry, lifetime usage — distinct
// from DownloadStatus, which is about one specific download.
type AccountStatus struct {
	PlanName             string
	IsSubscribed         bool
	PremiumExpiresAt     string
	TotalBytesDownloaded int64
	// CooldownUntil, if set, is when a provider-imposed restriction on this
	// account is believed to lift — see torbox.UserData.CooldownUntil's own
	// doc comment for exactly what was (and wasn't) confirmed about it.
	// Found live investigating a real "everything looks frozen" report:
	// while this was set to a future time, TorBox silently returned empty
	// (200 OK, zero items, no error) from every listing endpoint instead of
	// erroring or rate-limiting in a way AcerviNode's own backoff already
	// detects — indistinguishable from every download having vanished
	// (harmless — see database.RefreshFromProvider's mass-vanish circuit
	// breaker — but leaves everything looking frozen with no visible
	// explanation until this is surfaced). Empty string when not currently
	// restricted, or for a provider that doesn't report this at all.
	CooldownUntil string
}

// AccountProvider is implemented by a provider that can report its own
// account status. Not every provider needs to — a provider that doesn't
// simply doesn't satisfy this interface, and the settings UI's account
// status section has nothing to show, the same "structural, not every
// provider needs every capability" approach as UsenetProvider/
// WebDownloadProvider.
type AccountProvider interface {
	Account(ctx context.Context) (AccountStatus, error)
}
