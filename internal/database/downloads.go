package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

// Kind distinguishes the two compat shims' rows in the shared downloads table.
type Kind string

const (
	KindTorrent Kind = "torrent"
	KindUsenet  Kind = "usenet"
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
	ErrorMessage       string
	// RetryCount and NextRetryAt back internal/importer's backoff: a failed
	// fetch attempt increments RetryCount and sets NextRetryAt to when it's
	// eligible to try again, until MaxRetries is reached and the row moves
	// to StateError instead.
	RetryCount  int
	NextRetryAt *time.Time
	// Source is the original magnet URI (torrent) or NZB URL (usenet) this
	// download was added with, if it was added via a link rather than an
	// uploaded file — empty otherwise, since there's nothing to resubmit for
	// a file upload without keeping the raw bytes around. What
	// ReAddDownload resubmits to the provider when the original
	// provider-side download has been lost (expired from the provider's own
	// list, not just a transient fetch failure — see internal/api's
	// handleReAddDownload).
	Source string
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

// InsertDownload adds a new download row. AddedAt/UpdatedAt are set to now if zero.
func (db *DB) InsertDownload(ctx context.Context, d *Download) error {
	now := time.Now().UTC()
	if d.AddedAt.IsZero() {
		d.AddedAt = now
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = now
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO downloads (
			id, provider, provider_download_id, kind, hash, name, category,
			save_path, size_bytes, state, progress, added_at, updated_at,
			completed_at, error_message, source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Provider, d.ProviderDownloadID, string(d.Kind), nullable(d.Hash), d.Name,
		nullable(d.Category), nullable(d.SavePath), d.SizeBytes, d.State, d.Progress,
		d.AddedAt, d.UpdatedAt, d.CompletedAt, nullable(d.ErrorMessage), nullable(d.Source),
	)
	if err != nil {
		return fmt.Errorf("insert download %s: %w", d.ID, err)
	}
	return nil
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

// ListDownloadsDueForRetry returns every download (either kind) currently in
// the given state whose next_retry_at has passed (or was never set) — used
// by internal/importer so a download in backoff isn't retried before its
// scheduled time.
func (db *DB) ListDownloadsDueForRetry(ctx context.Context, state string, now time.Time) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+downloadColumns+`
		FROM downloads
		WHERE state = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY added_at`, state, now)
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
// this were a fresh add. Callers are expected to have already resubmitted
// source to the provider and obtained newProviderDownloadID (see
// internal/api's handleReAddDownload) — this only updates the local row.
func (db *DB) ReAddDownload(ctx context.Context, id, newProviderDownloadID string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET provider_download_id = ?, state = ?, progress = 0, size_bytes = 0,
		    retry_count = 0, next_retry_at = NULL, error_message = NULL,
		    completed_at = NULL, updated_at = ?
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
func (db *DB) UpdateDownloadStatus(ctx context.Context, id, state string, progress float64, sizeBytes int64, completedAt *time.Time, errorMessage string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET state = ?, progress = ?, size_bytes = ?, completed_at = ?, error_message = ?, updated_at = ?
		WHERE id = ?`,
		state, progress, sizeBytes, completedAt, nullable(errorMessage), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update download status %s: %w", id, err)
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
// proactively on its own background tick) — see
// docs/providers.md#completed-download-handling for why a single proactive
// poller was added: without it, a download only ever progressed when
// something external happened to poll, which could leave it looking stuck
// for a long time with nothing polling at all (e.g. only the web UI open,
// which never triggers a provider refresh on its own).
func (db *DB) RefreshFromProvider(ctx context.Context, rows []*Download, statuses []debrid.DownloadStatus) {
	byID := make(map[string]debrid.DownloadStatus, len(statuses))
	for _, st := range statuses {
		byID[string(st.ID)] = st
	}

	for _, d := range rows {
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

		st, ok := byID[d.ProviderDownloadID]
		if !ok {
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

// DeleteDownload removes a download and its files (files cascade via FK).
func (db *DB) DeleteDownload(ctx context.Context, id string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM downloads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete download %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
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
	size_bytes, state, progress, added_at, updated_at, completed_at, error_message,
	retry_count, next_retry_at, source`

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
	var hash, category, savePath, errorMessage, source sql.NullString
	var kind string
	if err := row.Scan(
		&d.ID, &d.Provider, &d.ProviderDownloadID, &kind, &hash, &d.Name,
		&category, &savePath, &d.SizeBytes, &d.State, &d.Progress,
		&d.AddedAt, &d.UpdatedAt, &d.CompletedAt, &errorMessage,
		&d.RetryCount, &d.NextRetryAt, &source,
	); err != nil {
		return nil, fmt.Errorf("scan download: %w", err)
	}
	d.Kind = Kind(kind)
	d.Hash = hash.String
	d.Category = category.String
	d.SavePath = savePath.String
	d.ErrorMessage = errorMessage.String
	d.Source = source.String
	return d, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
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
