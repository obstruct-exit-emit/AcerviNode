package api

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Roles a login account can have. RoleAdmin can do everything — Settings,
// user management, and both the Managed and Manual tabs. RoleMember is
// scoped to Manual downloads only: adding/viewing/managing a magnet/NZB/
// hoster link grabbed directly, never the *arr-driven Managed pipeline
// (interfering with something Sonarr/Radarr is actively tracking is a
// bigger deal than a member managing their own manual grabs) or Settings.
// Defined here rather than imported from internal/config, matching how
// GeneralInfo/GeneralUpdate are also defined in this package rather than
// imported — internal/api depends on config only through the Settings
// interface, never directly (see docs/providers.md#roles).
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// UserAccount is one login account, as reported to the settings UI — never
// includes a password hash (see Settings.ListUsers).
type UserAccount struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Default  bool   `json:"default"`
}

// requireAuth checks the Authorization: Bearer <api_key> header (unchanged
// from before login existed) OR a valid session cookie — either
// authenticates. Matches LibriNode's convention: the API key is the
// instance's root-equivalent master credential (what Sonarr/Radarr/scripts
// always use, since they can't do cookie logins), a session is a signed-in
// human account.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentRole(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// requireAdmin is requireAuth plus a role check — Settings and user
// management are admin-only; a member (or the API key, which always
// resolves to admin — see currentRole) still needs the same valid identity
// requireAuth checks, just held to a higher bar.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := s.currentRole(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if role != RoleAdmin {
			http.Error(w, "admin access required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// apiKeyMatches reports whether the request carries the current API key
// (header only — unlike the compat shims, the native API never accepted a
// query-param key).
func (s *Server) apiKeyMatches(r *http.Request) bool {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.settings.APIKey())) == 1
}

// currentUser resolves who's making the request: the API key always
// reports as an anonymous admin (it has no one username — Sonarr/Radarr/
// scripts use it), a valid session reports its account's username and
// role. ok is false when neither authenticates.
func (s *Server) currentUser(r *http.Request) (username, role string, ok bool) {
	if s.apiKeyMatches(r) {
		return "", RoleAdmin, true
	}
	if sess, valid := s.sessions.lookup(currentToken(r)); valid {
		return sess.user, sess.role, true
	}
	return "", "", false
}

// currentRole is currentUser without the username, for the (common) case
// where a caller only needs to authorize, not identify.
func (s *Server) currentRole(r *http.Request) (role string, ok bool) {
	_, role, ok = s.currentUser(r)
	return role, ok
}

// --- Password hashing: PBKDF2-SHA256 (stdlib crypto/pbkdf2), format
// "pbkdf2-sha256$<iterations>$<salt hex>$<hash hex>" — matches LibriNode's
// own format exactly, not that anything reads across between the two, just
// consistency within the same author's projects.

const pbkdf2Iterations = 600_000

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func verifyPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- Sessions: in-memory, cookie-based -------------------------------------
//
// Sessions live in memory only — a restart logs everyone out. Consistent
// with this project's own stance on the database itself (acceptable to
// lose; re-discovery recovers what matters) and with LibriNode's identical
// choice for the same reason: simpler than persisting session state
// somewhere durable for a benefit (surviving a restart while signed in)
// nobody's asked for.

const (
	sessionCookie = "acervinode_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type session struct {
	user   string
	role   string
	expiry time.Time
}

type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]session
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: map[string]session{}}
}

func (st *sessionStore) create(user, role string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	token := hex.EncodeToString(b)
	st.mu.Lock()
	// Prune expired sessions here — logins are rare, and an expired token
	// is otherwise only ever removed when presented again.
	now := time.Now()
	for t, sess := range st.tokens {
		if now.After(sess.expiry) {
			delete(st.tokens, t)
		}
	}
	st.tokens[token] = session{user: user, role: role, expiry: now.Add(sessionTTL)}
	st.mu.Unlock()
	return token
}

// lookup returns the session behind a token, if it's present and not
// expired — the single source of truth currentRole builds on.
func (st *sessionStore) lookup(token string) (session, bool) {
	if token == "" {
		return session{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, ok := st.tokens[token]
	if !ok {
		return session{}, false
	}
	if time.Now().After(sess.expiry) {
		delete(st.tokens, token)
		return session{}, false
	}
	return sess, true
}

func (st *sessionStore) revoke(token string) {
	st.mu.Lock()
	delete(st.tokens, token)
	st.mu.Unlock()
}

// revokeUser ends every session belonging to an account, keeping only the
// `except` token (the browser performing the change; pass "" to keep none)
// — used whenever an account's password or role changes, or the account is
// removed, so the change takes effect immediately rather than at the
// session's natural 30-day expiry.
func (st *sessionStore) revokeUser(user, except string) {
	st.mu.Lock()
	for t, sess := range st.tokens {
		if sess.user == user && t != except {
			delete(st.tokens, t)
		}
	}
	st.mu.Unlock()
}

func currentToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// --- Handlers ---------------------------------------------------------------

// handleAuthStatus is unauthenticated: the UI needs it to decide between
// the login page, the (existing) API-key prompt, and going straight in.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"auth_enabled":  s.settings.AuthEnabled(),
		"authenticated": false,
	}
	if sess, ok := s.sessions.lookup(currentToken(r)); ok {
		resp["authenticated"] = true
		resp["username"] = sess.user
		resp["role"] = sess.role
	}
	writeJSON(w, resp)
}

