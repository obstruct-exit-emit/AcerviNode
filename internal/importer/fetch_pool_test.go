package importer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
)

// poolCDN serves every file immediately except those whose ID is in hold,
// which block until release is closed. It counts requests in flight.
type poolCDN struct {
	srv     *httptest.Server
	release chan struct{}
	current atomic.Int32
	peak    atomic.Int32
}

func newPoolCDN(t *testing.T, hold map[string]bool) *poolCDN {
	t.Helper()
	c := &poolCDN{release: make(chan struct{})}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := c.current.Add(1)
		defer c.current.Add(-1)
		for {
			p := c.peak.Load()
			if n <= p || c.peak.CompareAndSwap(p, n) {
				break
			}
		}
		if hold[r.URL.Path[1:]] {
			select {
			case <-c.release:
			case <-r.Context().Done():
				return
			}
		}
		w.Write([]byte("bytes"))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func poolRow(t *testing.T, db *database.DB, id string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: id, Provider: "faketorbox", ProviderDownloadID: id, Kind: database.KindTorrent,
		Hash: "h-" + id, Name: id, Category: "tv", SavePath: filepath.Join(t.TempDir(), id),
		State: database.StateProviderCompleted,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("InsertDownload(%s) error = %v", id, err)
	}
	return d
}

