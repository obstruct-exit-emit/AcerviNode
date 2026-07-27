package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
			completed_at, error_message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Provider, d.ProviderDownloadID, string(d.Kind), nullable(d.Hash), d.Name,
		nullable(d.Category), nullable(d.SavePath), d.SizeBytes, d.State, d.Progress,
		d.AddedAt, d.UpdatedAt, d.CompletedAt, nullable(d.ErrorMessage),
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

// ListDownloadsByState returns every download (either kind) currently in the
// given state, oldest first — used by internal/importer to find downloads
// the provider has finished but that haven't been fetched to local disk yet.
func (db *DB) ListDownloadsByState(ctx context.Context, state string) ([]*Download, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE state = ? ORDER BY added_at`, state)
	if err != nil {
		return nil, fmt.Errorf("list downloads by state: %w", err)
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

// UpdateDownloadStatus updates a download's local state machine fields.
func (db *DB) UpdateDownloadStatus(ctx context.Context, id, state string, progress float64, completedAt *time.Time, errorMessage string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE downloads
		SET state = ?, progress = ?, completed_at = ?, error_message = ?, updated_at = ?
		WHERE id = ?`,
		state, progress, completedAt, nullable(errorMessage), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update download status %s: %w", id, err)
	}
	return checkRowsAffected(res, id)
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
	size_bytes, state, progress, added_at, updated_at, completed_at, error_message`

func (db *DB) scanOneDownload(ctx context.Context, query string, arg string) (*Download, error) {
	row := db.QueryRowContext(ctx, query, arg)
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
	var hash, category, savePath, errorMessage sql.NullString
	var kind string
	if err := row.Scan(
		&d.ID, &d.Provider, &d.ProviderDownloadID, &kind, &hash, &d.Name,
		&category, &savePath, &d.SizeBytes, &d.State, &d.Progress,
		&d.AddedAt, &d.UpdatedAt, &d.CompletedAt, &errorMessage,
	); err != nil {
		return nil, fmt.Errorf("scan download: %w", err)
	}
	d.Kind = Kind(kind)
	d.Hash = hash.String
	d.Category = category.String
	d.SavePath = savePath.String
	d.ErrorMessage = errorMessage.String
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
