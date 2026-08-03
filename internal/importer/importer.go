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
// discoverManual), Status for the fast per-download poll (see
// refreshActiveDownloads) — a single targeted lookup, cheaper than List, for
// checking one specific download that's actively in flight — Files/
// RequestDownloadLink to actually fetch a completed download's bytes, Delete
// for the retention/cleanup policy's best-effort provider-side removal (see
// cleanupDownload), Name to record/key discovered downloads and the
// discovery baseline. Both interfaces already share this exact method shape
// (see internal/debrid), so either provider type satisfies it without any
// adapter code.
type provider interface {
	Name() string
	List(ctx context.Context) ([]debrid.DownloadStatus, error)
	Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error)
	Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error)
	RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, fileID string) (string, error)
	Delete(ctx context.Context, id debrid.ProviderDownloadID, deleteFiles bool) error
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
	// fetchTimeout/webDownloadProvider/cleanupAfterDays, which SetConfig/
	// SetCategoryPaths/SetMaxConcurrent/SetFetchTimeout/
	// SetWebDownloadProvider/SetCleanupAfterDays can change live (see
	// cmd/acervinode's liveSettings) — everything else on Importer is set
	// once at construction and never mutated afterward.
	mu                  sync.Mutex
	downloadDir         string
	interval            time.Duration // also the backoff base: attempt N waits ~interval*2^N
	maxRetries          int
	categoryPaths       map[string]string // category name -> override dir, replacing downloadDir/<category> for that category
	maxConcurrent       int               // how many downloads Tick fetches to disk at once
	fetchTimeout        time.Duration     // per-file fetch deadline — see fetchFile
	cleanupAfterDays    int               // 0 disables the retention/cleanup policy — see cleanupOldDownloads
	webDownloadProvider provider          // nil if no web-download-capable provider is configured — see SetWebDownloadProvider

	// intervalChanged carries a fresh interval into Run's select loop so a
	// live SetConfig call can reset the ticker without Run having to poll
	// for changes. Buffered 1: a SetConfig that lands while Run hasn't
	// consumed the previous change just overwrites it — only the latest
	// interval matters.
	intervalChanged chan time.Duration

	// rateLimitMu guards rateLimitState — see refreshKind's cooldown check
	// and recordRateLimitHit/clearRateLimitHit. Purely in-memory,
	// deliberately: this is operational backoff state, not something that
	// needs to survive a restart the way a download's own retry_count does
	// (a restart just means starting the cooldown clock over, which is
	// fine — the provider's rate limit itself is what actually governs
	// this, not AcerviNode's memory of it).
	rateLimitMu    sync.Mutex
	rateLimitState map[database.Kind]*kindBackoff
}

// kindBackoff tracks one kind's (torrent/usenet/webdl) rate-limit backoff —
// see refreshKind.
type kindBackoff struct {
	nextAttempt     time.Time
	consecutiveHits int
}

// rateLimitBackoffBase/rateLimitBackoffMax bound the cooldown refreshKind
// applies after a provider rate-limits a List() call — doubling per
// consecutive hit, capped at rateLimitBackoffMax. Deliberately a much
// shorter cap than the per-download fetch-retry backoff's maxBackoff (1
// hour): a rate limit is a short-lived, provider-side condition (typically
// a per-minute window), not a download-specific failure that might
// genuinely need an hour before it's worth trying again, and pausing a
// whole kind's status refresh for that long would leave real progress
// invisible to the UI/compat shims far longer than the rate limit itself
// actually lasts. Motivated directly by a real incident, not a
// hypothetical: a burst of manual testing sustained a real TorBox 429 for
// several minutes straight, with the previous behavior (retry every single
// tick regardless) doing nothing to help it recover.
const (
	rateLimitBackoffBase = 30 * time.Second
	rateLimitBackoffMax  = 5 * time.Minute
)

