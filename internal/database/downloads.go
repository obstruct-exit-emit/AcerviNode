package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

// Kind distinguishes what a downloads row actually is. Torrent/usenet back
// the two compat shims (Sonarr/Radarr-facing); webdl (TorBox's Web Downloads
// / hoster-debrid service — Mega, 1Fichier, Mediafire, and ~160 others) has
// no *arr-facing shim at all, since neither qBittorrent nor SABnzbd has a
// "paste a hoster link" download-client concept — every webdl row is always
// AddedViaManual, added through the native API/web UI directly.
type Kind string

const (
	KindTorrent Kind = "torrent"
	KindUsenet  Kind = "usenet"
	KindWebDL   Kind = "webdl"
)

// State values for the downloads.state local state machine. This is
// AcerviNode's own vocabulary — each compat shim translates it to whatever
// its emulated protocol expects (see docs/qbittorrent-api.md and
// docs/sabnzbd-api.md) rather than storing the protocol's vocabulary here.
const (
	StateQueued            = "queued"
	StateDownloading       = "downloading"
	StateProviderCompleted = "provider_completed"
	StateReadyForImport    = "ready_for_import"
	StateError             = "error"
)

// AddedVia distinguishes how a download entered AcerviNode — which door it
// came through is a permanent, immutable fact set once at insert time, never
// changed afterward (see docs/providers.md#managed-vs-manual). It's what the
// web UI's Managed/Manual tabs filter on, and what internal/importer's
// Completed Download Handling uses to decide whether a download should ever
// be auto-fetched to local disk at all.
type AddedVia string

const (
	// AddedViaArr is a download added through the qBittorrent or SABnzbd
	// compat shim — i.e. by an *arr app, which requires the files to
	// actually land on local disk for its own import step to find them.
	// Auto-fetched by internal/importer; shown in the web UI's Managed tab.
	AddedViaArr AddedVia = "arr"
	// AddedViaManual is a download added directly — either through the web
	// UI's own "+ Add" form (the native API's add endpoints), or discovered
	// already sitting in the provider's own account and adopted (see
	// internal/importer's discovery step) — never auto-fetched to local
	// disk; the user grabs files on demand, the same way TorBox's own web
	// UI works. Shown in the web UI's Manual tab.
	//
	// Not necessarily permanent: if an *arr app later adds the same item
	// through a compat shim, the existing Manual row is promoted to
	// AddedViaArr rather than duplicated — see InsertOrClaimForArr. The
	// reverse never happens; nothing demotes a Managed row.
	AddedViaManual AddedVia = "manual"
)

// Download is a single row in the downloads table: one item added through
// either the qBittorrent shim (Kind = KindTorrent) or the SABnzbd shim
// (Kind = KindUsenet).
type Download struct {
	ID                 string
	Provider           string
	ProviderDownloadID string
	Kind               Kind
	Hash               string // torrent infohash; empty for usenet rows
	Name               string
	Category           string
	SavePath           string
	SizeBytes          int64
	State              string
	Progress           float64
	AddedAt            time.Time
	UpdatedAt          time.Time
	CompletedAt        *time.Time
	// CachedAt is set once, the first time this row is observed as
	// StateProviderCompleted — the provider itself is done, whether or not
	// the files have been fetched to local disk yet (that's CompletedAt, a
	// separate, later moment). Stays nil for a download that's never
	// reached that state. See UpdateDownloadStatus.
	CachedAt *time.Time
	// ProviderCachedAt is when the provider says it cached this content —
	// a property of the content, not of this download, and frequently long
	// before it was added. Nil for a provider that reports no such thing.
	// See CachedAt directly above for the one this is often confused with.
	ProviderCachedAt *time.Time
	// DeleteAfterFetch and KeepFiles are the lifecycle choices made when a
	// Managed download was added through AcerviNode's own UI or API. Nil
	// for an *arr-added download, which is how the two are told apart —
	// AddedVia is AddedViaArr for both. See migration 0013.
	DeleteAfterFetch *bool
	KeepFiles        *bool
	ErrorMessage     string
	// RetryCount and NextRetryAt back internal/importer's backoff: a failed
	// fetch attempt increments RetryCount and sets NextRetryAt to when it's
	// eligible to try again, until MaxRetries is reached and the row moves
	// to StateError instead.
	RetryCount  int
	NextRetryAt *time.Time
	// MissingCount backs proactive vanished-Manual-download detection: how
	// many consecutive successful provider listings this row has been
	// absent from — see RefreshFromProvider. Only ever nonzero for an
	// AddedViaManual row; a Managed (AddedViaArr) download that vanishes is
	// already caught by internal/importer's own fetch-retry path instead.
	// Reset to 0 the moment the row reappears in a listing.
	MissingCount int
	// Source is the original magnet URI (torrent) or NZB URL (usenet) this
	// download was added with, if it was added via a link rather than an
	// uploaded file — empty otherwise, since there's nothing to resubmit for
	// a file upload without keeping the raw bytes around. What
	// ReAddDownload resubmits to the provider when the original
	// provider-side download has been lost (expired from the provider's own
	// list, not just a transient fetch failure — see internal/api's
	// handleReAddDownload).
	Source string
	// SourceFile is the raw bytes of an uploaded .nzb, for a usenet download
	// added via file upload rather than a URL (Source stays empty in that
	// case — see above). Unlike Source, this doesn't apply to torrents (a
	// magnet is always reconstructable from just the hash — see
	// torbox.magnetFromHash — so there's nothing to gain from storing the
	// file) or webdl (link-only, no file-upload path exists at all). Stored
	// directly on the row rather than as a separate file on disk,
	// deliberately: deleting the row removes the stored file atomically
	// with it, no separate cleanup step and no possibility of an orphaned
	// file surviving a deleted download. What ReAddDownload falls back to
	// resubmitting via AddNZBFile when Source is empty — see
	// handleReAddDownload.
	//
	// Deliberately NOT included in downloadColumns/scanDownload — it would
	// mean every list/detail fetch pulls the full file bytes along with
	// everything else, for a field that's read exactly once, only during a
	// Re-add. Only ever populated here when explicitly constructing a row
	// to pass to InsertDownload; a row returned by GetDownloadByID etc.
	// always has this nil regardless of whether one is actually stored —
	// use SourceFileName (cheap, always scanned) to check for one, and
	// GetSourceFile to fetch the bytes on demand.
	SourceFile     []byte
	SourceFileName string
	// AddedVia is fixed at insert with one deliberate exception: an *arr
	// add that lands on a row already tracked as Manual promotes it to
	// AddedViaArr — see InsertOrClaimForArr and the AddedVia type.
	AddedVia AddedVia
}

// DownloadFile is a single file within a Download.
type DownloadFile struct {
	ID                   string
	DownloadID           string
	ProviderFileID       string
	Path                 string
	SizeBytes            int64
	DownloadURL          string
	DownloadURLExpiresAt *time.Time
}

