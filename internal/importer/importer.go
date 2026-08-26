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
	"regexp"
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

// listCachedProvider is the optional half of provider, implemented by
// debrid's Dynamic*Provider wrappers — the same pointers the compat shims
// hold. Going through it means one provider listing per kind per interval
// serves the importer and every *arr app at once, instead of each fetching
// its own; see debrid.ListCache. Optional rather than folded into provider
// so a plain provider (a test fake, or any implementation that has no
// reason to know about caching) still satisfies the interface — such a
// caller just fetches directly, exactly as before.
type listCachedProvider interface {
	ListFresh(ctx context.Context) ([]debrid.DownloadStatus, time.Time, error)
	SetListCacheTTL(ttl time.Duration)
}

// listWithCache fetches through p's shared listing cache when it has one,
// so the result is shared with everything else holding that provider —
// deliberately via ListFresh, which always fetches: this poll is what drives
// state transitions and must not read data it cached an interval ago. The
// returned time is when the underlying call started — see
// database.RefreshFromProvider's ordering guard.
func listWithCache(ctx context.Context, p provider) ([]debrid.DownloadStatus, time.Time, error) {
	if lc, ok := p.(listCachedProvider); ok {
		return lc.ListFresh(ctx)
	}
	fetchedAt := time.Now()
	statuses, err := p.List(ctx)
	return statuses, fetchedAt, err
}

// maxBackoff caps how long a failing download waits between retries,
// regardless of how the exponential backoff computation grows — a hardcoded
// ceiling rather than another config knob, since "eventually give up" is the
// one lever (MaxRetries) that's actually worth exposing.
const maxBackoff = time.Hour

// Importer periodically refreshes every tracked download's status from its
// provider and fetches provider_completed downloads' files to local disk.
type Importer struct {
	db *database.DB
	// registry is every configured provider. Polling walks it per kind, so
	// a second provider is picked up without any further wiring; anything
	// acting on a specific download resolves that download's own provider
	// through it (see providerFor).
	registry   *debrid.Registry
	httpClient *http.Client

	// mu guards downloadDir/interval/maxRetries/categoryPaths/maxConcurrent/
	// fetchTimeout/webDownloadProvider/cleanupAfterDays/dirMode/
	// fastPollInterval/minFetchFileSizeBytes/includeFileRegex/
	// excludeFileRegex/stuckDownloadTimeout/cleanupErrorAfterDays, which
	// SetConfig/SetCategoryPaths/SetMaxConcurrent/SetFetchTimeout/
	// SetWebDownloadProvider/SetCleanupAfterDays/SetDirMode/
	// SetFastPollInterval/SetFileFilters/SetStuckDownloadTimeout/
	// SetCleanupErrorAfterDays can change live (see cmd/acervinode's
	// liveSettings) — everything else on Importer is set once at
	// construction and never mutated afterward.
	mu               sync.Mutex
	downloadDir      string
	interval         time.Duration // also the backoff base: attempt N waits ~interval*2^N
	maxRetries       int
	categoryPaths    map[string]string // category name -> override dir, replacing downloadDir/<category> for that category
	maxConcurrent    int               // how many downloads Tick fetches to disk at once
	fetchTimeout     time.Duration     // per-file fetch deadline — see fetchFile
	cleanupAfterDays int               // 0 disables the retention/cleanup policy — see cleanupOldDownloads
	dirMode          os.FileMode       // permission mode every download directory is created with — see ensureWritableDir
	fastPollInterval time.Duration     // see fastPollIntervalDefault's own doc comment

	// minFetchFileSizeBytes/maxFetchFileSizeBytes/includeFileRegex/
	// excludeFileRegex back the per-file filtering policy — see
	// filterFiles. 0/0/nil/nil (the defaults) disable each check
	// independently.
	minFetchFileSizeBytes int64
	maxFetchFileSizeBytes int64
	includeFileRegex      *regexp.Regexp
	excludeFileRegex      *regexp.Regexp
	// stuckDownloadTimeout backs the stuck-download watchdog — see
	// checkStuckDownloads. 0 (the default) disables it.
	stuckDownloadTimeout time.Duration
	// cleanupErrorAfterDays backs the error-cleanup policy — see
	// cleanupErroredDownloads. 0 (the default) disables it.
	cleanupErrorAfterDays int

	// intervalChanged carries a fresh interval into Run's select loop so a
	// live SetConfig call can reset the ticker without Run having to poll
	// for changes. Buffered 1: a SetConfig that lands while Run hasn't
	// consumed the previous change just overwrites it — only the latest
	// interval matters.
	intervalChanged chan time.Duration
	// fastPollIntervalChanged is intervalChanged's exact counterpart for
	// runFastPoll's own ticker — see SetFastPollInterval.
	fastPollIntervalChanged chan time.Duration

	// rateLimitMu guards rateLimitState — see refreshKind's cooldown check
	// and recordRateLimitHit/clearRateLimitHit. Purely in-memory,
	// deliberately: this is operational backoff state, not something that
	// needs to survive a restart the way a download's own retry_count does
	// (a restart just means starting the cooldown clock over, which is
	// fine — the provider's rate limit itself is what actually governs
	// this, not AcerviNode's memory of it).
	rateLimitMu sync.Mutex
	// Keyed per provider *and* kind: rate limits are enforced per account,
	// so one provider being limited must not stall polling for another.
	// Sharing a single per-kind key would reintroduce exactly the freeze
	// this backoff exists to survive, just spread across providers.
	rateLimitState map[providerKind]*kindBackoff

	// statsMu guards tickAt/successfulListAt — passive health-signal
	// bookkeeping for GET /api/v1/status (see LastTickAt/
	// LastSuccessfulListAt), never read by any of the importer's own logic.
	// Deliberately in-memory only, same reasoning as rateLimitState: a
	// restart just means these start reporting zero again until the next
	// tick/list, which is fine for a liveness signal.
	statsMu          sync.Mutex
	tickAt           time.Time
	successfulListAt map[providerKind]time.Time
	// listStreak counts consecutive successful bulk listings per provider
	// and kind, reset to zero by any failure. Vanished-download detection
	// is gated on it — see trustedListStreak.
	listStreak map[providerKind]int

	// activeFetchesMu guards activeFetches — one entry per download
	// currently inside processDownload, keyed by download id. Two jobs:
	// (1) lets CancelFetch interrupt a specific in-flight fetch and wait
	// for it to genuinely stop — see handleDeleteDownload in internal/api,
	// the only caller, added to close a theoretically real race identified
	// by code inspection: deleting a download while it was still being
	// fetched could leave an orphaned file on disk, because the fetch
	// goroutine had no way to know the row it was writing for had just been
	// deleted and would keep going. (2) doubles as a
	// guard against processing the same download twice at once — a fetch
	// that outlives a single Tick (a large multi-file torrent taking
	// longer than import_interval_seconds) would otherwise still be sat in
	// StateProviderCompleted with no next_retry_at set when the *next*
	// Tick's own ListDownloadsDueForRetry runs, and get handed to a second,
	// fully concurrent processDownload goroutine writing into the same
	// destination directory — a latent hazard this closes as a side effect
	// of the same tracking, not a separate mechanism.
	activeFetchesMu sync.Mutex
	activeFetches   map[string]*activeFetch
}

