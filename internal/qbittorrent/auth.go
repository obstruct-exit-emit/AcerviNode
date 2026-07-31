package qbittorrent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "SID"
	sessionTTL        = 24 * time.Hour
)

// sessionStore tracks logged-in sessions in memory — a restart logs everyone
// out. That's fine here: *arr apps re-authenticate on every "Test" and
// transparently re-login if a call comes back 403.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // sid -> expiry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]time.Time{}}
}

func (s *sessionStore) create() string {
	sid := randomToken()
	s.mu.Lock()
	s.sessions[sid] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return sid
}

func (s *sessionStore) valid(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[sid]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, sid)
		return false
	}
	return true
}

func (s *sessionStore) revoke(sid string) {
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
}

func randomToken() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// handleLogin matches qBittorrent's real behavior: any username is accepted,
// only the password matters, and a bad password gets a 200 "Fails." rather
// than a 4xx — *arr apps' "Test" flow checks the response body, not the
// status code.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "Fails.")
		return
	}
	// Constant-time, matching the native API's own auth check (see
	// internal/api/auth.go) — a plain != comparison here would be the one
	// auth entry point in the app not following that convention.
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.settings.APIKey())) != 1 {
		writeText(w, http.StatusOK, "Fails.")
		return
	}

	sid := s.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeText(w, http.StatusOK, "Ok.")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.revoke(cookie.Value)
	}
	writeText(w, http.StatusOK, "")
}

// requireAuth matches qBittorrent's real behavior of returning 403 (not 401)
// for missing/invalid sessions.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.valid(cookie.Value) {
			writeText(w, http.StatusForbidden, "Forbidden")
			return
		}
		next(w, r)
	}
}