// fastPollInterval is how often refreshActiveDownloads checks each actively
// in-flight (queued/downloading) Managed download individually, via a single
// targeted per-ID Status() call rather than waiting for the next bulk List()
// tick (im.interval, 10s by default) to notice it. A live, controlled,
// same-account comparison against rogerfar/rdt-client (a reference debrid
// download client) found AcerviNode taking ~2x longer to notice an
// already-cached file was ready via an equivalent auto-fetch path — traced to
// exactly this: nothing polled more often than the bulk interval, so a
// download that finished moments after a tick simply waited for the next one.
// A targeted per-ID lookup is dramatically cheaper against a provider's rate
// limit than shortening the bulk interval itself would be — confirmed live
// the hard way (a 2s bulk interval, still listing every download on the
// account three times over, tripped TorBox's real rate limit immediately) —
// the same principle a reference implementation (decypharr) applies for its
// own active-download polling, see docs/providers.md. Deliberately not a live
// config knob: this only ever touches downloads already known to be actively
// in flight (typically very few for a personal-use instance), unlike im.interval
// which governs every kind's full-account listing.
const fastPollInterval = 3 * time.Second

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
		rateLimitState:  map[database.Kind]*kindBackoff{},
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

// SetCleanupAfterDays updates the retention/cleanup policy live — the next
// Tick uses the new value immediately. 0 (or negative) disables cleanup
// entirely; config.Config.Validate already rejects a negative value before
// it would reach here, but this stays defensive rather than assuming that.
func (im *Importer) SetCleanupAfterDays(days int) {
	im.mu.Lock()
	im.cleanupAfterDays = days
	im.mu.Unlock()
}

func (im *Importer) getCleanupAfterDays() int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.cleanupAfterDays
}

// CleanupAfterDays is the exported counterpart of getCleanupAfterDays, for
// callers outside this package confirming a SetCleanupAfterDays call took
// (see cmd/acervinode's settings tests).
func (im *Importer) CleanupAfterDays() int {
	return im.getCleanupAfterDays()
}

// Config reports the current live downloadDir/interval/maxRetries — the
// exported counterpart of getConfig, for callers outside this package that
// need to confirm a SetConfig call actually took (see
// cmd/acervinode's settings tests).
func (im *Importer) Config() (downloadDir string, interval time.Duration, maxRetries int) {
	return im.getConfig()
}

