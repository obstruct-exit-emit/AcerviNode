// Package importer implements Completed Download Handling: once a debrid
// provider reports a download finished, it fetches the resolved file(s) over
// plain HTTP and writes them to local disk — the same thing a real download
// client does, just sourced from a debrid CDN link instead of BitTorrent or
// NNTP. See ROADMAP.md Phase 2 and docs/configuration.md.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// provider is the subset of debrid.TorrentProvider and debrid.UsenetProvider
// the importer needs: List to proactively refresh status (see
// refreshStatuses) and discover manually-added downloads (see
// discoverManual), Files/RequestDownloadLink to actually fetch a completed
// download's bytes, Name to record/key discovered downloads and the
// discovery baseline. Both interfaces already share this exact method shape
// (see internal/debrid), so either provider type satisfies it without any
// adapter code.
type provider interface {
	Name() string
	List(ctx context.Context) ([]debrid.DownloadStatus, error)
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
}

// maxBackoff caps how long a failing download waits between retries,
// regardless of how the exponential backoff computation grows — a hardcoded
// ceiling rather than another config knob, since "eventually give up" is the
// one lever (MaxRetries) that's actually worth exposing.
const maxBackoff = time.Hour

// Importer periodically refreshes every tracked download's status from its
// provider and fetches provider_completed downloads' files to local disk.
type Importer struct {
	db              *database.DB
	torrentProvider provider // nil if no torrent-capable provider is configured
	usenetProvider  provider // nil if no usenet-capable provider is configured
	httpClient      *http.Client

	// mu guards downloadDir/interval/maxRetries/categoryPaths/maxConcurrent/
	// fetchTimeout/webDownloadProvider, which SetConfig/SetCategoryPaths/
	// SetMaxConcurrent/SetFetchTimeout/SetWebDownloadProvider can change live
	// (see cmd/acervinode's liveSettings) — everything else on Importer is
	// set once at construction and never mutated afterward.
	mu                  sync.Mutex
	downloadDir         string
	interval            time.Duration // also the backoff base: attempt N waits ~interval*2^N
	maxRetries          int
	categoryPaths       map[string]string // category name -> override dir, replacing downloadDir/<category> for that category
	maxConcurrent       int               // how many downloads Tick fetches to disk at once
	fetchTimeout        time.Duration     // per-file fetch deadline — see fetchFile
	webDownloadProvider provider          // nil if no web-download-capable provider is configured — see SetWebDownloadProvider

	// intervalChanged carries a fresh interval into Run's select loop so a
	// live SetConfig call can reset the ticker without Run having to poll
	// for changes. Buffered 1: a SetConfig that lands while Run hasn't
	// consumed the previous change just overwrites it — only the latest
	// interval matters.
	intervalChanged chan time.Duration
}

// New builds an Importer. Either provider may be nil if that capability
// isn't configured (see cmd/acervinode's buildProviders) — downloads of that
// kind are simply skipped with a logged error rather than crashing. Callers
// pass their concrete debrid.TorrentProvider/debrid.UsenetProvider values
// directly; both satisfy provider structurally since it's a subset of each.
// downloadDir is the fallback destination when a download has no save_path.
// interval is the tick period (also the backoff base and the proactive
// status-refresh cadence — see refreshStatuses); maxRetries is how many
// failed attempts a download gets before it's moved to StateError instead of
// retried again. All three can be changed later, live, via SetConfig.
// maxConcurrent starts at 1 (Tick processes its due downloads strictly one
// at a time) and fetchTimeout at 10 minutes — both changeable live via
// SetMaxConcurrent/SetFetchTimeout, which cmd/acervinode's liveSettings
// applies right after construction to match the configured values (mirroring
// how it already does for category paths — see SetCategoryPaths).
func New(db *database.DB, torrentProvider provider, usenetProvider provider, downloadDir string, interval time.Duration, maxRetries int) *Importer {
	return &Importer{
		db:              db,
		torrentProvider: torrentProvider,
		usenetProvider:  usenetProvider,
		downloadDir:     downloadDir,
		interval:        interval,
		maxRetries:      maxRetries,
		categoryPaths:   map[string]string{},
		maxConcurrent:   1,
		fetchTimeout:    10 * time.Minute,
		httpClient:      &http.Client{}, // no client-wide Timeout — fetchFile derives a per-request one from fetchTimeout instead, since it can change live
		intervalChanged: make(chan time.Duration, 1),
	}
}