// handleLogin is unauthenticated by nature. Failed attempts are logged and
// slowed down a little, mirroring LibriNode's own handleLogin.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !s.settings.AuthEnabled() {
		http.Error(w, "authentication is not enabled", http.StatusBadRequest)
		return
	}
	hash, role, found := s.settings.FindUser(req.Username)
	if !found || !verifyPassword(hash, req.Password) {
		slog.Warn("api: failed login attempt", "username", req.Username, "remote", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond)
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	s.setSessionCookie(w, s.sessions.create(req.Username, role), int(sessionTTL.Seconds()))
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	s.setSessionCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetupStatus tells the web UI whether to open the first-run wizard
// instead of asking for the API key. Unauthenticated — it must answer
// before any credentials exist.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"needed": s.settings.SetupNeeded()})
}

// handleSetup claims a fresh instance: creates the first (Default, always
// admin) login account and signs this browser in, in one step — no API key
// required. Refused (403) the moment the instance has a login account or a
// configured provider (see Settings.SetupNeeded).
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.settings.SetupNeeded() {
		http.Error(w, "this instance is already set up", http.StatusForbidden)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		http.Error(w, "hashing password: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	if err := s.settings.Setup(ctx, req.Username, hash); err != nil {
		http.Error(w, "saving config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("api: instance claimed via first-run setup", "username", req.Username)
	s.setSessionCookie(w, s.sessions.create(req.Username, RoleAdmin), int(sessionTTL.Seconds()))
	writeJSON(w, map[string]any{"ok": true})
}

// --- User management (Settings → Security, admin-only except self-service
// password changes) ---------------------------------------------------------

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"users": s.settings.ListUsers()})
}

// handleAddUser creates an additional login account. Role defaults to
// member, the safer choice for an account that isn't the instance owner.
func (s *Server) handleAddUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		http.Error(w, "hashing password: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.settings.AddUser(r.Context(), req.Username, hash, req.Role); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	slog.Info("api: user added", "username", req.Username)
	writeJSON(w, map[string]any{"users": s.settings.ListUsers()})
}

// handleRemoveUser deletes a login account; the default user is refused.
func (s *Server) handleRemoveUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if err := s.settings.RemoveUser(r.Context(), username); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A removed account's open sessions end now, not at the next restart.
	s.sessions.revokeUser(username, "")
	slog.Info("api: user removed", "username", username)
	writeJSON(w, map[string]any{"users": s.settings.ListUsers()})
}

// handleSetUserPassword changes one account's password. Admin-only in
// general, but self-service is always allowed: any signed-in account may
// change its own password without needing admin rights — this endpoint
// sits under requireAuth in routes(), not requireAdmin, precisely for that.
func (s *Server) handleSetUserPassword(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	actorUsername, actorRole, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if actorRole != RoleAdmin && !strings.EqualFold(actorUsername, username) {
		http.Error(w, "admin access required to change another user's password", http.StatusForbidden)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		http.Error(w, "hashing password: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.settings.SetUserPassword(r.Context(), username, hash); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// End the account's other sessions; the browser making the change (which
	// may be the same account) keeps its own.
	s.sessions.revokeUser(username, "")
	slog.Info("api: user password changed", "username", username)
	writeJSON(w, map[string]any{"ok": true})
}

// handleMakeDefaultUser promotes an account to the protected default (and
// to admin in the same step — see config.SetDefaultUser).
func (s *Server) handleMakeDefaultUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if err := s.settings.SetDefaultUser(r.Context(), username); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.sessions.revokeUser(username, "")
	slog.Info("api: default user changed", "username", username)
	writeJSON(w, map[string]any{"users": s.settings.ListUsers()})
}

// handleSetUserRole promotes/demotes an account between admin and member.
// Admin-only; the default user can't be demoted (Settings.SetUserRole).
func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := s.settings.SetUserRole(r.Context(), username, req.Role); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The account's sessions were issued under the old role; end all of
	// them, including the caller's own if they targeted their own account —
	// a session's role is cached at login and never re-derived from config,
	// so excepting the current token here (as handleSetUserPassword and
	// handleMakeDefaultUser correctly do, where the cached role doesn't go
	// stale) would let a demoted admin keep using their already-open
	// session's stale admin role indefinitely.
	s.sessions.revokeUser(username, "")
	slog.Info("api: user role changed", "username", username, "role", req.Role)
	writeJSON(w, map[string]any{"users": s.settings.ListUsers()})
}