// Run blocks, calling Tick every interval until ctx is done. The fast
// per-download poll (see refreshActiveDownloads) runs on its own goroutine
// with its own ticker, deliberately independent of this loop: Tick can block
// for a while — a slow file fetch, network hiccup — and a different
// download's fast poll shouldn't have to wait for that to finish.
func (im *Importer) Run(ctx context.Context) {
	_, interval, _ := im.getConfig()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	go im.runFastPoll(ctx)

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

// runFastPoll drives refreshActiveDownloads on fastPollInterval until ctx is
// done — see Run's doc comment for why this is a separate goroutine rather
// than another case in Run's own select loop.
func (im *Importer) runFastPoll(ctx context.Context) {
	ticker := time.NewTicker(fastPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			im.refreshActiveDownloads(ctx)
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
// that one connection for the brief moment any of them touches it. Finally,
// cleanupOldDownloads runs the retention policy (a no-op unless
// cleanup_after_days is configured) — last, so a download that just reached
// ready_for_import this same tick isn't somehow considered for cleanup
// before its completed_at has even had a chance to age past the cutoff.
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

	im.cleanupOldDownloads(ctx)
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
	// Computed once per tick, before any kind's own discovery runs, so all
	// three kinds agree on whether this is a genuinely fresh install — see
	// discoverManual. Checking it fresh inside each kind's own pass instead
	// would make the answer depend on iteration order (torrent adopting
	// first would make the database non-empty by the time usenet's own
	// check ran, even though both are equally "fresh").
	hasAny, err := im.db.HasAnyDownloads(ctx)
	// Safe default on error: treat as an established instance (freshInstall
	// false), so a transient DB error can never cause an account's entire
	// history to flood in.
	freshInstall := false
	if err != nil {
		slog.Error("importer: check for any existing downloads failed", "error", err)
	} else {
		freshInstall = !hasAny
	}

	im.refreshKind(ctx, database.KindTorrent, im.torrentProvider, freshInstall)
	im.refreshKind(ctx, database.KindUsenet, im.usenetProvider, freshInstall)
	// Every webdl row is always AddedViaManual (no *arr-facing shim exists
	// for it — see database.KindWebDL), but discovery/status-refresh still
	// applies the same way it does for a discovered Manual torrent/usenet
	// download: this is what makes a hoster link added directly through
	// TorBox's own site show up here too, not just links added through
	// AcerviNode's own "+ Add" form.
	im.refreshKind(ctx, database.KindWebDL, im.getWebDownloadProvider(), freshInstall)
}

func (im *Importer) refreshKind(ctx context.Context, kind database.Kind, p provider, freshInstall bool) {
	if p == nil {
		return
	}
	if until, cooling := im.rateLimitCooldown(kind); cooling {
		slog.Warn("importer: skipping provider list, still in rate-limit cooldown", "kind", kind, "until", until)
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
	fetchedAt := time.Now()
	statuses, err := p.List(ctx)
	if err != nil {
		if errors.Is(err, debrid.ErrRateLimited) {
			until := im.recordRateLimitHit(kind)
			slog.Error("importer: provider rate limited, backing off", "kind", kind, "until", until, "error", err)
			return
		}
		// Not yet configured is routine (e.g. no TorBox key set yet) and
		// would otherwise log an error every single tick — everything else
		// is worth surfacing.
		if !errors.Is(err, debrid.ErrNoProvider) {
			slog.Error("importer: provider list failed", "kind", kind, "error", err)
		}
		return
	}
	im.clearRateLimitHit(kind)
	im.db.RefreshFromProvider(ctx, rows, statuses, fetchedAt)
	im.discoverManual(ctx, kind, p.Name(), rows, statuses, freshInstall)
}

// refreshActiveDownloads is the fast path's entry point: for every Managed
// download currently queued/downloading (per kind), it checks that one
// download's status directly via a targeted per-ID lookup instead of relying
// solely on the next bulk refreshStatuses tick — see fastPollInterval.
func (im *Importer) refreshActiveDownloads(ctx context.Context) {
	im.refreshActiveKind(ctx, database.KindTorrent, im.torrentProvider)
	im.refreshActiveKind(ctx, database.KindUsenet, im.usenetProvider)
	im.refreshActiveKind(ctx, database.KindWebDL, im.getWebDownloadProvider())
}

// refreshActiveKind is refreshActiveDownloads' per-kind worker. It shares
// refreshKind's rate-limit cooldown state deliberately: a 429 from either the
// bulk List() path or this targeted Status() path means the same provider
// budget is under pressure, so both back off together rather than each
// tracking (and potentially fighting over) their own independent cooldown.
// Each row is refreshed through database.RefreshFromProvider — the exact same
// state-transition/backfill logic the bulk path already uses, just fed one
// row and one status instead of a whole account's worth, so there's no second
// implementation of that logic to keep in sync.
func (im *Importer) refreshActiveKind(ctx context.Context, kind database.Kind, p provider) {
	if p == nil {
		return
	}
	if _, cooling := im.rateLimitCooldown(kind); cooling {
		return
	}
	rows, err := im.db.ListActiveManagedDownloads(ctx, kind)
	if err != nil {
		slog.Error("importer: list active managed downloads failed", "kind", kind, "error", err)
		return
	}
	for _, d := range rows {
		fetchedAt := time.Now()
		st, err := p.Status(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID))
		if err != nil {
			if errors.Is(err, debrid.ErrRateLimited) {
				until := im.recordRateLimitHit(kind)
				slog.Error("importer: provider rate limited, backing off", "kind", kind, "until", until, "error", err)
				return
			}
			// A single lookup miss/transient error here isn't worth logging
			// on every fast-poll tick — the slower bulk refreshStatuses pass
			// (with its mass-vanish guard) is what actually owns deciding a
			// download is genuinely gone; this path only needs to notice real
			// progress, not chase every edge case the bulk path already
			// covers.
			continue
		}
		im.clearRateLimitHit(kind)
		im.db.RefreshFromProvider(ctx, []*database.Download{d}, []debrid.DownloadStatus{st}, fetchedAt)
	}
}

// rateLimitCooldown reports whether kind is still within a backoff window
// set by a previous recordRateLimitHit — see refreshKind, the only caller.
func (im *Importer) rateLimitCooldown(kind database.Kind) (time.Time, bool) {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()
	state, ok := im.rateLimitState[kind]
	if !ok || !time.Now().Before(state.nextAttempt) {
		return time.Time{}, false
	}
	return state.nextAttempt, true
}

// recordRateLimitHit advances kind's consecutive-hit count and schedules its
// next allowed attempt with exponential backoff (rateLimitBackoffBase *
// 2^hits, capped at rateLimitBackoffMax) — see refreshKind, the only caller.
func (im *Importer) recordRateLimitHit(kind database.Kind) time.Time {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()
	state, ok := im.rateLimitState[kind]
	if !ok {
		state = &kindBackoff{}
		im.rateLimitState[kind] = state
	}
	state.consecutiveHits++
	state.nextAttempt = time.Now().Add(rateLimitBackoffDuration(state.consecutiveHits))
	return state.nextAttempt
}

// clearRateLimitHit drops kind's backoff state entirely once a List() call
// succeeds — the next rate limit (if any) starts counting from scratch
// rather than continuing to grow from wherever it last left off.
func (im *Importer) clearRateLimitHit(kind database.Kind) {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()
	delete(im.rateLimitState, kind)
}

// RateLimitCooldownUntil is the exported counterpart of rateLimitCooldown,
// for tests confirming a rate-limit hit actually set a cooldown.
func (im *Importer) RateLimitCooldownUntil(kind database.Kind) (time.Time, bool) {
	return im.rateLimitCooldown(kind)
}

// rateLimitBackoffDuration returns rateLimitBackoffBase*2^consecutiveHits,
// capped at rateLimitBackoffMax — the shift itself is also capped so a long
// sustained rate limit can't overflow the calculation before the
// rateLimitBackoffMax clamp would apply anyway (mirrors Importer.backoff's
// identical shift-overflow guard for the per-download fetch backoff).
// consecutiveHits is 1-indexed (the first-ever hit is 1) so that first hit
// gets exactly rateLimitBackoffBase, not double it.
func rateLimitBackoffDuration(consecutiveHits int) time.Duration {
	const maxShift = 10
	shift := consecutiveHits - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxShift {
		shift = maxShift
	}
	d := rateLimitBackoffBase * time.Duration(int64(1)<<uint(shift))
	if d <= 0 || d > rateLimitBackoffMax {
		return rateLimitBackoffMax
	}
	return d
}

// discoverManual finds provider items AcerviNode has no local row for at all
// (as opposed to RefreshFromProvider, which only updates rows it already
// knows about) and adopts them as AddedViaManual downloads — this is what
// makes an item added directly through the provider's own website/app show
// up in the web UI's Manual tab, not just items added through AcerviNode's
// own "+ Add" form.
//
// The very first time this runs for a given provider+kind, what happens to
// whatever's currently unmatched depends on freshInstall (see
// refreshStatuses): on an established instance, nothing is adopted —
// every currently-unmatched item is instead recorded as a permanent
// baseline to ignore (see database.SeedDiscoveryBaseline), so this feature
// landing on an instance that's been running a while (or a second provider
// being added to one) doesn't suddenly flood the Manual tab with a big
// pre-existing history. On a genuinely fresh install, though, there is no
// "pre-existing history" to protect against — the account's current
// contents are exactly what a first-time user expects to see show up, so
// they're adopted immediately instead, and the baseline is seeded empty.
// Found live: a fresh Proxmox install recognized the configured TorBox
// account but never showed its existing downloads, because the original
// version of this always took the conservative branch.
func (im *Importer) discoverManual(ctx context.Context, kind database.Kind, providerName string, tracked []*database.Download, statuses []debrid.DownloadStatus, freshInstall bool) {
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
		if freshInstall {
			if err := im.db.SeedDiscoveryBaseline(ctx, providerName, kind, nil); err != nil {
				slog.Error("importer: seed discovery baseline failed", "kind", kind, "error", err)
				return
			}
			slog.Info("importer: fresh install, adopting whatever's already in the account",
				"kind", kind, "provider", providerName, "count", len(untracked))
			// Falls through to the adoption loop below instead of returning —
			// this tick's untracked items are exactly what should be adopted.
		} else {
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
	}

	if len(untracked) == 0 {
		return
	}

	baseline, err := im.db.DiscoveryBaseline(ctx, providerName, kind)
	if err != nil {
		slog.Error("importer: get discovery baseline failed", "kind", kind, "error", err)
		return
	}

	// A download deleted moments ago can still briefly appear "untracked"
	// here — the provider's own delete isn't always instantly reflected in
	// its listing endpoints, and this tick runs independently of any
	// specific delete request. Without this check, that timing gap would
	// silently re-adopt it as a ghost Manual download for something a user
	// just intentionally removed — confirmed live. See
	// database.RecordDeletedDownload/RecentlyDeletedDownloads.
	recentlyDeleted, err := im.db.RecentlyDeletedDownloads(ctx, providerName, kind)
	if err != nil {
		slog.Error("importer: get recently deleted downloads failed", "kind", kind, "error", err)
		return
	}

	for _, st := range untracked {
		if baseline[string(st.ID)] || recentlyDeleted[string(st.ID)] {
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
			// A discovered download has no add-request source to capture
			// the normal way — this is the closest equivalent, whenever the
			// provider happens to know the original link (a reconstructed
			// magnet for a torrent, or TorBox's own recorded original_url
			// for usenet/webdl — see debrid.DownloadStatus.OriginalURL).
			// Empty is still possible (e.g. a file-upload-based add TorBox
			// never got a URL for) — RefreshFromProvider's own backfill
			// covers a row already tracked before the provider happened to
			// know a link; this just avoids waiting a whole extra tick for
			// the common case of a brand-new discovery.
			Source: st.OriginalURL,
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

// providerForKind returns the configured provider for d's kind, or nil if
// none is — shared by processDownload and cleanupDownload, both of which
// need to reach the same provider a given download belongs to.
func (im *Importer) providerForKind(kind database.Kind) provider {
	switch kind {
	case database.KindTorrent:
		return im.torrentProvider
	case database.KindUsenet:
		return im.usenetProvider
	case database.KindWebDL:
		return im.getWebDownloadProvider()
	default:
		return nil
	}
}

// resolveDestDir computes where d's files land (or landed) on local disk —
// shared by processDownload (to fetch them there) and cleanupDownload (to
// know what to remove later). An *arr app's own explicit save_path always
// wins; otherwise a category override, then downloadDir/category, always
// namespaced by the download's own name so sibling downloads in the same
// category never collide.
func (im *Importer) resolveDestDir(d *database.Download) string {
	if d.SavePath != "" {
		return d.SavePath
	}
	if override, ok := im.categoryPath(d.Category); ok {
		return filepath.Join(override, d.Name)
	}
	downloadDir, _, _ := im.getConfig()
	return filepath.Join(downloadDir, d.Category, d.Name)
}

func (im *Importer) processDownload(ctx context.Context, d *database.Download) error {
	// Live fetch progress — see database.DB.SetFetchProgress's own doc
	// comment for why this is a separate concern from d.Progress (already
	// 1.0 the moment this function starts running: the provider itself
	// considers the download done the instant it entered
	// StateProviderCompleted, well before anything about this local
	// transfer has happened). Cleared unconditionally on every exit path
	// below — success, a fetch failure destined for retry, or an early
	// return above that failure — so a stale percentage never lingers into
	// the next retry attempt or past ready_for_import. Found live: a
	// 7.9GB movie sat at "Fetching 100%" (the provider's own progress,
	// frozen) for however long the actual local copy took, with nothing
	// telling the user or an *arr app it was still doing anything at all.
	defer im.db.ClearFetchProgress(d.ID)

	p := im.providerForKind(d.Kind)
	if p == nil {
		return fmt.Errorf("no provider configured for kind %q", d.Kind)
	}

	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	files, err := p.Files(ctx, id)
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}

	destDir := im.resolveDestDir(d)
	if err := ensureWritableDir(destDir); err != nil {
		return fmt.Errorf("prepare destination directory: %w", err)
	}
	// destDir's own parent (the category folder) isn't something
	// AcerviNode necessarily created itself — best-effort only, not a hard
	// failure: an *arr app that can't remove the (hopefully now-empty)
	// release folder afterward is a real but much smaller problem than the
	// import failing outright, which ensureWritableDir(destDir) above
	// already prevents regardless of whether this succeeds.
	if parent := filepath.Dir(destDir); parent != destDir {
		if err := os.Chmod(parent, 0o777); err != nil {
			slog.Warn("importer: failed to make category directory world-writable, continuing anyway", "dir", parent, "error", err)
		}
	}

	var doneBytes int64
	for _, f := range files {
		fileDoneBytes := doneBytes // captured per-iteration, not the loop variable
		onProgress := func(fileWritten int64) {
			if d.SizeBytes <= 0 {
				return // nothing sane to divide by
			}
			im.db.SetFetchProgress(d.ID, float64(fileDoneBytes+fileWritten)/float64(d.SizeBytes))
		}
		if err := im.fetchFile(ctx, p, id, f, destDir, onProgress); err != nil {
			return fmt.Errorf("fetch file %q: %w", f.Path, err)
		}
		doneBytes += f.SizeBytes
		onProgress(0) // exact boundary update, not just whatever the throttled in-flight calls last happened to land on
	}

	// resolveDestDir returning something other than d.SavePath means it fell
	// back to a computed default (the adding *arr app never supplied an
	// explicit save_path — always true for a SABnzbd add, real SABnzbd has
	// no such parameter; sometimes true for qBittorrent too). That computed
	// path only ever existed locally until now — persisting it here is what
	// makes handleHistory/handleInfo/handleProperties report the real
	// location afterward, instead of an empty string the *arr app's own
	// import step has nothing to scan. See database.UpdateDownloadSavePath.
	if d.SavePath == "" {
		if err := im.db.UpdateDownloadSavePath(ctx, d.ID, destDir); err != nil {
			return fmt.Errorf("persist resolved save path: %w", err)
		}
		d.SavePath = destDir
	}

	now := time.Now().UTC()
	if err := im.db.UpdateDownloadStatus(ctx, d.ID, database.StateReadyForImport, 1.0, d.SizeBytes, &now, ""); err != nil {
		return fmt.Errorf("mark ready_for_import: %w", err)
	}
	slog.Info("importer: download ready", "id", d.ID, "name", d.Name, "dest", destDir, "files", len(files))
	return nil
}

// cleanupOldDownloads implements the retention/cleanup policy: once a
// Managed download has sat in ready_for_import for at least
// getCleanupAfterDays days, its local files, its provider-side copy, and
// its own row are all removed — an *arr app has already imported it
// elsewhere by then, so AcerviNode's own copy (and the debrid quota it's
// still occupying provider-side) is redundant. Disabled entirely when
// getCleanupAfterDays is 0 (the default) — see
// database.ListDownloadsEligibleForCleanup for the exact eligibility rule
// (Managed + ready_for_import only; never a Manual download, which would
// mean deleting something before the user ever grabbed it).
func (im *Importer) cleanupOldDownloads(ctx context.Context) {
	days := im.getCleanupAfterDays()
	if days <= 0 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	rows, err := im.db.ListDownloadsEligibleForCleanup(ctx, cutoff)
	if err != nil {
		slog.Error("importer: list downloads eligible for cleanup failed", "error", err)
		return
	}
	for _, d := range rows {
		im.cleanupDownload(ctx, d)
	}
}

// RemoveLocalFiles deletes d's local files from disk — the local-filesystem
// half of a "delete and remove files" request. internal/api, internal/qbittorrent,
// and internal/sabnzbd's own delete handlers all call this (through the
// Settings interface, since none of them otherwise know about download_dir/
// category-override config) rather than duplicating resolveDestDir's
// config-dependent path logic — before this existed, every one of them asked
// the provider to delete deleteFiles-style, but the provider call only ever
// removes the provider-side copy; nothing anywhere actually touched local
// disk outside the automatic retention/cleanup policy below. Refuses to
// touch anything for a row with no Name, matching cleanupDownload's own
// guard: resolveDestDir would otherwise collapse to the bare category
// directory shared with every other download in it.
func (im *Importer) RemoveLocalFiles(d *database.Download) error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("refusing to remove local files: download has no name")
	}
	return os.RemoveAll(im.resolveDestDir(d))
}

// cleanupDownload removes one download's local files, best-effort deletes
// it provider-side, and removes its row — see cleanupOldDownloads. Local
// file removal is skipped (with a warning, not silently) for a row with no
// Name, since resolveDestDir would otherwise collapse to the bare category
// directory shared with every other download in it — deliberately
// conservative rather than risking os.RemoveAll on something broader than
// intended. A tombstone is recorded before the row is deleted, the same
// race-avoidance database.RecordDeletedDownload already covers for a
// user-initiated delete (internal/api's handleDeleteDownload) — the
// provider's own delete isn't always instantly reflected in its listing
// endpoints, and this runs on the same independent tick as discovery.
func (im *Importer) cleanupDownload(ctx context.Context, d *database.Download) {
	destDir := im.resolveDestDir(d)
	if strings.TrimSpace(d.Name) == "" {
		slog.Warn("importer: cleanup skipping local file removal, download has no name", "id", d.ID, "dest", destDir)
	} else if err := os.RemoveAll(destDir); err != nil {
		slog.Warn("importer: cleanup failed to remove local files, continuing anyway", "id", d.ID, "dest", destDir, "error", err)
	}

	if p := im.providerForKind(d.Kind); p != nil {
		if err := p.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), true); err != nil {
			slog.Warn("importer: cleanup best-effort provider delete failed", "id", d.ID, "error", err)
		}
	}

	if err := im.db.RecordDeletedDownload(ctx, d.Provider, d.Kind, d.ProviderDownloadID); err != nil {
		slog.Error("importer: cleanup record deleted-download tombstone failed", "id", d.ID, "error", err)
	}
	if err := im.db.DeleteDownload(ctx, d.ID); err != nil {
		slog.Error("importer: cleanup delete row failed", "id", d.ID, "error", err)
		return
	}
	slog.Info("importer: cleaned up old download", "id", d.ID, "name", d.Name, "dest", destDir)
}