// activeFetch is one entry in Importer.activeFetches — see its own doc
// comment for why this exists. done is closed (never sent on) the moment
// processDownload returns, however it returns; CancelFetch waits on it
// after calling cancel so a caller can be sure the fetch has genuinely
// stopped, not just been asked to.
type activeFetch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// kindBackoff tracks one kind's (torrent/usenet/webdl) rate-limit backoff —
// see refreshKind.
// trustedListStreak is how many consecutive successful bulk listings a
// provider must answer before its listings are trusted enough to conclude a
// download has vanished. Anything less and a listing that came back short
// during a wobble would erode missing_count toward flagging downloads that
// are still perfectly present.
//
// Deliberately paired with database.missingDetectionThreshold rather than
// replacing it: that one requires a row to be absent from several
// consecutive listings, this one requires those listings to have come from
// a provider that was actually answering reliably. A sustained outage
// simply never reaches the streak, so nothing is ever flagged during one —
// which is the behaviour a real multi-day provider outage showed was
// missing.
const trustedListStreak = 3

// recordListSuccess advances pk's consecutive-success streak and reports
// whether the provider is now trusted enough for vanished-download
// detection.
func (im *Importer) recordListSuccess(pk providerKind, at time.Time) bool {
	im.statsMu.Lock()
	defer im.statsMu.Unlock()
	im.successfulListAt[pk] = at
	im.listStreak[pk]++
	return im.listStreak[pk] >= trustedListStreak
}

// recordListFailure resets pk's streak, so a provider that just failed has
// to prove itself again before anything is concluded from its listings.
func (im *Importer) recordListFailure(pk providerKind) {
	im.statsMu.Lock()
	defer im.statsMu.Unlock()
	im.listStreak[pk] = 0
}

// providerKind identifies one provider's handling of one kind — the unit
// both rate-limit backoff and list-success bookkeeping are tracked against,
// since a provider is limited (or healthy) per account, not globally.
type providerKind struct {
	provider string
	kind     database.Kind
}

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

// fastPollInterval (im.fastPollInterval) is how often refreshActiveDownloads
// checks each actively in-flight (queued/downloading) Managed download
// individually, via a single targeted per-ID Status() call rather than
// waiting for the next bulk List() tick (im.interval, 10s by default) to
// notice it. A live, controlled, same-account comparison against
// rogerfar/rdt-client (a reference debrid download client) found AcerviNode
// taking ~2x longer to notice an already-cached file was ready via an
// equivalent auto-fetch path — traced to exactly this: nothing polled more
// often than the bulk interval, so a download that finished moments after a
// tick simply waited for the next one. A targeted per-ID lookup is
// dramatically cheaper against a provider's rate limit than shortening the
// bulk interval itself would be — confirmed live the hard way (a 2s bulk
// interval, still listing every download on the account three times over,
// tripped TorBox's real rate limit immediately) — the same principle a
// reference implementation (decypharr) applies for its own active-download
// polling, see docs/providers.md.
//
// fastPollIntervalDefault is New's starting value — live-changeable
// afterward via SetFastPollInterval (see config.Config.FastPollIntervalSeconds),
// unlike when this was a fixed const: a user with many downloads actively
// in flight at once might want to widen it themselves to stay further
// from a rate limit than the default already does, without needing a
// code change to do it.
const fastPollIntervalDefault = 3 * time.Second

// ensureWritableDirModeDefault is the fallback dirMode New() starts with —
// see config.Config.DownloadDirMode's own doc comment for why 0777 is the
// default.
const ensureWritableDirModeDefault = os.FileMode(0o777)

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
func New(db *database.DB, registry *debrid.Registry, downloadDir string, interval time.Duration, maxRetries int) *Importer {
	im := &Importer{
		db:                      db,
		registry:                registry,
		downloadDir:             downloadDir,
		interval:                interval,
		maxRetries:              maxRetries,
		categoryPaths:           map[string]string{},
		maxConcurrent:           1,
		fetchTimeout:            10 * time.Minute,
		dirMode:                 ensureWritableDirModeDefault,
		fastPollInterval:        fastPollIntervalDefault,
		httpClient:              &http.Client{}, // no client-wide Timeout — fetchFile derives a per-request one from fetchTimeout instead, since it can change live
		intervalChanged:         make(chan time.Duration, 1),
		fastPollIntervalChanged: make(chan time.Duration, 1),
		rateLimitState:          map[providerKind]*kindBackoff{},
		successfulListAt:        map[providerKind]time.Time{},
		listStreak:              map[providerKind]int{},
		activeFetches:           map[string]*activeFetch{},
	}
	im.applyListCacheTTL(interval)
	return im
}

