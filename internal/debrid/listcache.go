package debrid

import (
	"context"
	"sync"
	"time"
)

// reactiveListTTL bounds how stale a reused List() response may be. Small
// enough that an *arr app's view is never meaningfully behind (it polls on
// the order of seconds at best, and internal/importer is separately
// refreshing the same rows on its own interval), but long enough that a
// burst of polls arriving together collapses to a single provider call.
const reactiveListTTL = 2 * time.Second

// ListCache coalesces and briefly reuses a provider's full List() response
// across every caller that would otherwise fetch its own copy: both compat
// shims' reactive refreshes inside their HTTP handlers (qBittorrent's
// /torrents/info, SABnzbd's mode=queue and mode=history) and
// internal/importer's own background poll. Each of those originally made
// its own unconditional List() call — the shims on every single request.
//
// That is a lot of duplicated work in a real setup. Sonarr, Radarr, Readarr
// and Lidarr each poll their download client on their own schedule, and a
// single *arr app hits more than one of those endpoints per cycle — so the
// same full account listing was being fetched from the provider many times
// a minute, with every response then written back through
// database.RefreshFromProvider. Because the database deliberately runs on a
// single connection (see database.Open's SetMaxOpenConns(1)), all of that
// redundant write traffic serializes against every other query, including
// the web UI's own polling — which is what makes the whole app feel slow
// and its state look stuck, for usenet and torrents alike, rather than just
// making the shims themselves slow.
//
// One instance per kind lives on the shared Dynamic*Provider wrapper, which
// the importer and the shims already hold the same pointer to — so a single
// fetch per interval serves all of them, and provider load stops scaling
// with how many *arr apps are connected or how fast they poll. The TTL is
// driven by the importer's own poll interval (see SetTTL): that setting is
// already the user's answer to "how often should we ask the provider", and
// there's no reason for a shim request to answer it differently.
//
// Deliberately NOT applied to the importer's fast per-download poll, which
// uses a targeted Status() lookup rather than List(): that path exists
// precisely to get genuinely fresh data on demand for one active download,
// and is untouched by any of this.
type ListCache struct {
	// TTL is how long a response may be reused. Zero selects
	// reactiveListTTL; a negative value disables reuse entirely, so every
	// call fetches. The compat shims' own tests use the negative form:
	// they drive a fake provider whose state advances per call and poll it
	// twice back to back to observe a transition, which real *arr polling
	// never does anywhere near fast enough to be affected by the cache.
	TTL time.Duration

	mu        sync.Mutex
	fetchedAt time.Time
	statuses  []DownloadStatus
}

// ttl resolves TTL's zero value to the package default. Callers hold c.mu.
func (c *ListCache) ttl() time.Duration {
	if c.TTL == 0 {
		return reactiveListTTL
	}
	return c.TTL
}

// SetTTL changes how long a response may be reused, for a cache whose
// lifetime should track a live-configurable setting rather than staying
// fixed at construction — see the Dynamic*Provider wrappers, which tie it
// to internal/importer's own poll interval. Same conventions as the TTL
// field: zero selects the package default, negative disables reuse.
func (c *ListCache) SetTTL(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.TTL = d
}

// List returns fetch's result, reusing the previous one when it is still
// within reactiveListTTL. The returned time is when the underlying provider
// call was *started* — for a cache hit that is the original call's start
// time, not now, which is what database.RefreshFromProvider's ordering
// guard needs to see: a reused response genuinely is data from that earlier
// moment, and reporting it as current would let it overwrite fresher state
// that landed in between.
//
// Callers are serialized, so concurrent requests arriving during a slow
// provider call wait for it and then share its result instead of each
// starting another one. A failed fetch is never cached: fetchedAt is left
// untouched so the next caller retries immediately rather than being served
// an error for the rest of the TTL.
func (c *ListCache) List(ctx context.Context, fetch func(context.Context) ([]DownloadStatus, error)) ([]DownloadStatus, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl() {
		return c.statuses, c.fetchedAt, nil
	}

	startedAt := time.Now()
	statuses, err := fetch(ctx)
	if err != nil {
		return nil, startedAt, err
	}
	c.fetchedAt, c.statuses = startedAt, statuses
	return statuses, startedAt, nil
}