// SetConfig updates downloadDir/interval/maxRetries live, with no restart —
// the next Tick (and every one after) uses the new downloadDir/maxRetries
// immediately, and Run's ticker is reset to the new interval right away
// rather than waiting out whatever's left of the old one.
func (im *Importer) SetConfig(downloadDir string, interval time.Duration, maxRetries int) {
	im.mu.Lock()
	im.downloadDir = downloadDir
	im.maxRetries = maxRetries
	changed := interval != im.interval
	im.interval = interval
	im.mu.Unlock()

	if !changed {
		return
	}
	select {
	case im.intervalChanged <- interval:
	default:
		// A previous change is still waiting for Run to consume it — drain
		// it and replace with this newer one rather than blocking.
		select {
		case <-im.intervalChanged:
		default:
		}
		im.intervalChanged <- interval
	}
}

func (im *Importer) getConfig() (downloadDir string, interval time.Duration, maxRetries int) {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.downloadDir, im.interval, im.maxRetries
}

// SetCategoryPaths replaces the live category->override-dir map wholesale —
// the next processDownload for any category takes the new mapping
// immediately, no restart needed. A nil map is treated the same as empty.
func (im *Importer) SetCategoryPaths(paths map[string]string) {
	if paths == nil {
		paths = map[string]string{}
	}
	im.mu.Lock()
	im.categoryPaths = paths
	im.mu.Unlock()
}

// categoryPath reports category's override directory, if one is set.
func (im *Importer) categoryPath(category string) (string, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	dir, ok := im.categoryPaths[category]
	return dir, ok && dir != ""
}

// CategoryPaths reports the current live category->override-dir map — the
// exported counterpart of categoryPath, for callers outside this package
// that need to confirm a SetCategoryPaths call actually took (see
// cmd/acervinode's settings tests).
func (im *Importer) CategoryPaths() map[string]string {
	im.mu.Lock()
	defer im.mu.Unlock()
	out := make(map[string]string, len(im.categoryPaths))
	for k, v := range im.categoryPaths {
		out[k] = v
	}
	return out
}

// SetWebDownloadProvider sets (or, with nil, clears) the web-download-
// capable provider — a post-construction setter rather than a New() param
// like torrentProvider/usenetProvider, deliberately: this capability was
// added later, and every existing caller of New() (every test in this
// package included) would otherwise need a third provider argument they
// don't care about. cmd/acervinode calls this once during startup wiring,
// the same timing as SetCategoryPaths/SetImporter. A nil provider here just
// means refreshKind skips it, same as torrentProvider/usenetProvider being
// nil already does.
func (im *Importer) SetWebDownloadProvider(p provider) {
	im.mu.Lock()
	im.webDownloadProvider = p
	im.mu.Unlock()
}

func (im *Importer) getWebDownloadProvider() provider {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.webDownloadProvider
}

// SetMaxConcurrent updates how many provider_completed downloads Tick fetches
// to disk at once, live — the next Tick uses the new value immediately. n < 1
// is clamped to 1 rather than rejected, since 0 or negative would deadlock
// Tick's semaphore.
func (im *Importer) SetMaxConcurrent(n int) {
	if n < 1 {
		n = 1
	}
	im.mu.Lock()
	im.maxConcurrent = n
	im.mu.Unlock()
}

func (im *Importer) getMaxConcurrent() int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.maxConcurrent
}

// MaxConcurrent is the exported counterpart of getMaxConcurrent, for callers
// outside this package confirming a SetMaxConcurrent call took (see
// cmd/acervinode's settings tests).
func (im *Importer) MaxConcurrent() int {
	return im.getMaxConcurrent()
}

// SetFetchTimeout updates the per-file fetch deadline live — the next fetch
// (in-flight ones keep whatever deadline they already started with) uses the
// new value immediately.
func (im *Importer) SetFetchTimeout(d time.Duration) {
	im.mu.Lock()
	im.fetchTimeout = d
	im.mu.Unlock()
}