// progressReportInterval throttles how often progressWriter calls
// onProgress — frequent enough to feel live against the ~4s poll interval
// the native API/web UI already use, infrequent enough that it doesn't
// contend SetFetchProgress's mutex on every single Write call from
// io.Copy's default 32KB buffer (thousands of times a second for a large,
// fast transfer).
const progressReportInterval = 500 * time.Millisecond

// progressWriter wraps an io.Writer, invoking onProgress with the running
// total of bytes written so far — throttled by progressReportInterval. The
// very first Write always reports immediately (the zero-value lastReport
// makes time.Since huge), so progress shows up right away rather than
// waiting out the first interval.
type progressWriter struct {
	w          io.Writer
	written    int64
	onProgress func(written int64)
	lastReport time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if time.Since(p.lastReport) >= progressReportInterval {
		p.onProgress(p.written)
		p.lastReport = time.Now()
	}
	return n, err
}

// ensureWritableDir creates dir (and any missing parents) then makes sure
// it — even if it already existed — ends up world-writable (0777), not
// just requesting that mode from MkdirAll: the process's own umask
// (typically 022, systemd's default) silently strips the write bits at
// creation time regardless of the mode requested, the same as a plain
// `mkdir` would — an explicit Chmod afterward is the only way to guarantee
// they actually survive. Unconditional, not just for a directory this call
// happens to create fresh, so one left over from before this fix existed
// gets corrected retroactively too, the next time anything is fetched
// into it.
//
// This matters because of a real, live-diagnosed asymmetry: an *arr app
// importing a completed Managed download needs to move or hardlink the
// file out of wherever this wrote it, which requires write access on
// whichever directory directly contains it — not just read access to the
// file itself. AcerviNode's own process (a dedicated systemd user, not
// root) previously created these directories as 0755, writable only by
// that one user. Radarr's real SABnzbd import path always attempts a
// genuine move (confirmed against its source: CanMoveFiles is
// unconditionally true for every SABnzbd history item, unlike torrents —
// see below), so this broke every NZB-sourced Managed import outright with
// "Access ... is denied." Its qBittorrent import path only silently
// avoided the very same wall: Radarr only allows a move/hardlink there
// when the torrent is reported as paused after reaching its own configured
// seed limit, a state AcerviNode never reports (it has no real local
// seeding concept at all — TorBox handles that server-side) — so Radarr
// always fell back to copy-only for a qBittorrent-sourced item, silently
// doubling disk usage per import rather than erroring.
//
// World-writable, not just group-writable, deliberately: an *arr app
// almost never runs as the same user/group as AcerviNode's own dedicated
// systemd user — very commonly a separate Docker container with its own
// PUID/PGID, or (found live, on a real Proxmox/NAS deployment) AcerviNode
// itself running under some other ad hoc identity entirely. Matching
// group/user IDs across genuinely separate deployments (containers, VMs,
// even LXC UID-namespace remapping) is real, ongoing coordination a user
// would otherwise have to redo for every fresh install — the standard
// self-hosted-media-stack answer to this (linuxserver.io's PUID/PGID
// convention, confirmed live against a real reference client, rdt-client's
// own Dockerfile/README-DOCKER.md) is to make every container share one
// identity, which AcerviNode can't ask of apps it doesn't control the
// packaging of. World-writable download directories are the zero-
// configuration equivalent, and a narrow one: it only loosens these
// specific per-download directories, nothing else AcerviNode manages.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return fmt.Errorf("make directory world-writable: %w", err)
	}
	return nil
}

// fetchFile resolves a real download link and streams it to destDir/f.Path,
// skipping files already present at the expected size (the idempotency
// story for this version — a resumed download re-fetches a partial file
// from scratch rather than range-resuming it). onProgress is called
// (throttled — see progressWriter) with cumulative bytes written for this
// file as the transfer proceeds; not called at all for an already-fetched
// file skipped above — processDownload accounts for that file's full size
// in its own running total regardless, via its own boundary update after
// this returns.
func (im *Importer) fetchFile(ctx context.Context, p provider, id debrid.ProviderDownloadID, f debrid.DownloadFile, destDir string, onProgress func(written int64)) error {
	destPath, err := safeJoin(destDir, f.Path)
	if err != nil {
		return err
	}

	if info, err := os.Stat(destPath); err == nil && !info.IsDir() && info.Size() == f.SizeBytes {
		return nil // already fetched
	}

	if err := ensureWritableDir(filepath.Dir(destPath)); err != nil {
		return err
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
	pw := &progressWriter{w: out, onProgress: onProgress}
	if _, err := io.Copy(pw, resp.Body); err != nil {
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