// InsertDownload adds a new download row. AddedAt/UpdatedAt are set to now if
// zero; AddedVia defaults to AddedViaArr if unset (the zero value), matching
// the migration's own column default — every insert site is expected to set
// it explicitly, but this keeps behavior sane for any caller that forgets.
func (db *DB) InsertDownload(ctx context.Context, d *Download) error {
	now := time.Now().UTC()
	if d.AddedAt.IsZero() {
		d.AddedAt = now
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = now
	}
	if d.AddedVia == "" {
		d.AddedVia = AddedViaArr
	}
	// CachedAt's own doc comment says it's set "the first time this row is
	// observed as StateProviderCompleted" — but UpdateDownloadStatus, the
	// only other place that sets it, only fires on a state *transition*.
	// A row inserted already in StateProviderCompleted (TorBox's common
	// instant-cache case: already cached the moment it's added, so the very
	// first status this row ever has already is "done") never transitions
	// into that state — it's born there — so without this, cached_at stayed
	// permanently nil for exactly that case. Found live: a real Manual
	// download's detail view showed "Cached —" despite sitting at 100%
	// progress since the moment it was added.
	if d.State == StateProviderCompleted && d.CachedAt == nil {
		d.CachedAt = &now
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO downloads (
			id, provider, provider_download_id, kind, hash, name, category,
			save_path, size_bytes, state, progress, added_at, updated_at,
			completed_at, cached_at, error_message, source, added_via, source_file, source_file_name,
			delete_after_fetch, keep_files
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Provider, d.ProviderDownloadID, string(d.Kind), nullable(d.Hash), d.Name,
		nullable(d.Category), nullable(d.SavePath), d.SizeBytes, d.State, d.Progress,
		d.AddedAt, d.UpdatedAt, d.CompletedAt, d.CachedAt, nullable(d.ErrorMessage), nullable(d.Source), string(d.AddedVia),
		nullableBytes(d.SourceFile), nullable(d.SourceFileName),
		// Left nil by every caller that isn't the native add endpoint, which
		// is what keeps an *arr grab on its existing behaviour.
		d.DeleteAfterFetch, d.KeepFiles,
	)
	if err != nil {
		return fmt.Errorf("insert download %s: %w", d.ID, err)
	}
	return nil
}

// InsertOrClaimForArr records a download an *arr app just asked for, and is
// how both compat shims persist an add (see the two storeNewDownload
// functions, its only callers). It returns the row that actually represents
// the download, which is not always d.
//
// A plain InsertDownload isn't enough, because a row for this exact
// (provider, provider_download_id) may already exist — in which case the
// insert trips the UNIQUE constraint and the *arr add fails outright.
// There are two real ways that happens, and both end the same way: a
// download *arr explicitly asked for is stranded in the Manual tab, never
// auto-fetched to local disk, so *arr's own import step never sees it.
//
//   - The provider deduped by content. TorBox hands back the torrent_id it
//     already has for a hash, so re-adding through *arr something already
//     tracked as Manual (added through "+ Add", or discovered) collides
//     with that existing row.
//   - discoverManual adopted it first. The provider lists a just-added item
//     immediately, so a discovery pass overlapping an *arr add can adopt it
//     as Manual moments before the shim inserts its own Managed row. This
//     is the intermittent one — refreshKind reads its tracked-rows snapshot
//     after List() specifically to narrow that window, but can't fully
//     close it, since the add can land during the List() call itself.
//
// Either way the resolution is the same: an explicit *arr request outranks
// a passive discovery, so an existing Manual row is *claimed* — promoted to
// AddedViaArr and stamped with the category/save_path *arr asked for —
// rather than duplicated or rejected. Category/save_path are only
// overwritten when non-empty, so a re-add that omits them can't blank out
// what the row already had (an empty save_path in particular silently
// breaks *arr's import — see UpdateDownloadSavePath).
//
// This is the only place added_via is ever rewritten after insert, and the
// promotion is deliberately one-way: discovery never demotes a Managed row,
// so Manual->Managed can't oscillate.
func (db *DB) InsertOrClaimForArr(ctx context.Context, d *Download) (*Download, error) {
	existing, err := db.GetDownloadByProviderID(ctx, d.Provider, d.ProviderDownloadID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		if err := db.InsertDownload(ctx, d); err != nil {
			return nil, err
		}
		return d, nil
	}

	// hash is only ever filled in, never overwritten: the qBittorrent shim
	// is keyed on infohash, so a claimed row that somehow has none is
	// invisible to Sonarr/Radarr — but an existing hash is already the
	// provider's own answer for this same provider id, so there's nothing
	// to gain from replacing it.
	//
	// missing_count is Manual-only bookkeeping (see Download.MissingCount):
	// a claimed row is no longer a candidate for vanished-download
	// detection, so it shouldn't carry a stale count forward.
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET added_via = ?,
		    category = COALESCE(?, category),
		    save_path = COALESCE(?, save_path),
		    hash = COALESCE(NULLIF(hash, ''), ?),
		    missing_count = 0,
		    updated_at = ?
		WHERE id = ?`,
		string(AddedViaArr), nullable(d.Category), nullable(d.SavePath),
		nullable(d.Hash), time.Now().UTC(), existing.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("claim download %s for arr: %w", existing.ID, err)
	}
	if err := checkRowsAffected(res, existing.ID); err != nil {
		return nil, err
	}

	if existing.AddedVia != AddedViaArr {
		slog.Info("database: claimed an existing Manual download for an *arr add",
			"id", existing.ID, "provider_id", existing.ProviderDownloadID,
			"kind", existing.Kind, "name", existing.Name)
	}

	existing.AddedVia = AddedViaArr
	existing.MissingCount = 0
	if d.Category != "" {
		existing.Category = d.Category
	}
	if d.SavePath != "" {
		existing.SavePath = d.SavePath
	}
	if existing.Hash == "" {
		existing.Hash = d.Hash
	}
	return existing, nil
}

// GetDownloadByID looks up a download by its AcerviNode-assigned id.
func (db *DB) GetDownloadByID(ctx context.Context, id string) (*Download, error) {
	return db.scanOneDownload(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE id = ?`, id)
}

