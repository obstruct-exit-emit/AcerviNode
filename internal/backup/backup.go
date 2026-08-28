// Package backup keeps periodic snapshots of AcerviNode's database.
//
// Everything AcerviNode knows lives in one SQLite file: configuration, the
// download history, categories, login accounts and sessions. Losing it loses
// all of that, and until now nothing copied it anywhere — the one gap where
// doing nothing had a cost rather than merely lacking a feature.
package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// snapshotter is the database's own consistent-snapshot call — see
// database.DB.BackupTo. An interface so this package doesn't depend on the
// whole DB, and so tests can drive it without SQLite.
type snapshotter interface {
	BackupTo(ctx context.Context, path string) error
}

// filePrefix and fileSuffix bracket every file this package writes. Pruning
// only ever considers files matching both, so anything else in the backup
// directory — including a snapshot someone renamed to keep — is left alone.
const (
	filePrefix = "acervinode-"
	fileSuffix = ".db"
	// timeLayout sorts lexicographically in time order, which is what lets
	// pruning work on sorted names rather than reading timestamps back off
	// the filesystem (where they can be rewritten by a copy or a restore).
	timeLayout = "20060102-150405"
)

// Runner takes a snapshot on an interval and prunes old ones.
type Runner struct {
	db  snapshotter
	dir string

	mu       sync.Mutex
	// configPath is the config file copied alongside each snapshot. It, not
	// the database, holds the provider keys, the API key and every login
	// account, so a snapshot without it restores the download history and
	// leaves you locked out. Empty disables the copy.
	configPath string
	interval   time.Duration
	keep     int

	// changed carries a new interval to Run, so a settings change retunes
	// the ticker instead of waiting out the old one. Buffered 1 and drained
	// on write — the same pattern internal/importer uses.
	changed chan time.Duration
}

// New builds a Runner. interval of 0 disables scheduled backups entirely;
// manual ones still work. keep is how many snapshots to retain.
func New(db snapshotter, dir, configPath string, interval time.Duration, keep int) *Runner {
	return &Runner{
		db:         db,
		dir:        dir,
		configPath: configPath,
		interval: interval,
		keep:     keep,
		changed:  make(chan time.Duration, 1),
	}
}

// SetConfig retunes the schedule live. dir is deliberately not changeable
// here: it is resolved from data_dir at startup, and moving it while
// running would strand existing snapshots somewhere nothing prunes.
func (r *Runner) SetConfig(interval time.Duration, keep int) {
	r.mu.Lock()
	changed := interval != r.interval
	r.interval, r.keep = interval, keep
	r.mu.Unlock()

	if !changed {
		return
	}
	select {
	case r.changed <- interval:
	default:
		select {
		case <-r.changed:
		default:
		}
		select {
		case r.changed <- interval:
		default:
		}
	}
}

func (r *Runner) config() (time.Duration, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interval, r.keep
}

// Run takes a snapshot every interval until ctx is cancelled.
//
// The schedule is timed from the last snapshot on disk, not from process
// start -- see initialDelay for why that distinction turned out to matter.
func (r *Runner) Run(ctx context.Context) {
	interval, _ := r.config()
	timer := newTimer(r.initialDelay(interval))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case d := <-r.changed:
			// Also timed from the last snapshot: shortening the interval to
			// something already elapsed should back up now, not wait out the
			// new interval as well.
			timer.Reset(r.initialDelay(d))
		case <-timer.C():
			if _, err := r.RunOnce(ctx); err != nil {
				// Logged, not fatal: a failed backup is worth knowing about
				// but is no reason to stop trying on the next tick, and
				// certainly no reason to bring anything else down.
				slog.Error("backup: snapshot failed", "error", err)
			}
			cur, _ := r.config()
			timer.Reset(cur)
		}
	}
}

// dueNow is the delay used when a snapshot is already overdue.
//
// Not zero, because newTimer reads a non-positive duration as "disabled" --
// the one meaning that must never be confused with "immediately". A second
// also keeps the first snapshot from racing the rest of startup.
const dueNow = time.Second

// initialDelay is how long to wait before the next snapshot: whatever remains
// of the interval since the newest one already on disk.
//
// This is what makes the schedule survive restarts, and it replaces a
// deliberate choice that turned out to be wrong in practice. Run used to wait
// a full interval from process start and never back up at startup, so that a
// restart loop could not fill the directory with snapshots of a database
// nobody had touched. The unintended consequence: an instance restarted more
// often than the interval never backed up *at all*. Three days of drift on the
// development box before anyone noticed, across a period when every setting in
// the database had changed.
//
// Timing from the last snapshot keeps the original guarantee intact for the
// case it was written for. A restart loop still cannot produce more than one
// snapshot per interval, because every restart sees the one the previous
// attempt just wrote and waits out the remainder.
func (r *Runner) initialDelay(interval time.Duration) time.Duration {
	// Disabled stays disabled; newTimer stops on any non-positive duration.
	if interval <= 0 {
		return interval
	}
	snapshots, err := r.List()
	if err != nil || len(snapshots) == 0 {
		// Nothing to time from, so there is nothing to protect either.
		return dueNow
	}
	remaining := interval - time.Since(snapshots[0].TakenAt)
	switch {
	case remaining < dueNow:
		return dueNow
	case remaining > interval:
		// A snapshot stamped in the future, from a clock that has since been
		// corrected. Wait at most one interval rather than trusting it.
		return interval
	default:
		return remaining
	}
}

