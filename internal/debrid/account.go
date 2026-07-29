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