func (im *Importer) getFetchTimeout() time.Duration {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.fetchTimeout
}

// FetchTimeout is the exported counterpart of getFetchTimeout, for callers
// outside this package confirming a SetFetchTimeout call took.
func (im *Importer) FetchTimeout() time.Duration {
	return im.getFetchTimeout()
}

// Config reports the current live downloadDir/interval/maxRetries — the
// exported counterpart of getConfig, for callers outside this package that
// need to confirm a SetConfig call actually took (see
// cmd/acervinode's settings tests).
func (im *Importer) Config() (downloadDir string, interval time.Duration, maxRetries int) {
	return im.getConfig()
}

// Run blocks, calling Tick every interval until ctx is done.
func (im *Importer) Run(ctx context.Context) {
	_, interval, _ := im.getConfig()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case newInterval := <-im.intervalChanged:
			ticker.Reset(newInterval)
		case <-ticker.C:
			if err := im.Tick(ctx); err != nil {
				slog.Error("importer: tick failed", "error", err)
			}
		}
	}
}

// Tick first refreshes every tracked download's status from its provider
// (see refreshStatuses), then processes every provider_completed download
// whose next_retry_at has passed (or was never set), once each — including
// any row refreshStatuses itself just moved into provider_completed this
// same tick, so a download that finishes between polls is fetched within one
// tick instead of waiting for the next one. Up to getMaxConcurrent downloads
// are fetched in parallel (a semaphore-bounded goroutine per download); Tick
// itself still blocks until every one of this batch has finished, whether it
// succeeded or not. A failure is handled by handleFailure — backed off and
// retried, or given up on — rather than left to retry on every single tick
// forever. Each download's own db writes are independent (keyed by its own
// ID), and database.DB's connection pool is capped to one connection, so
// concurrent goroutines here can't corrupt anything — they just serialize on
// that one connection for the brief moment any of them touches it.
func (im *Importer) Tick(ctx context.Context) error {
	im.refreshStatuses(ctx)

	rows, err := im.db.ListDownloadsDueForRetry(ctx, database.StateProviderCompleted, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("list downloads due for retry: %w", err)
	}

	sem := make(chan struct{}, im.getMaxConcurrent())
	var wg sync.WaitGroup
	for _, d := range rows {
		sem <- struct{}{}
		wg.Add(1)
		go func(d *database.Download) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := im.processDownload(ctx, d); err != nil {
				im.handleFailure(ctx, d, err)
			}
		}(d)
	}
	wg.Wait()
	return nil
}

// refreshStatuses proactively syncs every queued/downloading row's local
// state against its provider — the same sync each compat shim already does
// reactively when an *arr app polls /info or mode=queue (see
// internal/qbittorrent and internal/sabnzbd's own refreshFromProvider), but
// run on im.interval regardless of whether anything is actively polling
// right now. Without this, a download only ever progressed when something
// external happened to poll — including never, if nothing but the native API
// or web UI (which don't touch the provider at all) is watching it — so it
// could sit looking stuck long after the provider actually finished. See
// docs/providers.md#completed-download-handling.
func (im *Importer) refreshStatuses(ctx context.Context) {
	im.refreshKind(ctx, database.KindTorrent, im.torrentProvider)
	im.refreshKind(ctx, database.KindUsenet, im.usenetProvider)
	// Every webdl row is always AddedViaManual (no *arr-facing shim exists
	// for it — see database.KindWebDL), but discovery/status-refresh still
	// applies the same way it does for a discovered Manual torrent/usenet
	// download: this is what makes a hoster link added directly through
	// TorBox's own site show up here too, not just links added through
	// AcerviNode's own "+ Add" form.
	im.refreshKind(ctx, database.KindWebDL, im.getWebDownloadProvider())
}

