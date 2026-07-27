// Package importer implements Completed Download Handling: once a debrid
// provider reports a download finished, it fetches the resolved file(s) over
// plain HTTP and writes them to local disk — the same thing a real download
// client does, just sourced from a debrid CDN link instead of BitTorrent or
// NNTP. See ROADMAP.md Phase 2 and docs/configuration.md.
package importer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// fileResolver is the subset of debrid.TorrentProvider and
// debrid.UsenetProvider the importer needs. Both interfaces already share
// this exact method shape (see internal/debrid), so either provider type
// satisfies it without any adapter code.
type fileResolver interface {
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
}

// Importer periodically scans for provider_completed downloads and fetches
// their files to local disk.
type Importer struct {
	db              *database.DB
	torrentProvider fileResolver // nil if no torrent-capable provider is configured
	usenetProvider  fileResolver // nil if no usenet-capable provider is configured
	downloadDir     string
	httpClient      *http.Client
}

// New builds an Importer. Either provider may be nil if that capability
// isn't configured (see cmd/acervinode's buildProviders) — downloads of that
// kind are simply skipped with a logged error rather than crashing. Callers
// pass their concrete debrid.TorrentProvider/debrid.UsenetProvider values
// directly; both satisfy fileResolver structurally since it's a subset of
// each. downloadDir is the fallback destination when a download has no
// save_path.
func New(db *database.DB, torrentProvider fileResolver, usenetProvider fileResolver, downloadDir string) *Importer {
	return &Importer{
		db:              db,
		torrentProvider: torrentProvider,
		usenetProvider:  usenetProvider,
		downloadDir:     downloadDir,
		httpClient:      &http.Client{Timeout: 10 * time.Minute}, // files can be large
	}
}

// Run blocks, calling Tick every interval until ctx is done.
func (im *Importer) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := im.Tick(ctx); err != nil {
				slog.Error("importer: tick failed", "error", err)
			}
		}
	}
}

// Tick processes every download currently in provider_completed state, once
// each. Failures are logged and left for the next tick to retry — there's no
// separate retry-count/backoff bookkeeping in this version.
func (im *Importer) Tick(ctx context.Context) error {
	rows, err := im.db.ListDownloadsByState(ctx, database.StateProviderCompleted)
	if err != nil {
		return fmt.Errorf("list provider_completed downloads: %w", err)
	}
	for _, d := range rows {
		if err := im.processDownload(ctx, d); err != nil {
			slog.Error("importer: process download failed", "id", d.ID, "name", d.Name, "error", err)
		}
	}
	return nil
}

func (im *Importer) processDownload(ctx context.Context, d *database.Download) error {
	var provider fileResolver
	switch d.Kind {
	case database.KindTorrent:
		provider = im.torrentProvider
	case database.KindUsenet:
		provider = im.usenetProvider
	default:
		return fmt.Errorf("unknown kind %q", d.Kind)
	}
	if provider == nil {
		return fmt.Errorf("no provider configured for kind %q", d.Kind)
	}

	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	files, err := provider.Files(ctx, id)
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}

	destDir := d.SavePath
	if destDir == "" {
		destDir = filepath.Join(im.downloadDir, d.Category, d.Name)
	}

	for _, f := range files {
		if err := im.fetchFile(ctx, provider, id, f, destDir); err != nil {
			return fmt.Errorf("fetch file %q: %w", f.Path, err)
		}
	}

	now := time.Now().UTC()
	if err := im.db.UpdateDownloadStatus(ctx, d.ID, database.StateReadyForImport, 1.0, d.SizeBytes, &now, ""); err != nil {
		return fmt.Errorf("mark ready_for_import: %w", err)
	}
	slog.Info("importer: download ready", "id", d.ID, "name", d.Name, "dest", destDir, "files", len(files))
	return nil
}

// fetchFile resolves a real download link and streams it to destDir/f.Path,
// skipping files already present at the expected size (the idempotency
// story for this version — a resumed download re-fetches a partial file
// from scratch rather than range-resuming it).
func (im *Importer) fetchFile(ctx context.Context, provider fileResolver, id debrid.ProviderDownloadID, f debrid.DownloadFile, destDir string) error {
	destPath, err := safeJoin(destDir, f.Path)
	if err != nil {
		return err
	}

	if info, err := os.Stat(destPath); err == nil && !info.IsDir() && info.Size() == f.SizeBytes {
		return nil // already fetched
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	link, err := provider.RequestDownloadLink(ctx, id, f.ProviderFileID)
	if err != nil {
		return fmt.Errorf("resolve download link: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := im.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: unexpected status %d", resp.StatusCode)
	}

	// Write to a .part sibling and rename into place, so a process crash or
	// cancelled context mid-transfer never leaves a truncated file at the
	// real destination path (which fetchFile's own size check above would
	// otherwise mistake for a smaller-but-complete file on a later retry —
	// it can't, since the file only appears at destPath once whole).
	tmpPath := destPath + ".part"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write file: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("finalize file: %w", err)
	}
	return nil
}

// safeJoin joins destDir with a provider-supplied relative path, rejecting
// anything that would escape destDir (e.g. a crafted "../../etc/passwd"
// style entry from a torrent/NZB's own file list, which is otherwise
// untrusted third-party content).
func safeJoin(destDir, relPath string) (string, error) {
	joined := filepath.Join(destDir, filepath.FromSlash(relPath))

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", fmt.Errorf("resolve destination directory: %w", err)
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve destination path: %w", err)
	}
	if absJoined != absDestDir && !strings.HasPrefix(absJoined, absDestDir+string(filepath.Separator)) {
		return "", fmt.Errorf("file path %q escapes destination directory", relPath)
	}
	return absJoined, nil
}
