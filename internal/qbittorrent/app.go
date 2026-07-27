package qbittorrent

import "net/http"

// These are plain, fixed version strings — *arr apps check webapiVersion
// against a minimum they support, and AcerviNode only needs to clear that
// bar, not be a real qBittorrent build.
const (
	fakeAppVersion    = "v4.6.0"
	fakeWebAPIVersion = "2.9.3"
)

func (s *Server) handleAppVersion(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusOK, fakeAppVersion)
}

func (s *Server) handleWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusOK, fakeWebAPIVersion)
}
