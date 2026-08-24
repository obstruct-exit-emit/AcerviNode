package api

import (
	"net/http"
	"path/filepath"
)

// handleGetBackups implements GET /api/v1/settings/backups — the database
// snapshots currently on disk, newest first.
//
// Returns names and sizes, never contents: a snapshot holds every login
// account and session in the instance, so handing one out over the API
// would be a far bigger hole than the one backups exist to close. Restoring
// is deliberately a deliberate act — stop the service, put the file in
// place — rather than something reachable from a browser.
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
