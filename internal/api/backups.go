package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/acervinode/acervinode/internal/backup"
)

// handleGetBackups implements GET /api/v1/settings/backups — the database
// snapshots currently on disk, newest first.
//
// Returns names and sizes, never contents. A snapshot's config half holds
// both provider API keys, the AcerviNode API key and every login account's
// password hash, so handing one out over the API would be a far bigger hole
// than the one backups exist to close. Restoring stays a deliberate act —
// stop the service, put the files in place — rather than something reachable
// from a browser.
func (s *Server) handleGetBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.settings.Backups()
	if err != nil {
		http.Error(w, "failed to list backups: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if backups == nil {
		backups = []BackupInfo{}
	}
	writeJSON(w, backups)
}

// handleRunBackup implements POST /api/v1/settings/backups — takes a
// snapshot immediately, in addition to whatever the schedule does. Reports
// only the file's name, for the same reason the listing does.
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	path, err := s.settings.RunBackupNow(r.Context())
	if err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"name": filepath.Base(path)})
}

// handleDeleteBackup implements DELETE /api/v1/settings/backups/{name} —
// removes one snapshot and the config copy beside it.
//
// Deleting is offered where restoring is not, and the asymmetry is the point:
// a snapshot is credential material, so removing one from the browser is a
// tidying job, while putting one back is a decision that deserves a shell.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	err := s.settings.DeleteBackup(r.PathValue("name"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, backup.ErrNotASnapshot):
		// A name this package never wrote. Refused before it reaches the
		// filesystem, so a traversal attempt cannot even be probed for.
		http.Error(w, "not a snapshot name", http.StatusBadRequest)
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, "no such snapshot", http.StatusNotFound)
	default:
		http.Error(w, "failed to delete backup: "+err.Error(), http.StatusInternalServerError)
	}
}

// handleRestoreBackup implements POST /api/v1/settings/backups/{name}/restore.
//
// Returns 202, not 200: the restore has been *staged*, not performed. SQLite
// will not have its file swapped under an open connection, so the snapshot is
// put in place for the next startup and the service restarts itself. By the
// time a client could read this response the process is already going down.
//
// Only the database half is restored. The config half holds the API key and
// every login, so replacing it would sign out the session that asked and
// invalidate the credential the request authenticated with — that one stays a
// deliberate act at a shell, and the snapshot's .yaml sits ready for it.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	err := s.settings.RestoreBackup(r.Context(), r.PathValue("name"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"status": "restarting to apply the restore"})
	case errors.Is(err, backup.ErrNotASnapshot):
		http.Error(w, "not a snapshot name", http.StatusBadRequest)
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, "no such snapshot", http.StatusNotFound)
	default:
		http.Error(w, "failed to restore backup: "+err.Error(), http.StatusInternalServerError)
	}
}