// GetDownloadByHash looks up a torrent-kind download by infohash, the
// identifier the qBittorrent shim's API is keyed on.
func (db *DB) GetDownloadByHash(ctx context.Context, hash string) (*Download, error) {
	return db.scanOneDownload(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE hash = ? AND kind = 'torrent'`, hash)
}

// GetDownloadByProviderID looks up a download already tracked under a given
// provider's own download ID — used before inserting a fresh add, since a
// provider may dedupe by content and return an ID that's already tracked
// (e.g. TorBox returning the same torrent_id for a magnet whose hash it
// already has cached under an existing entry), which would otherwise trip
// the (provider, provider_download_id) UNIQUE constraint.
func (db *DB) GetDownloadByProviderID(ctx context.Context, provider, providerDownloadID string) (*Download, error) {
	return db.scanOneDownload(ctx,
		`SELECT `+downloadColumns+` FROM downloads WHERE provider = ? AND provider_download_id = ?`,
		provider, providerDownloadID)
}

// GetSourceFile fetches a row's stored .nzb bytes on demand — deliberately
// separate from the normal Download read path (see Download.SourceFile's
// doc comment), since this is only ever needed once, during
// handleReAddDownload's fallback for a usenet download with no Source URL.
// Returns ("", nil, nil) if the row exists but has nothing stored (a
// URL-based add, a torrent, a webdl, or a discovered download).
func (db *DB) GetSourceFile(ctx context.Context, id string) (filename string, data []byte, err error) {
	var name sql.NullString
	err = db.QueryRowContext(ctx, `SELECT source_file_name, source_file FROM downloads WHERE id = ?`, id).Scan(&name, &data)
	if err != nil {
		return "", nil, fmt.Errorf("get source file %s: %w", id, err)
	}
	return name.String, data, nil
}

// ListDownloadsByProvider is ListDownloads scoped to one provider, for the
// per-provider polling loops.
//
// The scoping is not an optimisation, it is a correctness requirement. A
// refresh pass compares tracked rows against one provider's listing, so
// handing it rows belonging to a *different* provider makes every one of
// them look absent — and missing-detection then flags them "no longer found
// in the provider's account" when the provider that actually holds them was
// never asked.
//
// Found by probing the real two-provider setup rather than by inspection:
// with five AllDebrid rows and two TorBox rows tracked, polling AllDebrid
// flagged both TorBox rows within three ticks. The mass-vanish guard hid
// this only while the wrongly-missing fraction happened to exceed its
// threshold; below that, nothing stopped it. Identity here is the
// (provider, provider_download_id) pair, exactly as
// GetDownloadByProviderID already treats it — which also removes any chance
// of two providers' id spaces colliding into a false match.
func (db *DB) ListDownloadsByProvider(ctx context.Context, provider string, kind Kind) ([]*Download, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+downloadColumns+` FROM downloads WHERE provider = ? AND kind = ? ORDER BY added_at DESC`,
		provider, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list downloads by provider: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDownloads returns every download of the given kind, most recently added first.
func (db *DB) ListDownloads(ctx context.Context, kind Kind) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE kind = ? ORDER BY added_at DESC`, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list downloads: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDownloadsByState returns how many downloads currently sit in state,
// grouped by kind — backs internal/importer's health-status reporting (see
// Importer.ErrorCounts) for GET /api/v1/status, so a monitor can tell how
// many downloads are stuck in error without listing every row. A kind with
// no matching rows simply doesn't appear in the returned map at all, rather
// than an explicit 0 — a caller that needs every kind represented should
// default to zero itself.
func (db *DB) CountDownloadsByState(ctx context.Context, state string) (map[Kind]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM downloads WHERE state = ? GROUP BY kind`, state)
	if err != nil {
		return nil, fmt.Errorf("count downloads by state: %w", err)
	}
	defer rows.Close()

	counts := map[Kind]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("count downloads by state: %w", err)
		}
		counts[Kind(kind)] = n
	}
	return counts, rows.Err()
}

// ListAllDownloads returns every download regardless of kind, most recently
// added first — backs the native API's GET /api/v1/downloads (see
// internal/api), which is kind-agnostic unlike either compat shim.
func (db *DB) ListAllDownloads(ctx context.Context) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+downloadColumns+` FROM downloads ORDER BY added_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list all downloads: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDownloadsDueForRetry returns every AddedViaArr download currently in
// the given state whose next_retry_at has passed (or was never set) — used
// by internal/importer's Completed Download Handling to find downloads ready
// to fetch to local disk, so a download in backoff isn't retried before its
// scheduled time. Filtered to AddedViaArr only: a manual/discovered download
// is never auto-fetched at all (see docs/providers.md#managed-vs-manual), so
// it should never be a candidate here regardless of its state.
func (db *DB) ListDownloadsDueForRetry(ctx context.Context, state string, now time.Time) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE state = ? AND added_via = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY added_at`, state, string(AddedViaArr), now)
	if err != nil {
		return nil, fmt.Errorf("list downloads due for retry: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListActiveManagedDownloads returns every AddedViaArr download currently in
// StateQueued or StateDownloading — the set internal/importer's fast,
// per-download status poll targets (see Importer.refreshActiveDownloads), as
// opposed to the slower bulk List() poll refreshStatuses runs for every kind
// regardless of activity. Scoped to AddedViaArr only, matching
// ListDownloadsDueForRetry: a Manual download is never auto-fetched, so
// there's no latency-sensitive reason to poll it any faster than the bulk
// refresh already does.
func (db *DB) ListActiveManagedDownloads(ctx context.Context, kind Kind) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE kind = ? AND added_via = ? AND state IN (?, ?)
		ORDER BY added_at`, string(kind), string(AddedViaArr), StateQueued, StateDownloading)
	if err != nil {
		return nil, fmt.Errorf("list active managed downloads: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDownloadsEligibleForCleanup returns every AddedViaArr download in
// StateReadyForImport whose completed_at is older than olderThan — see
// internal/importer's retention/cleanup policy. Scoped to arr+
// ready_for_import specifically: that's a Managed download an *arr app has
// already imported elsewhere, so AcerviNode's own local copy (and the
// provider-side one) are redundant storage at that point. A Manual download
// in provider_completed is never a candidate — that's the ongoing
// "available, not yet grabbed" state for something the user hasn't
// downloaded yet, not something safe to auto-delete.
func (db *DB) ListDownloadsEligibleForCleanup(ctx context.Context, olderThan time.Time) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE state = ? AND added_via = ? AND completed_at IS NOT NULL AND completed_at < ?
		  -- keep_files is set only by the native add endpoint, for a Managed
		  -- download added by hand. Cleanup's whole premise is that an *arr
		  -- already imported the files elsewhere, which is true for an *arr
		  -- grab and false for one added here: nothing imports it, so
		  -- removing the local copy deletes the thing the operator asked
		  -- for. NULL (every *arr grab) is not 1, so they clean as before.
		  AND (keep_files IS NULL OR keep_files != 1)
		ORDER BY completed_at`, StateReadyForImport, string(AddedViaArr), olderThan)
	if err != nil {
		return nil, fmt.Errorf("list downloads eligible for cleanup: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListStuckDownloads returns every StateQueued or StateDownloading download
// whose updated_at is older than olderThan — see internal/importer's
// stuck-download watchdog. updated_at only ever moves when
// UpdateDownloadStatus/RefreshFromProvider actually change something
// (state/progress/size/error), never on a no-op poll, so an old updated_at
// here means the provider has genuinely stopped reporting anything new, not
// just that the download has been running a while. Applies to both Managed
// and Manual downloads — unlike ListDownloadsEligibleForCleanup's arr-only
// scope, being stuck queued/downloading isn't a state that means anything
// different depending on how it was added.
func (db *DB) ListStuckDownloads(ctx context.Context, olderThan time.Time) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE state IN (?, ?) AND updated_at < ?
		ORDER BY updated_at`, StateQueued, StateDownloading, olderThan)
	if err != nil {
		return nil, fmt.Errorf("list stuck downloads: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListErroredDownloadsEligibleForCleanup returns every StateError download
// whose updated_at is older than olderThan — see internal/importer's
// error-cleanup policy. Applies to both Managed and Manual downloads: an
// error here already means AcerviNode gave up (retry-exhausted) or the
// provider genuinely lost track of it (a vanished Manual download), unlike
// ListDownloadsEligibleForCleanup's arr-only ready_for_import scope, which
// exists specifically to avoid deleting a Manual download before the user
// ever grabbed it — that concern doesn't apply to a row that's already
// errored out.
func (db *DB) ListErroredDownloadsEligibleForCleanup(ctx context.Context, olderThan time.Time) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE state = ? AND updated_at < ?
		ORDER BY updated_at`, StateError, olderThan)
	if err != nil {
		return nil, fmt.Errorf("list errored downloads eligible for cleanup: %w", err)
	}
	defer rows.Close()

	var out []*Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDownloadRetry records a failed attempt: increments bookkeeping for
// internal/importer's backoff without changing state — the row stays
// provider_completed and is picked up again once next_retry_at passes.
// errorMessage is stored so the API/UI can show the last failure while a
// download is still being retried, not just after it finally gives up.
func (db *DB) UpdateDownloadRetry(ctx context.Context, id string, retryCount int, nextRetryAt time.Time, errorMessage string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET retry_count = ?, next_retry_at = ?, error_message = ?, updated_at = ?
		WHERE id = ?`,
		retryCount, nextRetryAt, nullable(errorMessage), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update download retry %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// RetryDownload resets a download that gave up after exhausting
// import_max_retries (StateError) back to StateProviderCompleted, with
// retry_count cleared and next_retry_at unset, so internal/importer's very
// next tick picks it up and attempts the fetch again from scratch — the
// manual counterpart to the automatic retry/backoff in internal/importer.
// Callers are expected to have already checked the row is actually in
// StateError (see internal/api's handleRetryDownload); this doesn't guard
// against retrying a download in some other state.
func (db *DB) RetryDownload(ctx context.Context, id string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET state = ?, retry_count = 0, next_retry_at = NULL, error_message = NULL, updated_at = ?
		WHERE id = ?`,
		StateProviderCompleted, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("retry download %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// ReAddDownload points an existing local row at a brand new provider-side
// download (newProviderDownloadID) and resets it back to a freshly-added
// state — for when RetryDownload alone isn't enough because the *original*
// provider-side download itself is gone (e.g. expired from the provider's
// own list), not just a transient fetch failure. The local id/name/
// category/hash/source are preserved; provider_download_id, state,
// progress, size, retry bookkeeping, and completed_at are all reset as if
// this were a fresh add, including cached_at (see UpdateDownloadStatus) — a
// new provider-side download hasn't been observed as provider_completed
// yet, whatever the old one's cached_at said. Callers are expected to have
// already resubmitted source to the provider and obtained
// newProviderDownloadID (see internal/api's handleReAddDownload) — this
// only updates the local row.
func (db *DB) ReAddDownload(ctx context.Context, id, newProviderDownloadID string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET provider_download_id = ?, state = ?, progress = 0, size_bytes = 0,
		    retry_count = 0, next_retry_at = NULL, error_message = NULL,
		    completed_at = NULL, cached_at = NULL, updated_at = ?
		WHERE id = ?`,
		newProviderDownloadID, StateQueued, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("re-add download %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// UpdateDownloadStatus updates a download's local state machine fields,
// including size_bytes — a magnet-only add starts with no size info (magnet
// URIs don't carry it), so this is what backfills the real value once the
// provider reports one, rather than leaving it stuck at 0 forever.
//
// cached_at is set here too, but only the first time: the CASE guards on
// cached_at already being NULL, so a row that's already recorded one keeps
// it even as later calls (any state, any caller) keep passing state through
// unconditionally — simpler than threading a "first time?" bool through
// every one of this function's call sites.
func (db *DB) UpdateDownloadStatus(ctx context.Context, id, state string, progress float64, sizeBytes int64, completedAt *time.Time, errorMessage string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET state = ?, progress = ?, size_bytes = ?, completed_at = ?, error_message = ?, updated_at = ?,
		    cached_at = CASE WHEN cached_at IS NULL AND ? = ? THEN ? ELSE cached_at END
		WHERE id = ?`,
		state, progress, sizeBytes, completedAt, nullable(errorMessage), time.Now().UTC(),
		state, StateProviderCompleted, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update download status %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// SetProviderCachedAt records the provider's own cache timestamp for this
// content.
//
// Distinct from cached_at, which UpdateDownloadStatus sets once when this row
// is first seen provider-complete. This one is a fact about the *content* and
// often predates the add — TorBox reports dates set by whoever's download
// first populated its cache, confirmed live at a month earlier than the add.
//
// Written whenever it changes rather than set-once: it is simply what the
// provider currently reports, so a later answer supersedes an earlier one. A
// nil value is ignored, so a listing that omits the field leaves the stored
// value alone instead of blanking it.
func (db *DB) SetProviderCachedAt(ctx context.Context, id string, at *time.Time) error {
	if at == nil {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`UPDATE downloads SET provider_cached_at = ? WHERE id = ? AND (provider_cached_at IS NULL OR provider_cached_at != ?)`,
		at.UTC(), id, at.UTC())
	if err != nil {
		return fmt.Errorf("set provider cached_at %s: %w", id, err)
	}
	return nil
}

// UpdateDownloadSavePath persists a save path that wasn't known at insert
// time — see internal/importer's resolveDestDir/processDownload, the only
// caller: when the adding *arr app didn't supply an explicit save_path (SABnzbd's
// real addurl/addfile API has no such parameter at all; qBittorrent's does,
// but callers don't always send one), AcerviNode computes a fallback
// destination itself the moment it actually fetches the files, and this is
// what makes that computed path stick — without it, the row's save_path
// stayed empty forever, and both compat shims report save_path/storage
// straight from this column (see qbittorrent.handleInfo/handleProperties,
// sabnzbd.handleHistory) for the *arr app's own import step to read. An
// empty value there isn't a soft failure: Sonarr/Radarr/LibriNode see the
// download as "Completed" but have no path to scan, so it silently never
// imports — found live via a real LibriNode + AcerviNode setup where every
// other symptom looked fine.
func (db *DB) UpdateDownloadSavePath(ctx context.Context, id, savePath string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET save_path = ?, updated_at = ?
		WHERE id = ?`,
		savePath, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update download save path %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// UpdateDownloadCategory changes a download's category after the fact — see
// internal/qbittorrent's handleSetCategory, the only caller: real
// qBittorrent's own POST /api/v2/torrents/setCategory, which Sonarr/Radarr
// call from MarkItemAsImported when a separate "post-import category"
// setting differs from the add-time one (confirmed against their real
// source — an optional setting, not part of the default add flow, but a
// real gap found during an API-parity audit since AcerviNode had no way to
// change an already-tracked row's category at all before this).
func (db *DB) UpdateDownloadCategory(ctx context.Context, id, category string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET category = ?, updated_at = ?
		WHERE id = ?`,
		nullable(category), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update download category %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// UpdateMissingCount records how many consecutive successful provider
// listings a tracked AddedViaManual download has been absent from — see
// RefreshFromProvider, the only caller (both to increment it on a miss and
// to reset it back to 0 the moment the row reappears in a listing).
func (db *DB) UpdateMissingCount(ctx context.Context, id string, missingCount int) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET missing_count = ?, updated_at = ?
		WHERE id = ?`,
		missingCount, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update missing count %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// missingDetectionThreshold is how many consecutive successful provider
// listings a tracked AddedViaManual download can be absent from before
// RefreshFromProvider flags it StateError — see handleMissingFromProvider.
// Debounced rather than flagged on the very first miss: a row only ever
// starts being tracked once it was already visible to the provider (either
// an immediate Status() call right after adding it, or already present in a
// List() response at discovery time — see internal/importer.discoverManual),
// but TorBox's own listing endpoints have shown brief eventual-consistency
// gaps around exactly that boundary (see docs/providers.md), so a
// single-miss rule would risk wrongly flagging a download that's still
// genuinely there. Not user-configurable — this is a debounce implementation
// detail, not a tuning knob operators need (see ROADMAP.md Phase 7).
const missingDetectionThreshold = 3

// missingDownloadErrorMessage is the ErrorMessage set once a Manual download
// crosses missingDetectionThreshold — deliberately distinct wording from a
// provider-reported failure (see LocalStateFromProvider), since this is
// AcerviNode's own conclusion ("we stopped seeing it"), not something the
// provider itself ever said.
const missingDownloadErrorMessage = "no longer found in the provider's account"

// massVanishMinTracked/massVanishFraction bound isSuspectedMassVanish: a
// provider listing is only distrusted once there's enough tracked history
// for a fraction to be meaningful (massVanishMinTracked) and the fraction of
// currently-missing Manual downloads crosses massVanishFraction. Found
// worth building the same day the underlying missing-detection feature
// shipped: the debounce in missingDetectionThreshold only protects against
// one item briefly disappearing, not against every tracked item looking
// gone at once because the provider's own listing came back
// successful-but-empty or truncated (a partial outage, a backend bug) —
// nothing about a successful HTTP response distinguishes that from a
// genuine mass-delete. massVanishFraction is deliberately conservative
// (more than half) rather than a lower bar, so a real, ordinary case where
// several downloads happen to finish/get cleaned up around the same time
// doesn't false-trigger.
const (
	massVanishMinTracked = 3
	massVanishFraction   = 0.5
)

// massVanishMaxDuration bounds how long the mass-vanish guard will keep
// distrusting a listing before concluding the listing is simply telling the
// truth.
//
// Without a bound the guard cannot ever conclude anything. It exists to
// discount a listing that came back successful-but-empty because something
// was wrong on the provider's side — but "the account really is empty now"
// produces byte-for-byte the same listing, and that is a completely ordinary
// thing for a user to cause (deleting everything from the provider's own
// site, or from a second AcerviNode pointed at the same account). Observed
// live doing exactly that: a genuinely emptied account left three rows
// frozen indefinitely — never progressing, never flagged missing, never
// cleaned up — while this warning fired 6,409 times over ten hours.
//
// Thirty minutes is deliberately far longer than any plausible transient:
// at the default ten-second poll it means ~180 consecutive successful
// listings all agreeing the account is empty. And releasing the guard is not
// itself the destructive step — missingDetectionThreshold still requires
// three further consecutive misses per row after that, so a listing that
// recovers in the meantime costs nothing.
const massVanishMaxDuration = 30 * time.Minute

// massVanishLogInterval throttles the guard's warning. It used to log on
// every pass, which on a jammed instance meant 73% of the entire log was one
// repeated line — drowning the very signal it was trying to raise.
const massVanishLogInterval = 5 * time.Minute

// massVanishEntry is one scope's suspicion history — see massVanishDecision.
type massVanishEntry struct {
	// since is when this scope's listing first looked anomalous. Zero once
	// a healthy listing clears it.
	since time.Time
	// lastLog throttles the warning to massVanishLogInterval.
	lastLog time.Time
	// released records that this scope already crossed massVanishMaxDuration
	// and was handed back to normal missing-detection, so the transition is
	// announced once rather than on every subsequent pass.
	released bool
	// lastSeen is when this scope was last evaluated at all. A scope is
	// normally evaluated every poll interval, so a long gap means it stopped
	// being polled — its provider was removed, or reconfigured. See
	// massVanishDecision for why an inherited clock would be wrong.
	lastSeen time.Time
}

// massVanishDecision converts "this pass looks anomalous" into "distrust it",
// applying massVanishMaxDuration so a permanently-empty account eventually
// reconciles instead of freezing forever.
//
// Returns distrust (skip missing-detection this pass), since (when the
// anomaly started, zero if none), and logNow (whether the caller should emit
// its throttled warning).
func (db *DB) massVanishDecision(scope string, suspect bool, now time.Time) (distrust bool, since time.Time, logNow bool) {
	db.massVanishMu.Lock()
	defer db.massVanishMu.Unlock()

	if !suspect {
		// A healthy listing clears the history outright: the next anomaly
		// is a new one and gets its own full grace period, rather than
		// inheriting a stale clock from something that already recovered.
		delete(db.massVanish, scope)
		return false, time.Time{}, false
	}

	e := db.massVanish[scope]
	if e == nil {
		e = &massVanishEntry{since: now}
		db.massVanish[scope] = e
	} else if now.Sub(e.lastSeen) > massVanishMaxDuration {
		// This scope stopped being polled for longer than the grace period
		// itself — a provider removed and later re-added under the same
		// name, most plausibly. The old clock describes a different
		// provider's history, and inheriting it would hand the re-added one
		// an already-expired grace period: its very first anomalous listing
		// would be believed outright, with no benefit of the doubt the
		// setting exists to give. Start it over.
		*e = massVanishEntry{since: now}
	}
	e.lastSeen = now

	if now.Sub(e.since) > massVanishMaxDuration {
		// Long past any plausible glitch — believe the provider. Announced
		// once, on the transition, because it changes what the next pass
		// will actually do to rows.
		if !e.released {
			e.released = true
			return false, e.since, true
		}
		return false, e.since, false
	}

	if e.lastLog.IsZero() || now.Sub(e.lastLog) >= massVanishLogInterval {
		e.lastLog = now
		return true, e.since, true
	}
	return true, e.since, false
}

// MassVanishSince reports when scope's listing first looked anomalous, and
// whether it currently does at all — surfaced through GET /api/v1/status so
// a jammed instance is visible to an operator rather than only to whoever is
// reading the logs.
func (db *DB) MassVanishSince(scope string) (time.Time, bool) {
	db.massVanishMu.Lock()
	defer db.massVanishMu.Unlock()
	e := db.massVanish[scope]
	if e == nil {
		return time.Time{}, false
	}
	return e.since, true
}

// isSuspectedMassVanish reports whether the fraction of tracked
// AddedViaManual rows absent from byID is high enough that the listing
// itself is more likely anomalous than every one of them having genuinely
// vanished at once — see RefreshFromProvider, the only caller, which skips
// handleMissingFromProvider entirely for the whole pass when this is true.
// Scoped to AddedViaManual only, mirroring handleMissingFromProvider itself
// — a Managed row is never a candidate for missing-detection in the first
// place, so it shouldn't factor into whether this pass looks suspicious.
func isSuspectedMassVanish(rows []*Download, byID map[string]debrid.DownloadStatus, provider string) bool {
	var manualTotal, manualMissing int
	for _, d := range rows {
		// StateError is excluded from both halves of the fraction, exactly
		// as handleMissingFromProvider excludes it — counting rows this
		// guard protects nothing from is what made it jam permanently.
		// A row already flagged error stays absent from every future
		// listing forever (it's gone, or it belongs to a provider account
		// that was swapped out), so counting it as "missing" meant the
		// fraction could never fall back below the threshold: once enough
		// rows had errored, missing-detection was disabled for that kind
		// for good and this warning fired on every single tick. Observed
		// live on a real instance — 335 identical warnings, tracked_rows=4
		// statuses_returned=1, where 3 of those 4 were long-dead rows from
		// a rotated API key.
		if d.AddedVia != AddedViaManual || d.State == StateError {
			continue
		}
		// Same scoping as the refresh loop itself: another provider's rows
		// are absent from this listing by construction, so counting them
		// would inflate the "missing" fraction with rows that were never
		// this listing's to report and could trip the guard on a provider
		// that is answering perfectly.
		if provider != "" && d.Provider != provider {
			continue
		}
		manualTotal++
		if _, ok := byID[d.ProviderDownloadID]; !ok {
			manualMissing++
		}
	}
	if manualTotal < massVanishMinTracked {
		return false
	}
	return float64(manualMissing)/float64(manualTotal) > massVanishFraction
}

// handleMissingFromProvider is RefreshFromProvider's counterpart for a row
// whose provider_download_id was absent from the current tick's listing —
// see the docs' "Proactively detect a vanished Manual download" section for
// the full design rationale.
func (db *DB) handleMissingFromProvider(ctx context.Context, d *Download) {
	// A Managed (AddedViaArr) download that vanishes is already caught by
	// internal/importer's own fetch-retry path within a few ticks — the
	// fetch attempt itself fails and eventually gives up with a clear
	// reason (see handleFailure) — so this mechanism only needs to cover
	// Manual, which has no such path. Also skip a row already in
	// StateError, whatever put it there, so this never clobbers an
	// existing (possibly more specific) error reason.
	if d.AddedVia != AddedViaManual || d.State == StateError {
		return
	}

	count := d.MissingCount + 1
	if count < missingDetectionThreshold {
		if err := db.UpdateMissingCount(ctx, d.ID, count); err != nil {
			slog.Error("database: update missing count failed", "id", d.ID, "error", err)
			return
		}
		d.MissingCount = count
		return
	}

	// Threshold crossed. RetryCount is deliberately left at 0 here (never
	// touched by this path) — that's what keeps this recoverable exactly
	// the same way a provider-reported error already is: if the provider
	// reports the download again on some later tick, the
	// state==StateError-and-RetryCount>0 stickiness check above doesn't
	// apply, so it self-heals automatically with no special-case code (see
	// docs/providers.md#state-mapping).
	if err := db.UpdateDownloadStatus(ctx, d.ID, StateError, d.Progress, d.SizeBytes, nil, missingDownloadErrorMessage); err != nil {
		slog.Error("database: mark vanished download as error failed", "id", d.ID, "error", err)
		return
	}
	d.State = StateError
	d.ErrorMessage = missingDownloadErrorMessage
}

// BackfillHashAndName fills in a row's hash and name from the provider's
// current data — see RefreshFromProvider, the only caller. A torrent's
// provider-side listing is provisional right after it's added (placeholder
// name, no hash yet) until the provider finishes indexing it; a row whose
// initial snapshot was captured during that window otherwise never gets a
// second chance to pick up the real values, since nothing else touches
// Hash/Name after insert. Only ever called when the row's own hash is
// currently empty, so this never overwrites a value that was already
// trustworthy.
func (db *DB) BackfillHashAndName(ctx context.Context, id, hash, name string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET hash = ?, name = ?, updated_at = ?
		WHERE id = ?`,
		nullable(hash), name, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("backfill hash/name %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// BackfillSource fills in a row's Source from the provider's own recorded
// original link — see RefreshFromProvider, the only caller. Only ever called
// when the row's own Source is currently empty, so this never overwrites a
// value that was already there (e.g. one AcerviNode itself submitted at add
// time). What lets a *discovered* download (one AcerviNode never received an
// add request for — see internal/importer.discoverManual) still support
// Re-add, whenever the provider happens to know the original link: a
// reconstructed magnet for a torrent (always derivable from its hash), or
// TorBox's own recorded original_url for usenet/webdl (present for a
// URL-based add, empty for a file-upload-based one — see
// debrid.DownloadStatus.OriginalURL).
func (db *DB) BackfillSource(ctx context.Context, id, source string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET source = ?, updated_at = ?
		WHERE id = ?`,
		nullable(source), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("backfill source %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// LocalStateFromProvider translates a debrid provider's own DownloadState
// into AcerviNode's local state machine (see the State* constants above) —
// shared by both compat shims and internal/importer so all three interpret a
// provider's state identically. This never produces StateReadyForImport:
// that transition only happens once internal/importer has actually fetched a
// download's files to local disk, not merely when the provider says it's
// done.
func LocalStateFromProvider(s debrid.DownloadState) string {
	switch s {
	case debrid.StateQueued:
		return StateQueued
	case debrid.StateDownloading:
		return StateDownloading
	case debrid.StateCompleted:
		return StateProviderCompleted
	case debrid.StateError:
		return StateError
	default:
		return StateQueued
	}
}

// RefreshFromProvider updates every row in rows whose provider-reported
// state, progress, or size has changed, using one bulk statuses slice (a
// single provider List() call) rather than a Status() call per row. Mutates
// both the database and the rows themselves in place, so callers see current
// values immediately without a re-query.
//
// Shared by both compat shims (called reactively on every /info or
// mode=queue poll from an *arr app) and internal/importer (called
// proactively on both its bulk tick and its fast per-download poll) — see
// docs/providers.md#completed-download-handling for why a single proactive
// poller was added: without it, a download only ever progressed when
// something external happened to poll, which could leave it looking stuck
// for a long time with nothing polling at all (e.g. only the web UI open,
// which never triggers a provider refresh on its own).
//
// fetchedAt is when the caller *started* the provider call that produced
// statuses — not now, not when the call returned — and every actual write
// this call makes is gated on it via refreshGuardAllows. See
// refreshFetchedAt's own doc comment for the real, live-observed race this
// closes: with multiple independent pollers now hitting the same download,
// a slower request that started earlier can finish (and try to write) after
// a faster one that started later, and without this guard it would silently
// regress progress/state back to stale data.
// RefreshOptions tunes a single RefreshFromProvider pass.
type RefreshOptions struct {
	// DetectMissing enables vanished-download detection for this pass: a
	// tracked Manual row absent from statuses has its missing_count
	// incremented and is eventually flagged error (see
	// handleMissingFromProvider).
	//
	// Off by default, and deliberately so. Deciding a download is gone is
	// the one destructive inference this refresh makes, and it is only
	// sound when the listing can be trusted to be complete. Both compat
	// shims refresh reactively on every *arr poll — far more often than the
	// importer's own pass, with no rate-limit backoff and no view of
	// whether the provider has been answering reliably — so a truncated or
	// briefly-degraded listing seen there would erode missing_count quickly
	// and flag downloads that were never gone. Only internal/importer's
	// bulk poll opts in, and only while that provider has been consistently
	// healthy, which is what the surrounding code always claimed anyway:
	// "the slower bulk refreshStatuses pass is what actually owns deciding
	// a download is genuinely gone."
	//
	// Found the hard way during a real multi-day provider outage: rows
	// vanished from listings a few at a time, staying under the
	// mass-vanish guard's more-than-half threshold, and real downloads were
	// marked "no longer found in the provider's account" while still
	// sitting on the account.
	DetectMissing bool

	// Provider and Kind identify which batch this pass covers.
	//
	// Provider is load-bearing for correctness, not bookkeeping: a pass
	// compares tracked rows against one provider's listing, so any row
	// belonging to a different provider is absent by construction and must
	// not be judged by it. When set, such rows are skipped outright. Callers
	// are expected to pass only that provider's rows anyway (see
	// ListDownloadsByProvider) — this is the backstop that makes forgetting
	// harmless rather than destructive, since the failure mode is flagging
	// live downloads as gone.
	//
	// Together they also key the mass-vanish guard's grace period, so one
	// provider's empty listing can't reset or extend another's clock.
	Provider string
	Kind     Kind
}

// MassVanishScope keys the mass-vanish guard's grace period for one
// provider/kind pair — see massVanishDecision and MassVanishSince. Exported
// so callers reading the state back key it identically to how a refresh pass
// wrote it, rather than each rebuilding the string.
func MassVanishScope(provider string, kind Kind) string {
	return provider + "/" + string(kind)
}

// scope keys the mass-vanish guard — see massVanishDecision.
func (o RefreshOptions) scope() string { return MassVanishScope(o.Provider, o.Kind) }

func (db *DB) RefreshFromProvider(ctx context.Context, rows []*Download, statuses []debrid.DownloadStatus, fetchedAt time.Time, opts RefreshOptions) {
	byID := make(map[string]debrid.DownloadStatus, len(statuses))
	for _, st := range statuses {
		byID[string(st.ID)] = st
	}

	// suspectMassVanish guards handleMissingFromProvider against a listing
	// that came back successful but anomalously empty/truncated (a partial
	// provider-side outage, a transient backend bug — anything that isn't a
	// hard error and so wouldn't otherwise be distinguished from every
	// tracked item genuinely having vanished at once). Computed once per
	// call rather than per row: a real mass-vanish would affect this whole
	// batch identically, so there's nothing row-specific to decide.
	// Cheap short-circuit: with detection off there is nothing for the
	// guard to protect, and computing it would only produce a misleading
	// warning about a pass that was never going to act on it.
	// Time-bounded rather than indefinite: an account that is genuinely
	// empty produces exactly the listing this guard was built to distrust,
	// so without a bound it can never conclude anything and the rows freeze
	// permanently — see massVanishMaxDuration.
	suspect := opts.DetectMissing && isSuspectedMassVanish(rows, byID, opts.Provider)
	suspectMassVanish, since, logNow := db.massVanishDecision(opts.scope(), suspect, fetchedAt)
	if logNow {
		if suspectMassVanish {
			slog.Warn("database: suspected mass-vanish from provider listing, skipping missing-download detection this pass",
				"scope", opts.scope(), "tracked_rows", len(rows), "statuses_returned", len(statuses),
				"anomalous_since", since, "gives_up_after", massVanishMaxDuration)
		} else {
			slog.Warn("database: provider listing has looked anomalous for longer than the mass-vanish grace period, trusting it from now on",
				"scope", opts.scope(), "tracked_rows", len(rows), "statuses_returned", len(statuses),
				"anomalous_since", since, "grace_period", massVanishMaxDuration)
		}
	}

	for _, d := range rows {
		// A row from another provider is absent from this listing by
		// construction, never because it vanished — judging it here is how
		// a healthy download on provider A gets flagged gone by provider
		// B's poll. Also stops two providers' id spaces colliding into a
		// false match below.
		if opts.Provider != "" && d.Provider != opts.Provider {
			continue
		}
		st, ok := byID[d.ProviderDownloadID]
		if !ok {
			if opts.DetectMissing && !suspectMassVanish {
				db.handleMissingFromProvider(ctx, d)
			}
			continue
		}
		// Gates this row's entire processing below, not just the persisted
		// write further down — including LiveStatus's own cache, which
		// would be exactly as wrong to serve stale as the database would be
		// to write stale. Recording still happens even for a row that turns
		// out to need no persisted write at all (e.g. state/progress
		// genuinely unchanged) — see refreshGuardAllows's own doc comment.
		if !db.refreshGuardAllows(d.ID, fetchedAt, LiveStatus{
			ETASeconds:         st.ETASeconds,
			Seeders:            st.Seeders,
			Leechers:           st.Leechers,
			DownloadSpeedBytes: st.DownloadSpeedBytes,
			Phase:              st.Phase,
			Airlocked:          st.Airlocked,
		}) {
			slog.Debug("database: skipping stale refresh, a fresher update already landed", "id", d.ID)
			continue
		}
		if d.MissingCount > 0 {
			if err := db.UpdateMissingCount(ctx, d.ID, 0); err != nil {
				slog.Error("database: reset missing count failed", "id", d.ID, "error", err)
			} else {
				d.MissingCount = 0
			}
		}

		// A torrent's provider-side listing is provisional right after it's
		// added — placeholder name, no hash yet — until the provider
		// finishes indexing it. A row whose initial snapshot (almost always
		// via internal/importer's discoverManual — a magnet-based add
		// already carries a real hash) was caught during that window
		// otherwise never gets a second chance, since nothing below this
		// touches Hash/Name. Deliberately unconditional on state (runs even
		// for a row the guards below would otherwise skip entirely) and
		// deliberately gated on Hash being empty — a reliable signal this
		// row really was caught mid-indexing, not something to second-guess
		// for a row that already has one.
		if d.Hash == "" && st.Hash != "" {
			hash := strings.ToLower(st.Hash)
			name := st.Name
			if name == "" {
				name = d.Name
			}
			if err := db.BackfillHashAndName(ctx, d.ID, hash, name); err != nil {
				slog.Error("database: backfill hash/name from provider failed", "id", d.ID, "error", err)
			} else {
				d.Hash = hash
				d.Name = name
			}
		}

		// The provider's own cache timestamp for this content, recorded for
		// every matched row rather than only ones being backfilled: it is a
		// property of the content that any listing can report, including for
		// a row that has long since reached ready_for_import.
		if err := db.SetProviderCachedAt(ctx, d.ID, st.ProviderCachedAt); err != nil {
			slog.Warn("database: recording provider cache time failed", "id", d.ID, "error", err)
		}

		// A discovered download (see internal/importer.discoverManual) never
		// had a Source recorded at insert time — there was no add request
		// for AcerviNode to capture a link from. Backfilling it retroactively
		// here, gated the same way as the hash/name backfill above (only
		// when currently empty, so this never overwrites a value that was
		// already there), is what lets Re-add work for a discovered download
		// whenever the provider happens to know its original link — see
		// debrid.DownloadStatus.OriginalURL and BackfillSource.
		if d.Source == "" && st.OriginalURL != "" {
			if err := db.BackfillSource(ctx, d.ID, st.OriginalURL); err != nil {
				slog.Error("database: backfill source from provider failed", "id", d.ID, "error", err)
			} else {
				d.Source = st.OriginalURL
			}
		}

		// Once internal/importer has moved a row to ready_for_import (files
		// actually on disk), the provider's own state is no longer
		// authoritative for it — TorBox still reporting "completed" must not
		// regress the row back to provider_completed.
		if d.State == StateReadyForImport {
			continue
		}
		// A download internal/importer gave up on after exhausting its own
		// fetch retries (RetryCount > 0 — see importer.handleFailure, which
		// always persists the final attempt count before flipping state) is
		// a sticky, local decision: only an explicit manual retry/re-add
		// should revive it, not the provider simply still reporting its old
		// "completed" state on a later poll, which would otherwise silently
		// undo the give-up. A StateError the provider itself is reporting
		// (RetryCount == 0, since that path never goes through fetch-retry
		// bookkeeping at all) stays live below and can recover on its own —
		// e.g. a "stalled (no seeds)" torrent that later finds a seed.
		if d.State == StateError && d.RetryCount > 0 {
			continue
		}

		newState := LocalStateFromProvider(st.State)
		// errorMessage carries the provider's own raw state string (e.g.
		// TorBox's "stalled (no seeds)") whenever the provider itself is
		// reporting a failure — distinct from an error internal/importer's
		// own fetch attempts produced, but surfaced the same way through
		// both compat shims and the native API/UI (see debrid.DownloadStatus
		// .RawState). Included in the no-op change check below so an
		// updated failure reason (e.g. "stalled (no seeds)" -> "Error")
		// isn't silently skipped just because progress/size didn't move.
		errorMessage := ""
		if newState == StateError {
			errorMessage = st.RawState
		}
		if newState == d.State && st.Progress == d.Progress && st.SizeBytes == d.SizeBytes && errorMessage == d.ErrorMessage {
			continue
		}
		// completed_at is set once files are actually on disk
		// (internal/importer), not merely when the provider reports done —
		// so it isn't touched here.
		if err := db.UpdateDownloadStatus(ctx, d.ID, newState, st.Progress, st.SizeBytes, nil, errorMessage); err != nil {
			slog.Error("database: refresh from provider failed", "id", d.ID, "error", err)
			continue
		}
		d.State = newState
		d.Progress = st.Progress
		d.SizeBytes = st.SizeBytes
		d.ErrorMessage = errorMessage
	}
}

// refreshGuardAllows reports whether an update whose underlying provider
// data was fetched at fetchedAt should actually be applied for id — false
// if a fresher update (a later fetchedAt) has already been applied. Also
// caches live — see DB's own doc comment for why — but only when the
// ordering check itself passes; a stale live snapshot is exactly as wrong
// to cache as it would be to write to the database. The check and the
// record are done under the same lock so two concurrent callers can't both
// pass the check before either has recorded — the connection pool's own
// single-connection serialization (see Open's SetMaxOpenConns(1)) only
// protects the SQL writes themselves from corrupting each other, not this
// in-memory ordering decision.
func (db *DB) refreshGuardAllows(id string, fetchedAt time.Time, live LiveStatus) bool {
	db.refreshMu.Lock()
	defer db.refreshMu.Unlock()
	if db.refreshState == nil {
		db.refreshState = map[string]refreshCacheEntry{}
	}
	if existing, ok := db.refreshState[id]; ok && fetchedAt.Before(existing.fetchedAt) {
		return false
	}
	db.refreshState[id] = refreshCacheEntry{fetchedAt: fetchedAt, live: live}
	return true
}

// LiveStatus returns the most recently observed live status for id, if
// any — see refreshGuardAllows, the only writer. ok is false if nothing's
// ever been polled for this download yet (e.g. it was just added and no
// tick has run) — a caller should treat that the same as "no live data
// available right now," not an error.
func (db *DB) LiveStatus(id string) (LiveStatus, bool) {
	db.refreshMu.Lock()
	defer db.refreshMu.Unlock()
	entry, ok := db.refreshState[id]
	return entry.live, ok
}

// DeleteDownload removes a download and its files (files cascade via FK).
func (db *DB) DeleteDownload(ctx context.Context, id string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM downloads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete download %s: %w", id, err)
	}
	if err := checkRowsAffected(res, id); err != nil {
		return err
	}
	// refreshState is keyed by download id and nothing else ever removes
	// from it, so without this every download ever deleted leaves its
	// ordering timestamp and cached LiveStatus behind for the lifetime of
	// the process — an unbounded leak on a long-running instance with
	// ordinary add/remove churn.
	db.forgetRefreshState(id)
	return nil
}

// forgetRefreshState drops a download's refresh-ordering record and cached
// live status — see refreshGuardAllows, which is what populates it.
func (db *DB) forgetRefreshState(id string) {
	db.refreshMu.Lock()
	defer db.refreshMu.Unlock()
	delete(db.refreshState, id)
}

// ReplaceDownloadFiles overwrites the file list for a download — the simplest
// correct way to sync against a provider's Files() response, which is always
// authoritative for the whole set.
func (db *DB) ReplaceDownloadFiles(ctx context.Context, downloadID string, files []*DownloadFile) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace files for %s: %w", downloadID, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM download_files WHERE download_id = ?`, downloadID); err != nil {
		return fmt.Errorf("clear files for %s: %w", downloadID, err)
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO download_files (
				id, download_id, provider_file_id, path, size_bytes,
				download_url, download_url_expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.ID, downloadID, nullable(f.ProviderFileID), f.Path, f.SizeBytes,
			nullable(f.DownloadURL), f.DownloadURLExpiresAt,
		); err != nil {
			return fmt.Errorf("insert file %s for %s: %w", f.ID, downloadID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace files for %s: %w", downloadID, err)
	}
	return nil
}

// ListDownloadFiles returns every file belonging to a download.
func (db *DB) ListDownloadFiles(ctx context.Context, downloadID string) ([]*DownloadFile, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, download_id, provider_file_id, path, size_bytes, download_url, download_url_expires_at
		FROM download_files WHERE download_id = ? ORDER BY path`, downloadID)
	if err != nil {
		return nil, fmt.Errorf("list files for %s: %w", downloadID, err)
	}
	defer rows.Close()

	var out []*DownloadFile
	for rows.Next() {
		f := &DownloadFile{}
		var providerFileID, downloadURL sql.NullString
		if err := rows.Scan(&f.ID, &f.DownloadID, &providerFileID, &f.Path, &f.SizeBytes,
			&downloadURL, &f.DownloadURLExpiresAt); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		f.ProviderFileID = providerFileID.String
		f.DownloadURL = downloadURL.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetDownloadFileURL caches a resolved download link for a file.
func (db *DB) SetDownloadFileURL(ctx context.Context, fileID, url string, expiresAt time.Time) error {
	res, err := db.ExecContext(ctx, `
		UPDATE download_files SET download_url = ?, download_url_expires_at = ? WHERE id = ?`,
		url, expiresAt, fileID)
	if err != nil {
		return fmt.Errorf("set file url %s: %w", fileID, err)
	}
	return checkRowsAffected(res, fileID)
}

const downloadColumns = `
	id, provider, provider_download_id, kind, hash, name, category, save_path,
	size_bytes, state, progress, added_at, updated_at, completed_at, cached_at, provider_cached_at, error_message,
	retry_count, next_retry_at, source, added_via, missing_count, source_file_name,
	delete_after_fetch, keep_files`

func (db *DB) scanOneDownload(ctx context.Context, query string, args ...any) (*Download, error) {
	row := db.QueryRowContext(ctx, query, args...)
	d, err := scanDownload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDownload(row rowScanner) (*Download, error) {
	d := &Download{}
	var hash, category, savePath, errorMessage, source, sourceFileName sql.NullString
	var kind, addedVia string
	if err := row.Scan(
		&d.ID, &d.Provider, &d.ProviderDownloadID, &kind, &hash, &d.Name,
		&category, &savePath, &d.SizeBytes, &d.State, &d.Progress,
		&d.AddedAt, &d.UpdatedAt, &d.CompletedAt, &d.CachedAt, &d.ProviderCachedAt, &errorMessage,
		&d.RetryCount, &d.NextRetryAt, &source, &addedVia, &d.MissingCount, &sourceFileName,
		&d.DeleteAfterFetch, &d.KeepFiles,
	); err != nil {
		return nil, fmt.Errorf("scan download: %w", err)
	}
	d.Kind = Kind(kind)
	d.Hash = hash.String
	d.Category = category.String
	d.SavePath = savePath.String
	d.ErrorMessage = errorMessage.String
	d.Source = source.String
	d.SourceFileName = sourceFileName.String
	d.AddedVia = AddedVia(addedVia)
	return d, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func checkRowsAffected(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("no download with id %s", id)
	}
	return nil
}
