package debrid

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestListCache_ReusesWithinTTL(t *testing.T) {
	var c ListCache
	calls := 0
	fetch := func(context.Context) ([]DownloadStatus, error) {
		calls++
		return []DownloadStatus{{ID: "a"}}, nil
	}

	first, firstAt, err := c.List(context.Background(), fetch)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	second, secondAt, err := c.List(context.Background(), fetch)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if calls != 1 {
		t.Errorf("fetch called %d times, want 1 — the second call should reuse", calls)
	}
	if len(first) != 1 || len(second) != 1 || second[0].ID != first[0].ID {
		t.Errorf("second = %+v, want the same statuses as first (%+v)", second, first)
	}
	// The reused response must keep the original fetch time: reporting it
	// as current would let stale data overwrite fresher state that landed
	// in between, via database.RefreshFromProvider's ordering guard.
	if !secondAt.Equal(firstAt) {
		t.Errorf("reused fetchedAt = %v, want the original %v", secondAt, firstAt)
	}
}

func TestListCache_NegativeTTLDisablesReuse(t *testing.T) {
	c := ListCache{TTL: -1}
	calls := 0
	fetch := func(context.Context) ([]DownloadStatus, error) {
		calls++
		return nil, nil
	}

	for i := 0; i < 3; i++ {
		if _, _, err := c.List(context.Background(), fetch); err != nil {
			t.Fatalf("List() error = %v", err)
		}
	}
	if calls != 3 {
		t.Errorf("fetch called %d times, want 3 — a negative TTL must disable reuse", calls)
	}
}

func TestListCache_RefetchesAfterTTL(t *testing.T) {
	c := ListCache{TTL: time.Millisecond}
	calls := 0
	fetch := func(context.Context) ([]DownloadStatus, error) {
		calls++
		return nil, nil
	}

	if _, _, err := c.List(context.Background(), fetch); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, _, err := c.List(context.Background(), fetch); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times, want 2 — an expired entry must be refetched", calls)
	}
}

// A failed fetch must not be cached: the next caller has to retry rather
// than be served the failure for the rest of the TTL.
func TestListCache_DoesNotCacheErrors(t *testing.T) {
	var c ListCache
	calls := 0
	wantErr := errors.New("provider exploded")
	fetch := func(context.Context) ([]DownloadStatus, error) {
		calls++
		if calls == 1 {
			return nil, wantErr
		}
		return []DownloadStatus{{ID: "recovered"}}, nil
	}

	if _, _, err := c.List(context.Background(), fetch); !errors.Is(err, wantErr) {
		t.Fatalf("first List() error = %v, want %v", err, wantErr)
	}
	got, _, err := c.List(context.Background(), fetch)
	if err != nil {
		t.Fatalf("second List() error = %v, want the retry to succeed", err)
	}
	if len(got) != 1 || got[0].ID != "recovered" {
		t.Errorf("second List() = %+v, want the retried result", got)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times, want 2", calls)
	}
}
