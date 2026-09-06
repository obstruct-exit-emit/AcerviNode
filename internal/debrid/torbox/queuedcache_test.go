package torbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingQueuedServer answers the queued endpoint and counts how often it is
// actually asked — which is the only thing this cache exists to change.
func countingQueuedServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/queued/getqueued":
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"success":true,"data":[{"id":1,"name":"Queued.Thing","type":"torrent"}]}`)
		default:
			// Every other lookup misses, which is what sends Status() to the
			// queued listing in the first place.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"success":true,"data":null}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// One poll pass over several queued downloads used to make one identical
// request per download. This is the regression that matters: the count must
// track passes, not downloads.
func TestListQueuedCached_OnePassIsOneRequest(t *testing.T) {
	srv, hits := countingQueuedServer(t)
	c := NewClient("key", WithBaseURL(srv.URL))

	for i := 0; i < 5; i++ {
		if _, err := c.listQueuedCached(context.Background(), "torrent"); err != nil {
			t.Fatalf("listQueuedCached() error = %v", err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("queued endpoint hit %d times for one pass over 5 downloads, want 1", got)
	}
}

// Torrent and usenet are separate listings polled on separate passes, so they
// must not share an entry.
func TestListQueuedCached_KindsAreSeparate(t *testing.T) {
	srv, hits := countingQueuedServer(t)
	c := NewClient("key", WithBaseURL(srv.URL))

	c.listQueuedCached(context.Background(), "torrent")
	c.listQueuedCached(context.Background(), "usenet")
	c.listQueuedCached(context.Background(), "torrent")

	if got := hits.Load(); got != 2 {
		t.Errorf("hit %d times, want 2 — one per kind, with the repeat served from cache", got)
	}
}

// The cache is a within-pass optimisation, not a way to stop polling. Once it
// expires the next pass has to see current state.
func TestListQueuedCached_RefetchesAfterTTL(t *testing.T) {
	srv, hits := countingQueuedServer(t)
	c := NewClient("key", WithBaseURL(srv.URL))

	c.listQueuedCached(context.Background(), "torrent")
	// Age the entry past the TTL rather than sleeping for it.
	c.queued.mu.Lock()
	e := c.queued.entries["torrent"]
	e.fetchedAt = time.Now().Add(-queuedListTTL - time.Second)
	c.queued.entries["torrent"] = e
	c.queued.mu.Unlock()

	c.listQueuedCached(context.Background(), "torrent")
	if got := hits.Load(); got != 2 {
		t.Errorf("hit %d times, want 2 — a stale entry must be refetched", got)
	}
}

// A failure must not be remembered: the call is best-effort at every site, and
// caching the error would suppress a retry that could well succeed.
func TestListQueuedCached_DoesNotCacheFailures(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient("key", WithBaseURL(srv.URL))

	for i := 0; i < 3; i++ {
		if _, err := c.listQueuedCached(context.Background(), "torrent"); err == nil {
			t.Fatal("expected the failure to surface")
		}
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("hit %d times, want 3 — a failure must not be cached", got)
	}
}

// Concurrent callers in the same pass must collapse to one request, not race
// into several.
func TestListQueuedCached_ConcurrentCallersShareOneFetch(t *testing.T) {
	srv, hits := countingQueuedServer(t)
	c := NewClient("key", WithBaseURL(srv.URL))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.listQueuedCached(context.Background(), "torrent")
		}()
	}
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("hit %d times from 8 concurrent callers, want 1", got)
	}
}

// The bulk listing deliberately does not use the cache: it stamps its result
// as fetched now, and RefreshFromProvider's ordering guard trusts that stamp.
func TestListQueued_BulkPathStaysUncached(t *testing.T) {
	srv, hits := countingQueuedServer(t)
	c := NewClient("key", WithBaseURL(srv.URL))

	for i := 0; i < 3; i++ {
		if _, err := c.ListQueued(context.Background(), "torrent"); err != nil {
			t.Fatalf("ListQueued() error = %v", err)
		}
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("hit %d times, want 3 — the raw call must always fetch", got)
	}
}