// applyListCacheTTL points every registered provider's shared listing cache
// at ttl — see debrid.ListCache and SetConfig.
func (im *Importer) applyListCacheTTL(ttl time.Duration) {
	if im.registry == nil {
		return
	}
	for _, name := range im.registry.Names() {
		if p := im.registry.Torrent(name); p != nil {
			p.SetListCacheTTL(ttl)
		}
		if p := im.registry.Usenet(name); p != nil {
			p.SetListCacheTTL(ttl)
		}
		if p := im.registry.WebDL(name); p != nil {
			p.SetListCacheTTL(ttl)
		}
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
	// The shared listing cache's lifetime tracks this interval: it's already
	// the user's answer to how often the provider should be asked, and a
	// shim request has no reason to answer it differently.
	im.applyListCacheTTL(interval)

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

// SetDirMode updates the permission mode every download directory is
// created with, live — the next directory ensureWritableDir touches (new
// or already existing) uses the new mode immediately.
func (im *Importer) SetDirMode(mode os.FileMode) {
	im.mu.Lock()
	im.dirMode = mode
	im.mu.Unlock()
}

func (im *Importer) getDirMode() os.FileMode {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.dirMode
}

// DirMode is the exported counterpart of getDirMode, for callers outside
// this package confirming a SetDirMode call took (see cmd/acervinode's
// settings tests).
func (im *Importer) DirMode() os.FileMode {
	return im.getDirMode()
}

// SetFastPollInterval updates the fast per-download poll interval live —
// runFastPoll's ticker is reset to the new value right away rather than
// waiting out whatever's left of the old one, the same treatment SetConfig
// already gives im.interval.
func (im *Importer) SetFastPollInterval(d time.Duration) {
	im.mu.Lock()
	changed := d != im.fastPollInterval
	im.fastPollInterval = d
	im.mu.Unlock()

	if !changed {
		return
	}
	select {
	case im.fastPollIntervalChanged <- d:
	default:
		select {
		case <-im.fastPollIntervalChanged:
		default:
		}
		im.fastPollIntervalChanged <- d
	}
}

func (im *Importer) getFastPollInterval() time.Duration {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.fastPollInterval
}

// FastPollInterval is the exported counterpart of getFastPollInterval, for
// callers outside this package confirming a SetFastPollInterval call took.
func (im *Importer) FastPollInterval() time.Duration {
	return im.getFastPollInterval()
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

// SetFileFilters updates the per-file fetch filtering policy live — the
// next download processed uses the new values immediately. minBytes <= 0
// disables the minimum-size check, maxBytes <= 0 disables the maximum-size
// check; includeRegex/excludeRegex nil disables each respectively. Callers
// are responsible for compiling the regexes themselves (see
// config.Config.Validate, which already confirms they compile before a
// candidate settings update is ever accepted) — Importer stores compiled
// patterns, not raw strings, since it never needs to serialize them
// anywhere.
func (im *Importer) SetFileFilters(minBytes, maxBytes int64, includeRegex, excludeRegex *regexp.Regexp) {
	im.mu.Lock()
	im.minFetchFileSizeBytes = minBytes
	im.maxFetchFileSizeBytes = maxBytes
	im.includeFileRegex = includeRegex
	im.excludeFileRegex = excludeRegex
	im.mu.Unlock()
}

func (im *Importer) getFileFilters() (minBytes, maxBytes int64, includeRegex, excludeRegex *regexp.Regexp) {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.minFetchFileSizeBytes, im.maxFetchFileSizeBytes, im.includeFileRegex, im.excludeFileRegex
}

// SetStuckDownloadTimeout updates the stuck-download watchdog live (see
// checkStuckDownloads) — the next Tick uses the new value immediately. 0
// (or negative) disables it entirely.
func (im *Importer) SetStuckDownloadTimeout(d time.Duration) {
	im.mu.Lock()
	im.stuckDownloadTimeout = d
	im.mu.Unlock()
}

func (im *Importer) getStuckDownloadTimeout() time.Duration {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.stuckDownloadTimeout
}

// SetCleanupErrorAfterDays updates the error-cleanup policy live (see
// cleanupErroredDownloads) — the next Tick uses the new value immediately.
// 0 (or negative) disables it entirely.
func (im *Importer) SetCleanupErrorAfterDays(days int) {
	im.mu.Lock()
	im.cleanupErrorAfterDays = days
	im.mu.Unlock()
}

func (im *Importer) getCleanupErrorAfterDays() int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.cleanupErrorAfterDays
}

// FileFilters is the exported counterpart of getFileFilters, for callers
// outside this package confirming a SetFileFilters call took (see
// cmd/acervinode's settings tests).
func (im *Importer) FileFilters() (minBytes, maxBytes int64, includeRegex, excludeRegex *regexp.Regexp) {
	return im.getFileFilters()
}

// StuckDownloadTimeout is the exported counterpart of
// getStuckDownloadTimeout, for callers outside this package confirming a
// SetStuckDownloadTimeout call took (see cmd/acervinode's settings tests).
func (im *Importer) StuckDownloadTimeout() time.Duration {
	return im.getStuckDownloadTimeout()
}

// CleanupErrorAfterDays is the exported counterpart of
// getCleanupErrorAfterDays, for callers outside this package confirming a
// SetCleanupErrorAfterDays call took (see cmd/acervinode's settings tests).
func (im *Importer) CleanupErrorAfterDays() int {
	return im.getCleanupErrorAfterDays()
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
				logTickError(ctx, "importer: tick failed", "error", err)
			}
		}
	}
}