// RunOnce takes one snapshot now and prunes, returning the file written.
// Used by the scheduler and by the settings API's manual trigger, so both
// produce identical results.
func (r *Runner) RunOnce(ctx context.Context) (string, error) {
	name := filePrefix + time.Now().UTC().Format(timeLayout) + fileSuffix
	path := filepath.Join(r.dir, name)

	if err := r.db.BackupTo(ctx, path); err != nil {
		return "", err
	}
	// VACUUM INTO creates the file under the process umask, which leaves it
	// world-readable on a normal install. A snapshot carries the whole
	// download history, and the config beside it carries credentials, so
	// neither has any business being readable by anyone else.
	if err := os.Chmod(path, snapshotMode); err != nil {
		slog.Warn("backup: could not restrict snapshot permissions", "path", path, "error", err)
	}
	if err := r.copyConfig(name); err != nil {
		// Logged, not fatal, and no reason to discard the database snapshot
		// that just succeeded: half a backup beats none.
		slog.Error("backup: config copy failed", "error", err)
	}
	slog.Info("backup: snapshot written", "path", path)

	// Pruned after a successful write, never before: trimming first would
	// mean a failed backup had also thrown away a good one.
	if err := r.prune(); err != nil {
		slog.Error("backup: prune failed", "error", err)
	}
	return path, nil
}

// snapshotMode keeps a snapshot readable only by the user that wrote it.
const snapshotMode = 0o600

// configSuffix names the config copy sitting beside a snapshot. It shares
// the snapshot timestamp, so the pair is obvious and prunes together.
const configSuffix = ".yaml"

// copyConfig writes the config file alongside the snapshot of the same
// moment.
//
// A separate file rather than an archive: it keeps VACUUM INTO's
// consistent-snapshot property for the database, leaves both halves
// directly usable without unpacking anything, and makes a restore two
// copies rather than a tool.
func (r *Runner) copyConfig(snapshotName string) error {
	if r.configPath == "" {
		return nil
	}
	data, err := os.ReadFile(r.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Running entirely from environment variables is legitimate.
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}
	dest := filepath.Join(r.dir, strings.TrimSuffix(snapshotName, fileSuffix)+configSuffix)
	if err := os.WriteFile(dest, data, snapshotMode); err != nil {
		return fmt.Errorf("write config copy: %w", err)
	}
	return nil
}

// Snapshot is one backup file on disk.
type Snapshot struct {
	Name      string
	Path      string
	SizeBytes int64
	TakenAt   time.Time
}

// List returns every snapshot in the backup directory, newest first.
func (r *Runner) List() ([]Snapshot, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing taken yet is not an error — the directory is created
			// by the first backup, not up front.
			return nil, nil
		}
		return nil, fmt.Errorf("read backup dir: %w", err)
	}

	var out []Snapshot
	for _, e := range entries {
		if e.IsDir() || !isSnapshotName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Snapshot{
			Name:      e.Name(),
			Path:      filepath.Join(r.dir, e.Name()),
			SizeBytes: info.Size(),
			TakenAt:   takenAt(e.Name(), info.ModTime()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// prune deletes the oldest snapshots beyond keep. A keep of 0 or less
// retains everything, so a misconfigured value can never wipe the lot.
func (r *Runner) prune() error {
	_, keep := r.config()
	if keep <= 0 {
		return nil
	}
	snaps, err := r.List()
	if err != nil {
		return err
	}
	for _, s := range snaps[min(keep, len(snaps)):] {
		// The config copy shares this snapshot's stamp and goes with it.
		cfgCopy := strings.TrimSuffix(s.Path, fileSuffix) + configSuffix
		if err := os.Remove(cfgCopy); err != nil && !os.IsNotExist(err) {
			slog.Warn("backup: could not remove config copy", "path", cfgCopy, "error", err)
		}
		if err := os.Remove(s.Path); err != nil {
			return fmt.Errorf("remove old backup %s: %w", s.Name, err)
		}
		slog.Info("backup: pruned old snapshot", "path", s.Path)
	}
	return nil
}

// isSnapshotName reports whether a filename is one of ours. Anything else
// in the directory is left strictly alone — including a snapshot someone
// renamed deliberately to stop it being pruned.
func isSnapshotName(name string) bool {
	return strings.HasPrefix(name, filePrefix) && strings.HasSuffix(name, fileSuffix)
}

// takenAt reads the timestamp back out of the filename, falling back to the
// file's own mtime. The name is preferred because copying or restoring a
// backup rewrites mtime, while the name still says when it was taken.
func takenAt(name string, modTime time.Time) time.Time {
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	if t, err := time.Parse(timeLayout, stamp); err == nil {
		return t.UTC()
	}
	return modTime.UTC()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// timer wraps time.Timer so a zero or negative interval means "never fire"
// rather than panicking, which is how backups are disabled.
type timer struct{ t *time.Timer }

func newTimer(d time.Duration) *timer {
	if d <= 0 {
		// Stopped: only a SetConfig with a positive interval starts it.
		t := time.NewTimer(time.Hour)
		t.Stop()
		return &timer{t: t}
	}
	return &timer{t: time.NewTimer(d)}
}

func (t *timer) C() <-chan time.Time { return t.t.C }

func (t *timer) Reset(d time.Duration) {
	t.t.Stop()
	select {
	case <-t.t.C:
	default:
	}
	if d > 0 {
		t.t.Reset(d)
	}
}

func (t *timer) Stop() { t.t.Stop() }
