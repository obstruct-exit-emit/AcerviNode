package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Restoring a snapshot is staged, not applied in place.
//
// SQLite will not tolerate its file being swapped underneath an open
// connection, and this process holds exactly one for its whole life. So a
// restore copies the chosen snapshot to a staging path and asks for a restart;
// the next startup notices the staged file and moves it into place *before*
// anything opens the database. The window where the file is being replaced is
// therefore one in which nothing is reading it.
//
// The same reasoning is why there is still no way to restore the config half
// from the browser: it carries the API key and every login, so swapping it
// would sign out the session making the request and change the credential the
// request authenticated with. That one stays a deliberate act at a shell, and
// the snapshot's .yaml is right there for it.
const (
	pendingRestoreName = "restore-pending.db"
	// Whatever the database was before a restore, kept rather than deleted.
	// A restore is the one operation here most likely to be regretted.
	displacedName = "acervinode.db.pre-restore"
)

// stageRestore copies a snapshot into place for the next startup to apply.
func stageRestore(snapshotPath, dataDir string) error {
	return copyFile(snapshotPath, filepath.Join(dataDir, pendingRestoreName))
}

// applyPendingRestore swaps in a staged snapshot, if one is waiting.
//
// Called before the database is opened. Ordering is what makes it safe to
// interrupt: the current database is moved aside first, so a crash between the
// two moves leaves the staged file still staged and the old one recoverable,
// rather than leaving nothing at all where the database should be.
func applyPendingRestore(dataDir string) error {
	staged := filepath.Join(dataDir, pendingRestoreName)
	if _, err := os.Stat(staged); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("check for staged restore: %w", err)
	}

	live := filepath.Join(dataDir, "acervinode.db")
	displaced := filepath.Join(dataDir, displacedName)

	if _, err := os.Stat(live); err == nil {
		if err := os.Rename(live, displaced); err != nil {
			return fmt.Errorf("set the current database aside: %w", err)
		}
	}
	// The -wal and -shm sidecars belong to the database being replaced. Left
	// behind they would be read as this one's, which is a corrupt pairing.
	for _, sidecar := range []string{live + "-wal", live + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			slog.Warn("restore: could not remove stale sidecar", "path", sidecar, "error", err)
		}
	}
	if err := os.Rename(staged, live); err != nil {
		return fmt.Errorf("move the restored database into place: %w", err)
	}

	slog.Info("restore: snapshot applied", "database", live, "previous", displaced)
	return nil
}

// copyFile writes src to dst, replacing it, at the source's own permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