func (im *Importer) refreshKind(ctx context.Context, kind database.Kind, p provider) {
	if p == nil {
		return
	}
	rows, err := im.db.ListDownloads(ctx, kind)
	if err != nil {
		slog.Error("importer: list downloads failed", "kind", kind, "error", err)
		return
	}
	// Unlike the old version of this check, rows being empty doesn't skip
	// the List() call below — discoverManual still needs it to catch a
	// first-ever manually-added download for a kind nothing's tracked yet.
	statuses, err := p.List(ctx)
	if err != nil {
		// Not yet configured is routine (e.g. no TorBox key set yet) and
		// would otherwise log an error every single tick — everything else
		// is worth surfacing.
		if !errors.Is(err, debrid.ErrNoProvider) {
			slog.Error("importer: provider list failed", "kind", kind, "error", err)
		}
		return
	}
	im.db.RefreshFromProvider(ctx, rows, statuses)
	im.discoverManual(ctx, kind, p.Name(), rows, statuses)
}

// discoverManual finds provider items AcerviNode has no local row for at all
// (as opposed to RefreshFromProvider, which only updates rows it already
// knows about) and adopts them as AddedViaManual downloads — this is what
// makes an item added directly through the provider's own website/app show
// up in the web UI's Manual tab, not just items added through AcerviNode's
// own "+ Add" form. The very first time this runs for a given provider+kind,
// nothing is adopted: every currently-unmatched item is instead recorded as
// a permanent baseline to ignore (see database.SeedDiscoveryBaseline), so
// shipping this feature doesn't flood the Manual tab with an account's
// entire pre-existing history — only items that show up afterward ever are.
func (im *Importer) discoverManual(ctx context.Context, kind database.Kind, providerName string, tracked []*database.Download, statuses []debrid.DownloadStatus) {
	trackedIDs := make(map[string]bool, len(tracked))
	for _, d := range tracked {
		trackedIDs[d.ProviderDownloadID] = true
	}

	var untracked []debrid.DownloadStatus
	for _, st := range statuses {
		if !trackedIDs[string(st.ID)] {
			untracked = append(untracked, st)
		}
	}

	// The seeded check happens before the "nothing unmatched" early return
	// deliberately: seeding must record that this provider+kind has been
	// observed at all, even if there happened to be zero unmatched items at
	// that exact moment — an empty baseline is a perfectly valid "nothing
	// pre-existing to ignore" result. Checking after the early return would
	// mean a kind with nothing untracked the first few ticks never gets
	// seeded at all, and the actual first-ever new item to show up later
	// would be wrongly absorbed into "pre-existing" instead of adopted.
	seeded, err := im.db.IsDiscoveryBaselineSeeded(ctx, providerName, kind)
	if err != nil {
		slog.Error("importer: check discovery baseline seeded failed", "kind", kind, "error", err)
		return
	}
	if !seeded {
		ids := make([]string, len(untracked))
		for i, st := range untracked {
			ids[i] = string(st.ID)
		}
		if err := im.db.SeedDiscoveryBaseline(ctx, providerName, kind, ids); err != nil {
			slog.Error("importer: seed discovery baseline failed", "kind", kind, "error", err)
			return
		}
		slog.Info("importer: seeded discovery baseline, nothing adopted this run",
			"kind", kind, "provider", providerName, "count", len(ids))
		return
	}

	if len(untracked) == 0 {
		return
	}

	baseline, err := im.db.DiscoveryBaseline(ctx, providerName, kind)
	if err != nil {
		slog.Error("importer: get discovery baseline failed", "kind", kind, "error", err)
		return
	}

	for _, st := range untracked {
		if baseline[string(st.ID)] {
			continue
		}
		d := &database.Download{
			ID:                 uuid.NewString(),
			Provider:           providerName,
			ProviderDownloadID: string(st.ID),
			Kind:               kind,
			Hash:               strings.ToLower(st.Hash),
			Name:               st.Name,
			SizeBytes:          st.SizeBytes,
			State:              database.LocalStateFromProvider(st.State),
			Progress:           st.Progress,
			AddedVia:           database.AddedViaManual,
		}
		if d.Name == "" {
			d.Name = d.Hash
		}
		if err := im.db.InsertDownload(ctx, d); err != nil {
			slog.Error("importer: adopt discovered download failed", "provider_id", st.ID, "kind", kind, "error", err)
			continue
		}
		slog.Info("importer: discovered and adopted download from provider",
			"kind", kind, "provider_id", st.ID, "name", d.Name)
	}
}

