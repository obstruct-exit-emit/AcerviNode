package database

import "sync"

// fetchProgress backs DB.SetFetchProgress/FetchProgress/ClearFetchProgress —
// an in-memory-only cache of how far along internal/importer's own local
// file transfer is for a download currently in StateProviderCompleted (the
// *arr-facing "Fetching" state). Deliberately separate from
// refreshMu/refreshState (LiveStatus): that map is written exclusively by
// RefreshFromProvider, on data that came from the debrid provider itself;
// this one is written exclusively by internal/importer as it streams bytes
// to local disk, a concern the provider has no visibility into at all —
// the provider already considers the download 100% done the moment this
// phase begins. Keyed by database.Download.ID, matching refreshState.
//
// Without this, a download's reported progress would jump to (and stay at)
// 1.0 the instant the provider finished, then sit there — visually frozen
// — for however long the actual local transfer takes, which for a large
// file is real, observed time (found live: a 7.9GB movie sitting at
// "Fetching 100%" for tens of seconds). See database.EffectiveProgress,
// which both compat shims and the native API use to substitute this value
// in for Download.Progress during that phase.
type fetchProgressStore struct {
	mu   sync.Mutex
	byID map[string]float64
}

// SetFetchProgress records id's current local-fetch progress (0.0-1.0) —
// called repeatedly, throttled, as internal/importer streams a download's
// files to local disk. Overwrites any previous value; there's no ordering
// concern to guard here the way RefreshFromProvider's fetchedAt does, since
// exactly one internal/importer fetch can ever be in flight for a given
// download at a time (see Importer.processDownload, the only caller).
func (db *DB) SetFetchProgress(id string, progress float64) {
	db.fetchProgress.mu.Lock()
	defer db.fetchProgress.mu.Unlock()
	if db.fetchProgress.byID == nil {
		db.fetchProgress.byID = map[string]float64{}
	}
	db.fetchProgress.byID[id] = progress
}

// FetchProgress reports id's current local-fetch progress, if
// internal/importer is actively tracking one right now — false once it's
// never been set, or after ClearFetchProgress.
func (db *DB) FetchProgress(id string) (float64, bool) {
	db.fetchProgress.mu.Lock()
	defer db.fetchProgress.mu.Unlock()
	p, ok := db.fetchProgress.byID[id]
	return p, ok
}

// ClearFetchProgress removes id's tracked fetch progress — called by
// Importer.processDownload once a fetch attempt ends, success or failure,
// so a stale value never lingers once the download has moved on (to
// ready_for_import, or back to provider_completed awaiting retry with
// nothing currently in flight).
func (db *DB) ClearFetchProgress(id string) {
	db.fetchProgress.mu.Lock()
	defer db.fetchProgress.mu.Unlock()
	delete(db.fetchProgress.byID, id)
}

// EffectiveProgress reports the progress value that should actually be
// shown for d: d.Progress (the provider's own download progress, which
// jumps to 1.0 the instant the provider itself is done) for every other
// state, or fetchProgress — internal/importer's own live local-transfer
// progress — while d is StateProviderCompleted and one is currently being
// tracked (ok true). See DB.FetchProgress/SetFetchProgress's own doc
// comments for why these are two genuinely different things, both called
// "progress," at two different phases of the same download's life.
func EffectiveProgress(d *Download, fetchProgress float64, ok bool) float64 {
	if d.State == StateProviderCompleted && ok {
		return fetchProgress
	}
	return d.Progress
}
