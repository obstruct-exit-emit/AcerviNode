package database

import (
	"testing"
)

func newPendingDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The case the whole table exists for: an *arr add the provider took even
// though the reply never arrived. Discovery must recognise the item as ours.
func TestClaimPendingArrAdd_MatchesByHash(t *testing.T) {
	ctx := t.Context()
	db := newPendingDB(t)

	if err := db.RecordPendingArrAdd(ctx, &PendingArrAdd{
		Provider: "torbox", Kind: KindTorrent,
		Hash: "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
		Name: "Some.Release", Category: "tv-sonarr", SavePath: "/tv",
	}); err != nil {
		t.Fatalf("RecordPendingArrAdd() error = %v", err)
	}

	// Discovery reports the hash lowercased; recording uppercased it. The two
	// must still meet, which is why both sides normalise.
	got, err := db.ClaimPendingArrAdd(ctx, "torbox", KindTorrent,
		"abcdef0123456789abcdef0123456789abcdef01", "A Totally Different Name")
	if err != nil {
		t.Fatalf("ClaimPendingArrAdd() error = %v", err)
	}
	if got == nil {
		t.Fatal("no match, want the pending add claimed by hash")
	}
	if got.Category != "tv-sonarr" || got.SavePath != "/tv" {
		t.Errorf("claimed %+v, want the original category and save path carried through", got)
	}
}

// Usenet has no client-side hash, so name is all there is.
func TestClaimPendingArrAdd_FallsBackToName(t *testing.T) {
	ctx := t.Context()
	db := newPendingDB(t)

	if err := db.RecordPendingArrAdd(ctx, &PendingArrAdd{
		Provider: "torbox", Kind: KindUsenet, Name: "Some.NZB.Release", Category: "tv-sonarr",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ClaimPendingArrAdd(ctx, "torbox", KindUsenet, "", "Some.NZB.Release")
	if err != nil || got == nil {
		t.Fatalf("ClaimPendingArrAdd() = %v, %v; want a name match", got, err)
	}
}

// Claimed once, and only once — a second untracked item must not inherit a
// Managed identity that has already been spent.
func TestClaimPendingArrAdd_ConsumesTheRow(t *testing.T) {
	ctx := t.Context()
	db := newPendingDB(t)

	if err := db.RecordPendingArrAdd(ctx, &PendingArrAdd{
		Provider: "torbox", Kind: KindTorrent, Hash: "aa", Name: "One",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ClaimPendingArrAdd(ctx, "torbox", KindTorrent, "aa", ""); got == nil {
		t.Fatal("first claim found nothing")
	}
	if got, _ := db.ClaimPendingArrAdd(ctx, "torbox", KindTorrent, "aa", ""); got != nil {
		t.Errorf("second claim returned %+v, want nil — the row was already spent", got)
	}
}

// The guards. Each of these would otherwise mislabel a genuinely Manual
// download as Managed, which is the failure mode this must not introduce while
// fixing the opposite one.
func TestClaimPendingArrAdd_DoesNotOverreach(t *testing.T) {
	ctx := t.Context()
	db := newPendingDB(t)

	if err := db.RecordPendingArrAdd(ctx, &PendingArrAdd{
		Provider: "torbox", Kind: KindTorrent, Hash: "aa", Name: "Mine",
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name           string
		provider       string
		kind           Kind
		hash, itemName string
	}{
		{"another provider's account", "alldebrid", KindTorrent, "aa", "Mine"},
		{"another kind", "torbox", KindUsenet, "aa", "Mine"},
		{"an unrelated item", "torbox", KindTorrent, "bb", "Something Else"},
		{"nothing to match on", "torbox", KindTorrent, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.ClaimPendingArrAdd(ctx, tc.provider, tc.kind, tc.hash, tc.itemName)
			if err != nil {
				t.Fatalf("ClaimPendingArrAdd() error = %v", err)
			}
			if got != nil {
				t.Errorf("claimed %+v, want nil — this item is not the failed add", got)
			}
		})
	}
}

// A genuinely-failed add must not leave a trap that mislabels an unrelated
// download later, so rows expire and expired ones are never claimable.
func TestClaimPendingArrAdd_IgnoresExpired(t *testing.T) {
	ctx := t.Context()
	db := newPendingDB(t)

	p := &PendingArrAdd{Provider: "torbox", Kind: KindTorrent, Hash: "aa", Name: "Old"}
	if err := db.RecordPendingArrAdd(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Age it past its own TTL.
	if _, err := db.ExecContext(ctx,
		`UPDATE pending_arr_adds SET expires_at = datetime('now', '-1 hour') WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}

	got, err := db.ClaimPendingArrAdd(ctx, "torbox", KindTorrent, "aa", "Old")
	if err != nil {
		t.Fatalf("ClaimPendingArrAdd() error = %v", err)
	}
	if got != nil {
		t.Errorf("claimed an expired row (%+v), want nil", got)
	}

	if err := db.PurgeExpiredPendingArrAdds(ctx); err != nil {
		t.Fatalf("PurgeExpiredPendingArrAdds() error = %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_arr_adds`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows after purge, want 0", n)
	}
}