// handleFailure records a failed attempt: either schedules the next retry
// with exponential backoff, or — once maxRetries is reached — gives up and
// moves the download to StateError so it stops being retried and shows up as
// failed rather than stuck forever in provider_completed.
func (im *Importer) handleFailure(ctx context.Context, d *database.Download, procErr error) {
	_, _, maxRetries := im.getConfig()
	attempt := d.RetryCount + 1

	if attempt >= maxRetries {
		// Persist the final attempt count before flipping state — this is
		// what lets database.RefreshFromProvider tell a give-up like this
		// one (RetryCount > 0) apart from a StateError the provider itself
		// reported (RetryCount == 0, since that path never goes through this
		// retry bookkeeping at all — see RefreshFromProvider) and treat this
		// one as a sticky, local decision that only a manual retry/re-add
		// should revive, not the provider simply still reporting its old
		// state on its next poll. next_retry_at doesn't matter here —
		// nothing consults it once state has left provider_completed.
		if err := im.db.UpdateDownloadRetry(ctx, d.ID, attempt, time.Now().UTC(), procErr.Error()); err != nil {
			slog.Error("importer: persist final retry count failed", "id", d.ID, "error", err)
			return
		}
		if err := im.db.UpdateDownloadStatus(ctx, d.ID, database.StateError, d.Progress, d.SizeBytes, nil, procErr.Error()); err != nil {
			slog.Error("importer: mark error failed", "id", d.ID, "error", err)
			return
		}
		slog.Error("importer: giving up after max retries", "id", d.ID, "name", d.Name, "attempts", attempt, "error", procErr)
		return
	}

	nextRetryAt := time.Now().UTC().Add(im.backoff(attempt))
	if err := im.db.UpdateDownloadRetry(ctx, d.ID, attempt, nextRetryAt, procErr.Error()); err != nil {
		slog.Error("importer: update retry failed", "id", d.ID, "error", err)
		return
	}
	slog.Warn("importer: process download failed, will retry",
		"id", d.ID, "name", d.Name, "attempt", attempt, "max_retries", maxRetries,
		"next_retry_at", nextRetryAt, "error", procErr)
}

// backoff returns interval*2^attempt, capped at maxBackoff — the shift
// itself is also capped so a very large maxRetries configuration can't
// overflow the calculation before the maxBackoff clamp would apply anyway.
func (im *Importer) backoff(attempt int) time.Duration {
	const maxShift = 10 // 2^10 = 1024x — already far past any sane maxBackoff
	shift := attempt
	if shift > maxShift {
		shift = maxShift
	}
	_, interval, _ := im.getConfig()
	d := interval * time.Duration(int64(1)<<uint(shift))
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

func (im *Importer) processDownload(ctx context.Context, d *database.Download) error {
	var p provider
	switch d.Kind {
	case database.KindTorrent:
		p = im.torrentProvider
	case database.KindUsenet:
		p = im.usenetProvider
	default:
		return fmt.Errorf("unknown kind %q", d.Kind)
	}
	if p == nil {
		return fmt.Errorf("no provider configured for kind %q", d.Kind)
	}

	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	files, err := p.Files(ctx, id)
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}

	destDir := d.SavePath
	if destDir == "" {
		if override, ok := im.categoryPath(d.Category); ok {
			destDir = filepath.Join(override, d.Name)
		} else {
			downloadDir, _, _ := im.getConfig()
			destDir = filepath.Join(downloadDir, d.Category, d.Name)
		}
	}

	for _, f := range files {
		if err := im.fetchFile(ctx, p, id, f, destDir); err != nil {
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
func (im *Importer) fetchFile(ctx context.Context, p provider, id debrid.ProviderDownloadID, f debrid.DownloadFile, destDir string) error {
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

	link, err := p.RequestDownloadLink(ctx, id, f.ProviderFileID)
	if err != nil {
		return fmt.Errorf("resolve download link: %w", err)
	}

	// A per-request deadline rather than the client's own Timeout field,
	// since fetchTimeout can change live (SetFetchTimeout) — the client
	// itself is built once at construction. Covers the whole transfer, not
	// just connecting, so it needs headroom for large files.
	fetchCtx, cancel := context.WithTimeout(ctx, im.getFetchTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, link, nil)
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