func stateOf(t *testing.T, db *database.DB, id string) string {
	t.Helper()
	d, err := db.GetDownloadByID(context.Background(), id)
	if err != nil || d == nil {
		return "missing"
	}
	return d.State
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// TestRun_ABigFetchDoesNotHoldUpOthers is the head-of-line block, measured on
// the first production burn-in: with a 9.9 GB file transferring, a movie and
// an episode that finished at TorBox waited 84s and 79s before their own
// transfers began, with two of three fetch slots free the whole time. Tick
// fetched its batch and then waited for all of it, inside Run's loop, so
// nothing that became ready afterwards could start until the slowest fetch in
// the batch was done. On a slower link the same file holds every other
// finished download back for minutes.
func TestRun_ABigFetchDoesNotHoldUpOthers(t *testing.T) {
	db := openTestDB(t)
	cdn := newPoolCDN(t, map[string]bool{"big": true})
	t.Cleanup(func() { close(cdn.release) })
	provider := &fakeProvider{cdn: cdn.srv, filePerDownload: true}

	poolRow(t, db, "big")
	im := New(db, testRegistry(provider, nil), t.TempDir(), 50*time.Millisecond, 5)
	im.SetMaxConcurrent(3)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go im.Run(ctx)

	waitFor(t, "the big fetch to be in flight", 5*time.Second, func() bool { return cdn.current.Load() == 1 })

	// Finishes at the provider while the big one is still transferring.
	poolRow(t, db, "small")

	waitFor(t, "the small download to be fetched while the big one is still in flight", 3*time.Second,
		func() bool { return stateOf(t, db, "small") == database.StateReadyForImport })
	if s := stateOf(t, db, "big"); s != database.StateProviderCompleted {
		t.Errorf("big = %q; the test only means something while it is still fetching", s)
	}
}

// TestRun_TickKeepsRunningDuringALongFetch — the other half of the same
// block. Everything a tick does stalled with it: the provider refresh,
// discovery, cleanup, the stuck-download watchdog. And last_tick_at, which
// GET /api/v1/status exposes precisely so a monitor can tell the loop is
// alive, froze for 103s during that production transfer.
func TestRun_TickKeepsRunningDuringALongFetch(t *testing.T) {
	db := openTestDB(t)
	cdn := newPoolCDN(t, map[string]bool{"big": true})
	t.Cleanup(func() { close(cdn.release) })
	provider := &fakeProvider{cdn: cdn.srv, filePerDownload: true}

	poolRow(t, db, "big")
	im := New(db, testRegistry(provider, nil), t.TempDir(), 50*time.Millisecond, 5)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go im.Run(ctx)

	waitFor(t, "the big fetch to be in flight", 5*time.Second, func() bool { return cdn.current.Load() == 1 })
	first, _ := im.LastTickAt()
	waitFor(t, "last_tick_at to advance while the fetch is still running", 2*time.Second, func() bool {
		now, _ := im.LastTickAt()
		return now.After(first)
	})
}

// TestRun_ConcurrencyLimitHoldsAcrossTicks — a fetch now outlives the tick
// that started it, so the limit can no longer be a per-tick semaphore. It must
// hold across ticks: more due downloads than slots, every fetch held open, and
// many ticks going by, still never more than max in flight.
func TestRun_ConcurrencyLimitHoldsAcrossTicks(t *testing.T) {
	db := openTestDB(t)
	hold := map[string]bool{}
	for i := 0; i < 5; i++ {
		hold[fmt.Sprintf("d%d", i)] = true
	}
	cdn := newPoolCDN(t, hold)
	provider := &fakeProvider{cdn: cdn.srv, filePerDownload: true}
	for i := 0; i < 5; i++ {
		poolRow(t, db, fmt.Sprintf("d%d", i))
	}

	im := New(db, testRegistry(provider, nil), t.TempDir(), 20*time.Millisecond, 5)
	im.SetMaxConcurrent(2)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go im.Run(ctx)

	waitFor(t, "two fetches in flight", 5*time.Second, func() bool { return cdn.current.Load() == 2 })
	time.Sleep(300 * time.Millisecond) // ~15 ticks with everything held
	if p := cdn.peak.Load(); p > 2 {
		t.Fatalf("peak concurrent fetches = %d across ticks, want <= 2", p)
	}

	close(cdn.release)
	waitFor(t, "all five to finish", 5*time.Second, func() bool {
		for i := 0; i < 5; i++ {
			if stateOf(t, db, fmt.Sprintf("d%d", i)) != database.StateReadyForImport {
				return false
			}
		}
		return true
	})
	if p := cdn.peak.Load(); p > 2 {
		t.Errorf("peak concurrent fetches = %d, want <= 2", p)
	}
}

// TestRun_AFreedSlotIsRefilledWithoutWaitingForATick — the regression the fix
// must not introduce. The old per-tick semaphore started the next due
// download the moment a slot freed. A pool fed only by ticks would make a
// Sonarr burst of twenty single episodes crawl through at max_concurrent per
// tick interval. The interval here is a full minute and the first tick is
// driven by hand, so the second download can only start by being refilled.
func TestRun_AFreedSlotIsRefilledWithoutWaitingForATick(t *testing.T) {
	db := openTestDB(t)
	cdn := newPoolCDN(t, map[string]bool{"first": true})
	provider := &fakeProvider{cdn: cdn.srv, filePerDownload: true}
	poolRow(t, db, "first")
	poolRow(t, db, "second")

	im := New(db, testRegistry(provider, nil), t.TempDir(), time.Minute, 5)
	im.SetMaxConcurrent(1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := im.tick(ctx, false); err != nil {
		t.Fatalf("tick() error = %v", err)
	}
	waitFor(t, "the first fetch to be in flight", 5*time.Second, func() bool { return cdn.current.Load() == 1 })
	if s := stateOf(t, db, "second"); s != database.StateProviderCompleted {
		t.Fatalf("second = %q before any slot freed, want still waiting", s)
	}

	close(cdn.release)
	waitFor(t, "the second download to start and finish with no further tick", 3*time.Second,
		func() bool { return stateOf(t, db, "second") == database.StateReadyForImport })
	im.fetchWG.Wait()
}

// TestRun_ShutdownWaitsForInFlightFetches — fetches now outlive the tick that
// started them, so Run must not return while one is still running: the caller
// closes the database next, and a fetch still writing to it would log errors
// on every clean restart.
func TestRun_ShutdownWaitsForInFlightFetches(t *testing.T) {
	db := openTestDB(t)
	cdn := newPoolCDN(t, map[string]bool{"big": true})
	provider := &fakeProvider{cdn: cdn.srv, filePerDownload: true}
	poolRow(t, db, "big")

	im := New(db, testRegistry(provider, nil), t.TempDir(), 50*time.Millisecond, 5)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { im.Run(ctx); close(returned) }()

	waitFor(t, "the fetch to be in flight", 5*time.Second, func() bool { return cdn.current.Load() == 1 })
	cancel()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	im.activeFetchesMu.Lock()
	n := len(im.activeFetches)
	im.activeFetchesMu.Unlock()
	if n != 0 {
		t.Errorf("%d fetch(es) still registered after Run returned, want 0", n)
	}
}
