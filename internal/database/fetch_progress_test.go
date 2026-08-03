package database

import "testing"

func TestSetFetchProgress_FetchProgress_ClearFetchProgress(t *testing.T) {
	db := openTestDB(t)

	if _, ok := db.FetchProgress("dl-1"); ok {
		t.Fatal("FetchProgress() before any Set = ok:true, want ok:false")
	}

	db.SetFetchProgress("dl-1", 0.25)
	got, ok := db.FetchProgress("dl-1")
	if !ok || got != 0.25 {
		t.Errorf("FetchProgress() = %v, %v, want 0.25, true", got, ok)
	}

	// A later Set for the same id overwrites, not accumulates.
	db.SetFetchProgress("dl-1", 0.75)
	got, ok = db.FetchProgress("dl-1")
	if !ok || got != 0.75 {
		t.Errorf("FetchProgress() after second Set = %v, %v, want 0.75, true", got, ok)
	}

	// A different id is tracked independently.
	db.SetFetchProgress("dl-2", 0.1)
	if got, ok := db.FetchProgress("dl-1"); !ok || got != 0.75 {
		t.Errorf("FetchProgress(dl-1) after Set(dl-2) = %v, %v, want unaffected 0.75, true", got, ok)
	}

	db.ClearFetchProgress("dl-1")
	if _, ok := db.FetchProgress("dl-1"); ok {
		t.Error("FetchProgress() after Clear = ok:true, want ok:false")
	}
	if _, ok := db.FetchProgress("dl-2"); !ok {
		t.Error("FetchProgress(dl-2) after clearing dl-1 = ok:false, want unaffected ok:true")
	}
}

func TestClearFetchProgress_UnknownID_NoOp(t *testing.T) {
	db := openTestDB(t)
	db.ClearFetchProgress("does-not-exist") // must not panic
}

// TestEffectiveProgress proves fetchProgress only ever substitutes for
// Download.Progress while the row is StateProviderCompleted — every other
// state always reports the persisted value unchanged, even when a caller
// mistakenly (or staleness) still has a fetchProgress value in hand.
func TestEffectiveProgress(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		dbProgress    float64
		fetchProgress float64
		haveFetch     bool
		want          float64
	}{
		{"downloading, no fetch progress tracked", StateDownloading, 0.4, 0, false, 0.4},
		{"provider_completed, fetch progress tracked", StateProviderCompleted, 1.0, 0.3, true, 0.3},
		{"provider_completed, no fetch progress tracked yet", StateProviderCompleted, 1.0, 0, false, 1.0},
		{"ready_for_import, fetch progress somehow still present", StateReadyForImport, 1.0, 0.6, true, 1.0},
		{"error, fetch progress somehow still present", StateError, 0.2, 0.6, true, 0.2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Download{State: tt.state, Progress: tt.dbProgress}
			if got := EffectiveProgress(d, tt.fetchProgress, tt.haveFetch); got != tt.want {
				t.Errorf("EffectiveProgress() = %v, want %v", got, tt.want)
			}
		})
	}
}