// runFastPoll drives refreshActiveDownloads on fastPollInterval until ctx is
// done — see Run's doc comment for why this is a separate goroutine rather
// than another case in Run's own select loop.
func (im *Importer) runFastPoll(ctx context.Context) {
	ticker := time.NewTicker(im.getFastPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case newInterval := <-im.fastPollIntervalChanged:
			ticker.Reset(newInterval)
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
// checkStuckDownloads runs right after (a no-op unless
// stuck_download_timeout_minutes is configured), reflecting the freshest
// possible updated_at for every row before deciding anything looks stuck.
// cleanupOldDownloads/cleanupErroredDownloads run the retention policies
// (each a no-op unless its own days setting is configured) — last, so a
// download that just reached ready_for_import or StateError this same tick
// isn't somehow considered for cleanup before its own timestamp has even had
// a chance to age past the cutoff.
// logTickError reports a failure from inside a tick, unless the tick's
// context has been cancelled — in which case the failure is only the process
// shutting down mid-work, and calling it an error is actively misleading.
//
// A clean restart used to emit three or four ERROR lines every time: the
// provider listing failing with "context canceled", and the tick itself
// failing with "sql: database is closed" as the connection went away
// underneath it. Nothing was wrong, but it made "count the errors in the
// log" useless as a health signal — which matters now that GET
// /api/v1/status exists to be monitored.
//
// Keyed on ctx rather than on the error text because the errors vary
// (context.Canceled, database/sql's unexported errDBClosed, whatever a
// provider returns when its request is cut short) while the cause does not.
// The importer's tick context is the long-lived one and is never given a
// deadline of its own, so a cancelled context here always means shutdown
// rather than a timeout.
func logTickError(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil {
		slog.Debug(msg+" (ignored, shutting down)", args...)
		return
	}
	slog.Error(msg, args...)
}

func (im *Importer) Tick(ctx context.Context) error {
	im.statsMu.Lock()
	im.tickAt = time.Now()
	im.statsMu.Unlock()

	im.refreshStatuses(ctx)
	im.checkStuckDownloads(ctx)

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
	im.cleanupErroredDownloads(ctx)
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
		logTickError(ctx, "importer: check for any existing downloads failed", "error", err)
	} else {
		freshInstall = !hasAny
	}

	im.eachProvider(func(kind database.Kind, name string, p provider) {
		im.refreshKind(ctx, kind, name, p, freshInstall)
	})
}

// eachProvider calls fn for every registered provider that supports each
// kind, in the registry's stable order. Kinds are walked in a fixed order
// so a tick's behaviour doesn't depend on map iteration — see discoverManual
// on why ordering between kinds is observable at all.
func (im *Importer) eachProvider(fn func(kind database.Kind, name string, p provider)) {
	if im.registry == nil {
		return
	}
	for _, name := range im.registry.TorrentNames() {
		if p := im.registry.Torrent(name); p != nil {
			fn(database.KindTorrent, name, p)
		}
	}
	for _, name := range im.registry.UsenetNames() {
		if p := im.registry.Usenet(name); p != nil {
			fn(database.KindUsenet, name, p)
		}
	}
	// Every webdl row is always AddedViaManual (no *arr-facing shim exists
	// for it — see database.KindWebDL), but discovery/status-refresh still
	// applies the same way it does for a discovered Manual torrent/usenet
	// download: this is what makes a hoster link added directly through
	// TorBox's own site show up here too, not just links added through
	// AcerviNode's own "+ Add" form.
	for _, name := range im.registry.WebDLNames() {
		if p := im.registry.WebDL(name); p != nil {
			fn(database.KindWebDL, name, p)
		}
	}
}

func (im *Importer) refreshKind(ctx context.Context, kind database.Kind, name string, p provider, freshInstall bool) {
	if p == nil {
		return
	}
	pk := providerKind{provider: name, kind: kind}
	if until, cooling := im.rateLimitCooldown(pk); cooling {
		slog.Warn("importer: skipping provider list, still in rate-limit cooldown", "provider", name, "kind", kind, "until", until)
		return
	}
	// The provider listing is fetched first and the local snapshot read
	// only after it returns, deliberately. List() is a network round-trip
	// that can take seconds, and an *arr add landing during it would be
	// absent from a snapshot taken beforehand — discoverManual would then
	// see the provider's copy of that brand-new Managed download as
	// untracked and adopt it as Manual, stranding an *arr-requested
	// download in the Manual tab where it's never auto-fetched. Reading
	// afterwards narrows the window to an add landing mid-List(), which
	// database.InsertOrClaimForArr then resolves by claiming the row.
	//
	// Nothing is skipped when no rows are tracked yet: discoverManual still
	// needs the listing to catch a first-ever manually-added download for a
	// kind nothing's tracked.
	statuses, fetchedAt, err := listWithCache(ctx, p)
	if err != nil {
		if errors.Is(err, debrid.ErrRateLimited) {
			until := im.recordRateLimitHit(pk)
			im.recordListFailure(pk)
			slog.Error("importer: provider rate limited, backing off", "provider", name, "kind", kind, "until", until, "error", err)
			return
		}
		im.recordListFailure(pk)
		// Not yet configured is routine (e.g. no TorBox key set yet) and
		// would otherwise log an error every single tick — everything else
		// is worth surfacing.
		if !errors.Is(err, debrid.ErrNoProvider) {
			logTickError(ctx, "importer: provider list failed", "kind", kind, "error", err)
		}
		return
	}
	im.clearRateLimitHit(pk)
	trusted := im.recordListSuccess(pk, fetchedAt)

	// Scoped to this provider, not just this kind. A refresh compares
	// tracked rows against one provider's listing, so including another
	// provider's rows makes them all look absent and missing-detection
	// flags them as vanished from an account that was never asked about
	// them — see database.ListDownloadsByProvider.
	rows, err := im.db.ListDownloadsByProvider(ctx, name, kind)
	if err != nil {
		logTickError(ctx, "importer: list downloads failed", "provider", name, "kind", kind, "error", err)
		return
	}
	// Only this pass ever concludes a download has vanished, and only once
	// the provider has proven steady — see database.RefreshOptions.
	im.db.RefreshFromProvider(ctx, rows, statuses, fetchedAt, database.RefreshOptions{
		DetectMissing: trusted,
		Provider:      name,
		Kind:          kind,
	})
	im.discoverManual(ctx, kind, p.Name(), rows, statuses, freshInstall)
}

// refreshActiveDownloads is the fast path's entry point: for every Managed
// download currently queued/downloading (per kind), it checks that one
// download's status directly via a targeted per-ID lookup instead of relying
// solely on the next bulk refreshStatuses tick — see fastPollInterval.
func (im *Importer) refreshActiveDownloads(ctx context.Context) {
	im.eachProvider(func(kind database.Kind, name string, p provider) {
		im.refreshActiveKind(ctx, kind, name, p)
	})
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
func (im *Importer) refreshActiveKind(ctx context.Context, kind database.Kind, name string, p provider) {
	if p == nil {
		return
	}
	pk := providerKind{provider: name, kind: kind}
	if _, cooling := im.rateLimitCooldown(pk); cooling {
		return
	}
	rows, err := im.db.ListActiveManagedDownloads(ctx, kind)
	if err != nil {
		logTickError(ctx, "importer: list active managed downloads failed", "kind", kind, "error", err)
		return
	}
	for _, d := range rows {
		// Only this provider's own rows: another provider's download of the
		// same kind is polled on its own pass, against its own account.
		if d.Provider != "" && d.Provider != name {
			continue
		}
		fetchedAt := time.Now()
		st, err := p.Status(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID))
		if err != nil {
			if errors.Is(err, debrid.ErrRateLimited) {
				until := im.recordRateLimitHit(pk)
				slog.Error("importer: provider rate limited, backing off", "provider", name, "kind", kind, "until", until, "error", err)
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
		im.clearRateLimitHit(pk)
		im.db.RefreshFromProvider(ctx, []*database.Download{d}, []debrid.DownloadStatus{st}, fetchedAt, database.RefreshOptions{})
	}
}

// rateLimitCooldown reports whether kind is still within a backoff window
// set by a previous recordRateLimitHit — see refreshKind, the only caller.
func (im *Importer) rateLimitCooldown(pk providerKind) (time.Time, bool) {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()
	state, ok := im.rateLimitState[pk]
	if !ok || !time.Now().Before(state.nextAttempt) {
		return time.Time{}, false
	}
	return state.nextAttempt, true
}

// recordRateLimitHit advances kind's consecutive-hit count and schedules its
// next allowed attempt with exponential backoff (rateLimitBackoffBase *
// 2^hits, capped at rateLimitBackoffMax) — see refreshKind, the only caller.
func (im *Importer) recordRateLimitHit(pk providerKind) time.Time {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()
	state, ok := im.rateLimitState[pk]
	if !ok {
		state = &kindBackoff{}
		im.rateLimitState[pk] = state
	}
	state.consecutiveHits++
	state.nextAttempt = time.Now().Add(rateLimitBackoffDuration(state.consecutiveHits))
	return state.nextAttempt
}

// clearRateLimitHit drops kind's backoff state entirely once a List() call
// succeeds — the next rate limit (if any) starts counting from scratch
// rather than continuing to grow from wherever it last left off.
func (im *Importer) clearRateLimitHit(pk providerKind) {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()
	delete(im.rateLimitState, pk)
}

// RateLimitCooldownUntil is the exported counterpart of rateLimitCooldown —
// originally added for tests confirming a rate-limit hit actually set a
// cooldown, now also read for real by GET /api/v1/status (see
// cmd/acervinode's liveSettings.Status).
// Aggregated across providers: reports the furthest-out cooldown for kind,
// so "is anything of this kind currently paused" keeps its existing meaning
// for callers (the status endpoint and the UI's rate-limit banner) now that
// the state underneath is per provider.
func (im *Importer) RateLimitCooldownUntil(kind database.Kind) (time.Time, bool) {
	im.rateLimitMu.Lock()
	defer im.rateLimitMu.Unlock()

	var latest time.Time
	now := time.Now()
	for pk, state := range im.rateLimitState {
		if pk.kind != kind || !now.Before(state.nextAttempt) {
			continue
		}
		if state.nextAttempt.After(latest) {
			latest = state.nextAttempt
		}
	}
	return latest, !latest.IsZero()
}

// ProviderKindStatus is one provider's handling of one kind, for
// GET /api/v1/status. The aggregate accessors above answer "is this kind
// working at all"; this answers "which provider is the one struggling",
// which an aggregate cannot: with two providers configured, one failing
// every list while the other succeeds still leaves the kind looking healthy,
// because the healthy one keeps the timestamp moving.
type ProviderKindStatus struct {
	Provider             string
	Kind                 string
	LastSuccessfulListAt time.Time
	RateLimitedUntil     time.Time
	// ListingAnomalousSince is when this provider/kind's listing started
	// failing the mass-vanish guard, if it currently is. A listing can be
	// succeeding — LastSuccessfulListAt moving, no rate limit — while still
	// being disbelieved, which previously showed up nowhere except a
	// repeated log line.
	ListingAnomalousSince time.Time
}

// ProviderStatuses reports every provider/kind pair the importer actually
// polls, in the registry's stable order so the output doesn't reshuffle
// between requests.
func (im *Importer) ProviderStatuses() []ProviderKindStatus {
	var out []ProviderKindStatus
	im.eachProvider(func(kind database.Kind, name string, _ provider) {
		pk := providerKind{provider: name, kind: kind}

		im.statsMu.Lock()
		listAt := im.successfulListAt[pk]
		im.statsMu.Unlock()

		var until time.Time
		im.rateLimitMu.Lock()
		if st, ok := im.rateLimitState[pk]; ok && time.Now().Before(st.nextAttempt) {
			until = st.nextAttempt
		}
		im.rateLimitMu.Unlock()

		anomalousSince, _ := im.db.MassVanishSince(database.MassVanishScope(name, kind))
		out = append(out, ProviderKindStatus{
			Provider:              name,
			Kind:                  string(kind),
			LastSuccessfulListAt:  listAt,
			RateLimitedUntil:      until,
			ListingAnomalousSince: anomalousSince,
		})
	})
	return out
}

// LastTickAt reports when Tick last ran, regardless of what it found once
// inside — a liveness signal for GET /api/v1/status: if this stops
// advancing, the tick loop itself has stalled or crashed, as opposed to one
// specific provider kind failing to list (see LastSuccessfulListAt). false
// if Tick has never run yet.
func (im *Importer) LastTickAt() (time.Time, bool) {
	im.statsMu.Lock()
	defer im.statsMu.Unlock()
	if im.tickAt.IsZero() {
		return time.Time{}, false
	}
	return im.tickAt, true
}

// LastSuccessfulListAt reports when kind's provider last answered a bulk
// List() call without erroring — see refreshKind. false if it never has
// (including if no provider is configured for kind at all).
// Aggregated across providers: the most recent success for kind, so this
// keeps answering "is polling for this kind working at all" as it did
// before the state underneath became per provider.
func (im *Importer) LastSuccessfulListAt(kind database.Kind) (time.Time, bool) {
	im.statsMu.Lock()
	defer im.statsMu.Unlock()

	var latest time.Time
	for pk, t := range im.successfulListAt {
		if pk.kind == kind && t.After(latest) {
			latest = t
		}
	}
	return latest, !latest.IsZero()
}

// ErrorCounts reports how many downloads currently sit in StateError, keyed
// by kind — see database.CountDownloadsByState. Backs GET /api/v1/status;
// nothing in the importer's own logic reads this.
func (im *Importer) ErrorCounts(ctx context.Context) (map[database.Kind]int, error) {
	return im.db.CountDownloadsByState(ctx, database.StateError)
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

// providerForKind returns the configured provider for a kind, with no
// regard for any particular download — for the bulk polling paths, which
// are inherently per-kind (refreshStatuses, refreshActiveDownloads).
// Anything acting on a *specific* download must use providerFor instead.
func (im *Importer) providerForKind(kind database.Kind) provider {
	if im.registry == nil {
		return nil
	}
	return im.providerNamed(im.registry.Default(), kind)
}

// providerNamed resolves one provider's handling of one kind, or nil if
// that provider isn't registered or doesn't support the kind. Returns the
// concrete wrapper as the local provider interface, and is careful to
// return a genuinely nil interface rather than one holding a nil pointer.
func (im *Importer) providerNamed(name string, kind database.Kind) provider {
	if im.registry == nil {
		return nil
	}
	switch kind {
	case database.KindTorrent:
		if p := im.registry.Torrent(name); p != nil {
			return p
		}
	case database.KindUsenet:
		if p := im.registry.Usenet(name); p != nil {
			return p
		}
	case database.KindWebDL:
		if p := im.registry.WebDL(name); p != nil {
			return p
		}
	}
	return nil
}

// providerFor returns the provider d actually belongs to, or nil if that
// provider isn't currently reachable — shared by processDownload and
// cleanupDownload, both of which act on one specific download.
//
// Resolving by kind alone would be wrong the moment more than one provider
// is configured: every row already records which provider it came from
// (database.Download.Provider), and its provider_download_id means nothing
// to a different account. Today the check also catches a real
// single-provider case — a download added under one API key, still tracked,
// after the key has been swapped for a different account. Fetching it would
// otherwise ask the new account for an id it has never heard of.
func (im *Importer) providerFor(d *database.Download) provider {
	name := d.Provider
	if name == "" {
		// Older rows predate the column being populated; the default is the
		// only sensible guess and matches the pre-registry behaviour.
		name = im.registry.Default()
	}
	p := im.providerNamed(name, d.Kind)
	if p == nil {
		slog.Warn("importer: no provider available for download",
			"id", d.ID, "download_provider", d.Provider, "resolved_name", name, "kind", d.Kind)
	}
	return p
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

// tryStartFetch registers id as actively fetching and returns the
// cancellable context processDownload should use for the rest of its work,
// plus a done func it must call (via defer) exactly once when it returns.
// ok is false if id is already registered — another goroutine (an
// overlapping Tick, for a fetch that outlived one import_interval_seconds —
// see Importer.activeFetches' own doc comment) is already processing this
// same download; the caller should treat that as "nothing to do this tick",
// not a failure.
func (im *Importer) tryStartFetch(ctx context.Context, id string) (fetchCtx context.Context, doneFn func(), ok bool) {
	im.activeFetchesMu.Lock()
	defer im.activeFetchesMu.Unlock()
	if _, exists := im.activeFetches[id]; exists {
		return nil, nil, false
	}
	fetchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	im.activeFetches[id] = &activeFetch{cancel: cancel, done: done}
	return fetchCtx, func() {
		cancel()
		close(done)
		im.activeFetchesMu.Lock()
		delete(im.activeFetches, id)
		im.activeFetchesMu.Unlock()
	}, true
}

// CancelFetch interrupts id's in-flight fetch, if one is currently running,
// and blocks (up to a generous bound, so a caller — handleDeleteDownload,
// the only one — never hangs indefinitely on a stuck goroutine) until it has
// genuinely stopped, not just been asked to. A no-op, returning immediately,
// if nothing is actively fetching id right now. See Importer.activeFetches'
// own doc comment for why this exists: without it, deleting a download
// while it's mid-fetch could leave an orphaned file on disk, since the
// fetch goroutine had no way to know the row it was writing for had just
// been deleted and would keep going regardless.
func (im *Importer) CancelFetch(id string) {
	im.activeFetchesMu.Lock()
	fetch, ok := im.activeFetches[id]
	im.activeFetchesMu.Unlock()
	if !ok {
		return
	}
	fetch.cancel()
	select {
	case <-fetch.done:
	case <-time.After(10 * time.Second):
		slog.Warn("importer: timed out waiting for in-flight fetch to stop after cancellation", "id", id)
	}
}

// filterFiles applies the configured minimum/maximum-file-size and
// include/exclude regex filters (see SetFileFilters) to files before
// processDownload fetches them to local disk. A file is kept only if it's
// at least the minimum size AND at most the maximum size (when each is set)
// AND (no include pattern, or its path matches it) AND (no exclude
// pattern, or its path doesn't match it) — every check that's actually
// configured must pass; a file has to satisfy all of them. Matched against
// the file's path (its name, or a relative path for a multi-file torrent's
// subdirectory structure) for the regex checks, not its size or any other
// field. Purely local: never changes what the provider itself considers
// part of the download, or what shows in GET /api/v1/downloads/{id}'s own
// files list — only which of those files actually get written to disk.
// Returns files unchanged (not a copy) when nothing is configured, the
// common case, so this costs nothing when unused.
func (im *Importer) filterFiles(files []debrid.DownloadFile) []debrid.DownloadFile {
	minBytes, maxBytes, include, exclude := im.getFileFilters()
	if minBytes <= 0 && maxBytes <= 0 && include == nil && exclude == nil {
		return files
	}
	out := make([]debrid.DownloadFile, 0, len(files))
	for _, f := range files {
		if minBytes > 0 && f.SizeBytes < minBytes {
			continue
		}
		if maxBytes > 0 && f.SizeBytes > maxBytes {
			continue
		}
		if include != nil && !include.MatchString(f.Path) {
			continue
		}
		if exclude != nil && exclude.MatchString(f.Path) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (im *Importer) processDownload(ctx context.Context, d *database.Download) error {
	fetchCtx, doneFetch, ok := im.tryStartFetch(ctx, d.ID)
	if !ok {
		return nil // already being fetched by another goroutine this same window — not a failure, nothing to do
	}
	defer doneFetch()
	ctx = fetchCtx

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

	p := im.providerFor(d)
	if p == nil {
		// Either nothing is configured for this kind, or what is doesn't
		// belong to this download — providerFor logs which.
		return fmt.Errorf("no provider available for download %s (kind %q, provider %q)", d.ID, d.Kind, d.Provider)
	}

	id := debrid.ProviderDownloadID(d.ProviderDownloadID)
	allFiles, err := p.Files(ctx, id)
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}
	// A provider listing no files at all is not a finished download, it is
	// a download whose file list hasn't materialised yet — a torrent whose
	// metadata is still resolving, or a listing that came back thin. Left
	// unguarded this walked straight through: destination created, nothing
	// fetched, ready_for_import set, and the *arr app then parked forever
	// on "No files found are eligible for import" against an empty folder.
	// Observed exactly that with a real Sonarr grab.
	//
	// Returning an error instead puts it through the ordinary retry
	// backoff, which is right for something expected to resolve on its own
	// shortly.
	//
	// Deliberately distinct from every file being *filtered* out below:
	// that is a real answer about a real file list, and completing is the
	// correct response to "you asked us to skip all of these".
	if len(allFiles) == 0 {
		return fmt.Errorf("provider reported no files for download %s yet", d.ID)
	}
	files := im.filterFiles(allFiles)
	if skipped := len(allFiles) - len(files); skipped > 0 {
		slog.Info("importer: skipped files not matching configured filters", "id", d.ID, "name", d.Name, "skipped", skipped, "kept", len(files))
	}
	if len(files) == 0 {
		slog.Warn("importer: every file was filtered out, nothing to fetch", "id", d.ID, "name", d.Name)
	}

	destDir := im.resolveDestDir(d)
	if err := im.ensureWritableDir(destDir); err != nil {
		return fmt.Errorf("prepare destination directory: %w", err)
	}
	// destDir's own parent (the category folder) isn't something
	// AcerviNode necessarily created itself — best-effort only, not a hard
	// failure: an *arr app that can't remove the (hopefully now-empty)
	// release folder afterward is a real but much smaller problem than the
	// import failing outright, which im.ensureWritableDir(destDir) above
	// already prevents regardless of whether this succeeds.
	if parent := filepath.Dir(destDir); parent != destDir {
		if err := os.Chmod(parent, im.getDirMode()); err != nil {
			slog.Warn("importer: failed to set category directory mode, continuing anyway", "dir", parent, "error", err)
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
		logTickError(ctx, "importer: list downloads eligible for cleanup failed", "error", err)
		return
	}
	for _, d := range rows {
		im.cleanupDownload(ctx, d)
	}
}

// checkStuckDownloads implements the stuck-download watchdog: a download
// still sitting in StateQueued or StateDownloading whose row hasn't actually
// changed — updated_at, which UpdateDownloadStatus/RefreshFromProvider only
// ever bump when state/progress/size/error genuinely changed, never on a
// no-op poll (see RefreshFromProvider's own no-op check) — in at least
// getStuckDownloadTimeout is marked StateError instead of sitting unnoticed
// forever. Deliberately keyed on "no real change reported," not "how long
// it's been running": a large download still genuinely making progress on a
// slow connection must never be punished just for taking a while — only a
// download the provider itself has stopped saying anything new about trips
// this. Applies to both Managed and Manual downloads; unlike
// cleanupOldDownloads' ready_for_import scope, being stuck queued/
// downloading isn't a state that means anything different depending on how
// it was added. Disabled entirely when getStuckDownloadTimeout is 0 (the
// default).
func (im *Importer) checkStuckDownloads(ctx context.Context) {
	timeout := im.getStuckDownloadTimeout()
	if timeout <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-timeout)
	rows, err := im.db.ListStuckDownloads(ctx, cutoff)
	if err != nil {
		logTickError(ctx, "importer: list stuck downloads failed", "error", err)
		return
	}
	for _, d := range rows {
		msg := fmt.Sprintf("no progress for over %s — possibly stuck", timeout)
		if err := im.db.UpdateDownloadStatus(ctx, d.ID, database.StateError, d.Progress, d.SizeBytes, nil, msg); err != nil {
			slog.Error("importer: mark stuck download as error failed", "id", d.ID, "error", err)
			continue
		}
		slog.Warn("importer: marked download as error, no progress for too long", "id", d.ID, "name", d.Name, "timeout", timeout)
	}
}

// cleanupErroredDownloads implements the error-cleanup policy: once a
// download has sat in StateError for at least getCleanupErrorAfterDays
// days, it's removed the same way cleanupOldDownloads removes a finished
// one — local files (if any), the provider-side copy (best-effort), and the
// row itself, via the same cleanupDownload. Unlike cleanupOldDownloads,
// applies to both Managed and Manual downloads: an error here already means
// AcerviNode gave up (retry-exhausted) or the provider genuinely lost track
// of it (a vanished Manual download) — either way a real dead end nobody's
// acted on, not an in-progress state that needs preserving the way
// ready_for_import's Managed-only scope does. Disabled entirely when
// getCleanupErrorAfterDays is 0 (the default).
func (im *Importer) cleanupErroredDownloads(ctx context.Context) {
	days := im.getCleanupErrorAfterDays()
	if days <= 0 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	rows, err := im.db.ListErroredDownloadsEligibleForCleanup(ctx, cutoff)
	if err != nil {
		slog.Error("importer: list errored downloads eligible for cleanup failed", "error", err)
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

	// Whether the provider actually removed its own copy decides the
	// tombstone's lifetime: a failed delete leaves the item on the account,
	// where discovery would re-adopt it as a ghost once a short window
	// lapsed — see database.RecordDeletedDownload.
	providerConfirmed := true
	if p := im.providerFor(d); p != nil {
		if err := p.Delete(ctx, debrid.ProviderDownloadID(d.ProviderDownloadID), true); err != nil {
			providerConfirmed = false
			slog.Warn("importer: cleanup best-effort provider delete failed", "id", d.ID, "error", err)
		}
	}

	if err := im.db.RecordDeletedDownload(ctx, d.Provider, d.Kind, d.ProviderDownloadID, providerConfirmed); err != nil {
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

// idleTimeoutReader wraps an io.Reader (a response body), calling cancel if
// no bytes are read for longer than timeout — an idle/stall timeout, not a
// total-transfer deadline: a large file that's steadily, actively
// transferring never trips this however long the whole download takes;
// only a connection that's actually gone quiet does. Every successful Read
// resets the clock. See fetchFile, the only caller — it also resets the
// same timer right after response headers arrive (before any Read call
// happens at all), so a server that accepts the connection but never sends
// a response is caught the same way a mid-transfer stall is, not left
// waiting forever.
type idleTimeoutReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
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
//
// The actual mode is live-configurable (config.Config.DownloadDirMode, see
// SetDirMode) — 0777 is only the default, not hardcoded — for a user who'd
// rather tighten this back down (e.g. AcerviNode's own systemd
// User=/Group= already matches their *arr stack).
func (im *Importer) ensureWritableDir(dir string) error {
	mode := im.getDirMode()
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set directory mode: %w", err)
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
	destPath, err := safeJoin(destDir, trimRedundantTopDir(f.Path, filepath.Base(destDir)))
	if err != nil {
		return err
	}

	if info, err := os.Stat(destPath); err == nil && !info.IsDir() && info.Size() == f.SizeBytes {
		return nil // already fetched
	}

	if err := im.ensureWritableDir(filepath.Dir(destPath)); err != nil {
		return err
	}

	link, err := p.RequestDownloadLink(ctx, id, f.ProviderFileID)
	if err != nil {
		return fmt.Errorf("resolve download link: %w", err)
	}

	// An idle/stall deadline, not a total-transfer one — see
	// idleTimeoutReader's own doc comment for why: a per-request context
	// (rather than the client's own Timeout field) since fetchTimeout can
	// change live (SetFetchTimeout) and the client itself is built once at
	// construction. The timer starts now, covering the connect-and-wait-for-
	// headers phase too (a server that accepts the connection but never
	// responds at all is exactly as "stalled" as one that stops mid-body),
	// and gets reset once more the moment headers actually arrive, before
	// any Read of the body has happened — so a real response starts the
	// clock fresh for the body-reading phase that follows.
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	idleTimeout := im.getFetchTimeout()
	timer := time.AfterFunc(idleTimeout, cancel)
	defer timer.Stop()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, link, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := im.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	timer.Reset(idleTimeout)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: unexpected status %d", resp.StatusCode)
	}
	body := &idleTimeoutReader{r: resp.Body, timer: timer, timeout: idleTimeout}

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
	if _, err := io.Copy(pw, body); err != nil {
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

// trimRedundantTopDir drops a leading directory from a provider-supplied
// file path when it already matches the directory the files are going into.
//
// resolveDestDir ends the destination with the download's own name, and some
// providers report each file with that same folder in front of it — TorBox's
// file "name" is "Big Buck Bunny/Big Buck Bunny.mp4" while its "short_name"
// is just the file. Joining the two applied the folder twice, so a release
// landed at <category>/<name>/<name>/file rather than the <category>/<name>/file
// real qBittorrent produces. Sonarr scans content_path recursively so imports
// still worked, but the layout was wrong, differed between providers for the
// same torrent, and is the kind of thing that quietly breaks a hardlink or a
// manual tidy-up later.
//
// Only the exact duplicate is removed. A torrent whose top-level folder is
// genuinely different from the download name keeps it, and a flat file list
// is untouched — so this normalises the doubled case without flattening any
// structure a release actually has.
func trimRedundantTopDir(relPath, destName string) string {
	if destName == "" || relPath == "" {
		return relPath
	}
	cleaned := strings.TrimPrefix(filepath.ToSlash(relPath), "./")
	first, rest, found := strings.Cut(cleaned, "/")
	if !found || rest == "" || first != destName {
		return relPath
	}
	return rest
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
