package torbox

import (
	"context"
	"sync"
	"time"
)

// A short cache over the queued listing, for the per-download status path only.
//
// Status() falls back to the queued listing whenever GetTorrent misses, and it
// misses for *every* download that is still queued — that is what "queued"
// means here. So one fast-poll pass over three queued downloads made three
// identical, uncached requests for the same list, every pass. At the previous
// three-second interval that was 120 calls a minute for three downloads, and
// it peaked exactly when several were added at once.
//
// Deliberately not applied to the bulk List() path, which also merges the
// queued listing in. List reports its result as fetched *now*, and
// database.RefreshFromProvider's ordering guard uses that timestamp to decide
// whether a status may overwrite newer state. Serving List a response a few
// seconds old while stamping it as current is precisely the shape of the
// refresh race this project has already been bitten by, and the bulk path runs
// rarely enough that it would gain almost nothing anyway.
const queuedListTTL = 5 * time.Second

type queuedEntry struct {
	fetchedAt time.Time
	items     []QueuedDownload
}

// queuedCache is keyed by kind ("torrent"/"usenet"), since those are separate
// listings and are polled on separate passes.
type queuedCache struct {
	mu      sync.Mutex
	entries map[string]queuedEntry
}

// listQueuedCached returns the queued listing for kind, reusing a recent
// response rather than refetching it once per download.
//
// The lock is held across the fetch on purpose. Callers within one pass arrive
// back to back, so holding it collapses them into a single request and makes
// the others wait for that one result — which is the entire point. Releasing
// it around the fetch would let every caller in the pass start its own,
// leaving exactly the behaviour this exists to remove.
func (c *Client) listQueuedCached(ctx context.Context, kind string) ([]QueuedDownload, error) {
	c.queued.mu.Lock()
	defer c.queued.mu.Unlock()

	if e, ok := c.queued.entries[kind]; ok && time.Since(e.fetchedAt) < queuedListTTL {
		return e.items, nil
	}

	items, err := c.ListQueued(ctx, kind)
	if err != nil {
		// Not cached: a failure here is best-effort at every call site, and
		// remembering it would suppress a retry that might well succeed.
		return nil, err
	}
	if c.queued.entries == nil {
		c.queued.entries = make(map[string]queuedEntry)
	}
	c.queued.entries[kind] = queuedEntry{fetchedAt: time.Now(), items: items}
	return items, nil
}
