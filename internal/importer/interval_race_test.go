package importer

import (
	"sync"
	"testing"
	"time"
)

// stormOf runs rounds of n concurrent calls of set, each round with its own
// values, and reports whether every call returned within the deadline.
//
// Many rounds rather than one big storm: a single storm hit the interleaving
// that blocked in only about one run in five, which would make this a test
// that mostly passes on the bug. Four hundred rounds make a miss vanishingly
// unlikely, while code that never blocks finishes them in milliseconds.
func stormOf(rounds, n int, set func(round, i int)) bool {
	done := make(chan struct{})
	go func() {
		for r := 0; r < rounds; r++ {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 1; i <= n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					set(r, i)
				}(i)
			}
			close(start)
			wg.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// TestSetConfig_ConcurrentSavesNeverBlock.
//
// SetConfig told Run about a new interval through a one-slot channel: send, and
// if the slot was full, drain it and send again -- and that second send blocked.
// Two saves racing could interleave as drain (first), refill (second), blocking
// send (first), which then waited for Run to read. Run reads it between ticks,
// so a settings request hung for as long as a tick took; before the fetch pool
// that meant a whole fetch batch.
//
// No Run here, so nothing ever reads the channel: the worst case, and the one
// where any blocking send hangs forever rather than for a while.
func TestSetConfig_ConcurrentSavesNeverBlock(t *testing.T) {
	im := New(openTestDB(t), testRegistry(&fakeProvider{}, nil), t.TempDir(), time.Minute, 5)
	dir := t.TempDir()
	if !stormOf(400, 64, func(r, i int) { im.SetConfig(dir, time.Duration(r*1000+i)*time.Second, 5) }) {
		t.Fatal("a SetConfig call blocked while nothing was reading the interval channel")
	}
}

// TestSetFastPollInterval_ConcurrentSavesNeverBlock — the same pattern, the same
// hazard in principle. Its reader runs on its own goroutine that a fetch never
// blocks, which is why only import_interval_seconds could hang in practice, but
// the send must not be able to block either way.
func TestSetFastPollInterval_ConcurrentSavesNeverBlock(t *testing.T) {
	im := New(openTestDB(t), testRegistry(&fakeProvider{}, nil), t.TempDir(), time.Minute, 5)
	if !stormOf(400, 64, func(r, i int) { im.SetFastPollInterval(time.Duration(r*1000+i) * time.Second) }) {
		t.Fatal("a SetFastPollInterval call blocked while nothing was reading the channel")
	}
}

// TestSetConfig_LastSaveWins — dropping a duplicate signal is only safe if the
// reader takes the value from config, not from the signal. After a storm the
// importer's interval is whichever save happened last, and a single further
// save must still be the one in force.
func TestSetConfig_LastSaveWins(t *testing.T) {
	im := New(openTestDB(t), testRegistry(&fakeProvider{}, nil), t.TempDir(), time.Minute, 5)
	dir := t.TempDir()
	stormOf(5, 32, func(r, i int) { im.SetConfig(dir, time.Duration(r*1000+i)*time.Second, 5) })
	im.SetConfig(dir, 77*time.Second, 5)
	if _, got, _ := im.getConfig(); got != 77*time.Second {
		t.Errorf("interval = %v, want the last save's 77s", got)
	}
}
