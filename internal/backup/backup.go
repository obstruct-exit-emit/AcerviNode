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
	interval time.Duration
	keep     int

	// changed carries a new interval to Run, so a settings change retunes
	// the ticker instead of waiting out the old one. Buffered 1 and drained
	// on write — the same pattern internal/importer uses.
	changed chan time.Duration
}

// New builds a Runner. interval of 0 disables scheduled backups entirely;
// manual ones still work. keep is how many snapshots to retain.
func New(db snapshotter, dir string, interval time.Duration, keep int) *Runner {
	return &Runner{
		db:       db,
		dir:      dir,
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
// Deliberately does *not* back up on startup: a restart loop would otherwise
// fill the directory with snapshots of a database nobody had a chance to
// change, and push the useful older ones out of the retention window.
func (r *Runner) Run(ctx context.Context) {
	interval, _ := r.config()
	timer := newTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case d := <-r.changed:
			timer.Reset(d)
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

// RunOnce takes one snapshot now and prunes, returning the file written.
// Used by the scheduler and by the settings API's manual trigger, so both
// produce identical results.
func (r *Runner) RunOnce(ctx context.Context) (string, error) {
	name := filePrefix + time.Now().UTC().Format(timeLayout) + fileSuffix
	path := filepath.Join(r.dir, name)

	if err := r.db.BackupTo(ctx, path); err != nil {
		return "", err
	}
	slog.Info("backup: snapshot written", "path", path)

	// Pruned after a successful write, never before: trimming first would
	// mean a failed backup had also thrown away a good one.
	if err := r.prune(); err != nil {
		slog.Error("backup: prune failed", "error", err)
	}
	return path, nil
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
