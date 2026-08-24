package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
)

// --- Password hashing --------------------------------------------------

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword() error = %v", err)
	}
	if !verifyPassword(hash, "correct horse battery staple") {
		t.Error("verifyPassword() = false for the correct password")
	}
}

func TestVerifyPassword_WrongPasswordFails(t *testing.T) {
	hash, err := hashPassword("the-real-password")
	if err != nil {
		t.Fatalf("hashPassword() error = %v", err)
	}
	if verifyPassword(hash, "not-the-password") {
		t.Error("verifyPassword() = true for the wrong password")
	}
}

func TestVerifyPassword_MalformedHashRejectedNotPanicked(t *testing.T) {
	cases := []string{"", "not-a-hash-at-all", "pbkdf2-sha256$notanumber$aa$bb", "pbkdf2-sha256$100$zz$bb"}
	for _, stored := range cases {
		if verifyPassword(stored, "anything") {
			t.Errorf("verifyPassword(%q) = true, want false", stored)
		}
	}
}

// --- Session store -------------------------------------------------------

func TestSessionStore_CreateAndLookup(t *testing.T) {
	st := newSessionStore()
	token := st.create("alice", RoleAdmin)
	sess, ok := st.lookup(token)
	if !ok {
		t.Fatal("lookup() ok = false for a freshly created token")
	}
	if sess.user != "alice" || sess.role != RoleAdmin {
		t.Errorf("session = %+v, want user=alice role=admin", sess)
	}
}

func TestSessionStore_LookupUnknownOrEmptyToken(t *testing.T) {
	st := newSessionStore()
	if _, ok := st.lookup("unknown-token"); ok {
		t.Error("lookup() of an unknown token should fail")
	}
	if _, ok := st.lookup(""); ok {
		t.Error("lookup() of an empty token should fail")
	}
}

func TestSessionStore_RevokeEndsSession(t *testing.T) {
	st := newSessionStore()
	token := st.create("alice", RoleAdmin)
	st.revoke(token)
	if _, ok := st.lookup(token); ok {
		t.Error("session should be gone after revoke()")
	}
}

func TestSessionStore_RevokeUserEndsAllExceptGiven(t *testing.T) {
	st := newSessionStore()
	keep := st.create("alice", RoleAdmin)
	drop := st.create("alice", RoleAdmin)
	other := st.create("bob", RoleMember)

	st.revokeUser("alice", keep)

	if _, ok := st.lookup(keep); !ok {
		t.Error("the excepted token should survive revokeUser")
	}
	if _, ok := st.lookup(drop); ok {
		t.Error("alice's other token should be revoked")
	}
	if _, ok := st.lookup(other); !ok {
		t.Error("bob's session should be untouched by revoking alice")
	}
}

func TestSessionStore_ExpiredSessionIsNotValid(t *testing.T) {
	st := newSessionStore()
	token := st.create("alice", RoleAdmin)
	// White-box: same package, so the unexported session's expiry can be
	// backdated directly rather than waiting out sessionTTL in a test.
	st.mu.Lock()
	sess := st.tokens[token]
	sess.expiry = time.Now().Add(-time.Minute)
	st.tokens[token] = sess
	st.mu.Unlock()

	if _, ok := st.lookup(token); ok {
		t.Error("an expired session should not be valid")
	}
}

// --- requireAuth / requireAdmin, exercised through real endpoints --------

func TestRequireAuth_SessionCookieWorks(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
	token := srv.sessions.create("alice", RoleMember)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (valid session should authenticate)", rec.Code)
	}
}

func TestRequireAuth_NeitherApiKeyNorSessionRejected(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAdmin_MemberSessionForbidden(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
	token := srv.sessions.create("bob", RoleMember)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/general", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (member must not reach Settings)", rec.Code)
	}
}

func TestRequireAdmin_AdminSessionAllowed(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
	token := srv.sessions.create("alice", RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/general", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (admin session should reach Settings)", rec.Code)
	}
}

func TestRequireAdmin_ApiKeyAlwaysAdmin(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/general"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (API key is root-equivalent admin)", rec.Code)
	}
}

// --- auth status / setup / login / logout ---------------------------------

func TestHandleAuthStatus_NotAuthenticated(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	json.NewDecoder(rec.Body).Decode(&got)
	if got["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", got["authenticated"])
	}
}

func TestHandleAuthStatus_AuthenticatedReportsUsernameAndRole(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)
	settings.users = []UserAccount{{Username: "alice", Role: RoleAdmin, Default: true}}
	token := srv.sessions.create("alice", RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var got map[string]any
	json.NewDecoder(rec.Body).Decode(&got)
	if got["authenticated"] != true || got["username"] != "alice" || got["role"] != RoleAdmin {
		t.Errorf("auth status = %+v", got)
	}
	if got["auth_enabled"] != true {
		t.Errorf("auth_enabled = %v, want true (a user exists)", got["auth_enabled"])
	}
}

func TestHandleSetupStatus_ReflectsSettings(t *testing.T) {
	settings := &fakeSettings{setupNeeded: true}
	srv, _ := newTestServer(t, nil, nil, settings)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil))
	var got map[string]bool
	json.NewDecoder(rec.Body).Decode(&got)
	if !got["needed"] {
		t.Error("needed = false, want true")
	}
}

func TestHandleSetup_CreatesAdminAndSignsIn(t *testing.T) {
	settings := &fakeSettings{setupNeeded: true}
	srv, _ := newTestServer(t, nil, nil, settings)

	body := strings.NewReader(`{"username":"alice","password":"correcthorse"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", body)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(settings.users) != 1 || settings.users[0].Username != "alice" || !settings.users[0].Default {
		t.Errorf("users = %+v, want one Default alice", settings.users)
	}
	// Should have set a session cookie signing the browser in immediately.
	resp := rec.Result()
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected a session cookie to be set after setup")
	}
}

func TestHandleSetup_RefusedWhenNotNeeded(t *testing.T) {
	settings := &fakeSettings{setupNeeded: false}
	srv, _ := newTestServer(t, nil, nil, settings)
	body := strings.NewReader(`{"username":"alice","password":"correcthorse"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/setup", body))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleSetup_RejectsShortPassword(t *testing.T) {
	settings := &fakeSettings{setupNeeded: true}
	srv, _ := newTestServer(t, nil, nil, settings)
	body := strings.NewReader(`{"username":"alice","password":"short"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/setup", body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleLogin_Success(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)
	hash, _ := hashPassword("correcthorse")
	settings.users = []UserAccount{{Username: "alice", Role: RoleAdmin}}
	settings.userHashes = map[string]string{"alice": hash}

	body := strings.NewReader(`{"username":"alice","password":"correcthorse"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	resp := rec.Result()
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected a session cookie after a successful login")
	}
}

func TestHandleLogin_WrongPassword(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)
	hash, _ := hashPassword("correcthorse")
	settings.users = []UserAccount{{Username: "alice", Role: RoleAdmin}}
	settings.userHashes = map[string]string{"alice": hash}

	body := strings.NewReader(`{"username":"alice","password":"wrong"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleLogin_AuthNotEnabled(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
	body := strings.NewReader(`{"username":"alice","password":"whatever1"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (no users configured)", rec.Code)
	}
}

func TestHandleLogout_ClearsCookie(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
	token := srv.sessions.create("alice", RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, ok := srv.sessions.lookup(token); ok {
		t.Error("session should be revoked after logout")
	}
}

// --- user management -------------------------------------------------------

func TestHandleListUsers(t *testing.T) {
	settings := &fakeSettings{users: []UserAccount{{Username: "alice", Role: RoleAdmin, Default: true}}}
	srv, _ := newTestServer(t, nil, nil, settings)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/users"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "alice") {
		t.Errorf("body = %s, want it to mention alice", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Error("response must never mention a password/hash field")
	}
}

func TestHandleAddUser_Success(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)
	body := strings.NewReader(`{"username":"bob","password":"longenough1","role":"member"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/users", body)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(settings.users) != 1 || settings.users[0].Username != "bob" {
		t.Errorf("users = %+v", settings.users)
	}
}

func TestHandleAddUser_ShortPasswordRejected(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)
	body := strings.NewReader(`{"username":"bob","password":"short","role":"member"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/users", body)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRemoveUser_SurfacesSettingsError(t *testing.T) {
	settings := &fakeSettings{
		users:         []UserAccount{{Username: "alice", Role: RoleAdmin, Default: true}},
		removeUserErr: context.DeadlineExceeded,
	}
	srv, _ := newTestServer(t, nil, nil, settings)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/settings/users/alice"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (default-account removal refused by Settings)", rec.Code)
	}
}

func TestHandleSetUserPassword_SelfServiceAllowedWithoutAdmin(t *testing.T) {
	settings := &fakeSettings{users: []UserAccount{{Username: "bob", Role: RoleMember}}}
	srv, _ := newTestServer(t, nil, nil, settings)
	token := srv.sessions.create("bob", RoleMember)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/users/bob/password", strings.NewReader(`{"password":"newpassword1"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (a member can change their own password)", rec.Code)
	}
}

func TestHandleSetUserPassword_OtherUserRequiresAdmin(t *testing.T) {
	settings := &fakeSettings{users: []UserAccount{{Username: "bob", Role: RoleMember}, {Username: "carol", Role: RoleMember}}}
	srv, _ := newTestServer(t, nil, nil, settings)
	token := srv.sessions.create("bob", RoleMember)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/users/carol/password", strings.NewReader(`{"password":"newpassword1"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (a member can't change someone else's password)", rec.Code)
	}
}

// TestHandleSetUserRole_RevokesActorsOwnSessionOnSelfDemotion guards against
// a real bug found by code inspection: a session's role is cached at login
// and never re-derived from config on later requests (see currentUser).
// handleSetUserRole used to except the caller's own session token from
// revocation the same way handleSetUserPassword/handleMakeDefaultUser
// correctly do — but unlike those two, a role change genuinely invalidates
// what's cached in an already-open session. Excepting it there let an admin
// who demoted their own (non-default) account keep full admin access
// through that same still-open browser session indefinitely, contradicting
// the handler's own stated intent ("end them so a demoted member can't keep
// using an admin session it already holds").
func TestHandleSetUserRole_RevokesActorsOwnSessionOnSelfDemotion(t *testing.T) {
	settings := &fakeSettings{users: []UserAccount{{Username: "alice", Role: RoleAdmin}}}
	srv, _ := newTestServer(t, nil, nil, settings)
	token := srv.sessions.create("alice", RoleAdmin)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/users/alice/role", strings.NewReader(`{"role":"member"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// The exact same session token, reused right after the self-demotion it
	// just performed, must no longer authenticate at all — not fall back to
	// member, fully revoked — since its cached role is now stale.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/settings/general", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("status after self-demotion, reusing the same session = %d, want 401 (stale admin session must be revoked)", rec2.Code)
	}
}

// TestHandleSetUserRole_RevokesTargetUsersOtherSessions is the ordinary case
// (an admin demotes a different account) — makes sure the fix above didn't
// change this already-correct behavior.
func TestHandleSetUserRole_RevokesTargetUsersOtherSessions(t *testing.T) {
	settings := &fakeSettings{users: []UserAccount{{Username: "alice", Default: true, Role: RoleAdmin}, {Username: "bob", Role: RoleAdmin}}}
	srv, _ := newTestServer(t, nil, nil, settings)
	bobToken := srv.sessions.create("bob", RoleAdmin)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/users/bob/role", strings.NewReader(`{"role":"member"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/settings/general", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: bobToken})
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("status for bob's other session after being demoted = %d, want 401", rec2.Code)
	}
}

func TestHandleMakeDefaultUser(t *testing.T) {
	settings := &fakeSettings{users: []UserAccount{{Username: "alice", Default: true, Role: RoleAdmin}, {Username: "bob", Role: RoleMember}}}
	srv, _ := newTestServer(t, nil, nil, settings)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/users/bob/default"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, u := range settings.users {
		if u.Username == "bob" && (!u.Default || u.Role != RoleAdmin) {
			t.Errorf("bob = %+v, want default=true role=admin", u)
		}
		if u.Username == "alice" && u.Default {
			t.Error("alice should no longer be default")
		}
	}
}

// --- member row-level enforcement on downloads -----------------------------

func seedDownloadWithAddedVia(t *testing.T, db *database.DB, kind database.Kind, providerDownloadID string, addedVia database.AddedVia) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: "dl-" + providerDownloadID, Provider: "fake", ProviderDownloadID: providerDownloadID,
		Kind: kind, Hash: "hash-" + providerDownloadID, Name: "Test Download",
		State: database.StateDownloading, Progress: 0.5, AddedVia: addedVia,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}
	return d
}

func TestDownloadByID_MemberCanAccessManualDownload(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, nil, nil, settings)
	d := seedDownloadWithAddedVia(t, db, database.KindTorrent, "manual-1", database.AddedViaManual)
	token := srv.sessions.create("bob", RoleMember)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+d.ID, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (member accessing a Manual download)", rec.Code)
	}
}

func TestDownloadByID_MemberForbiddenFromManagedDownload(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, nil, nil, settings)
	d := seedDownloadWithAddedVia(t, db, database.KindTorrent, "arr-1", database.AddedViaArr)
	token := srv.sessions.create("bob", RoleMember)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+d.ID, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (member must not reach a Managed download)", rec.Code)
	}
}

func TestDownloadByID_AdminCanAccessEitherKind(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, nil, nil, settings)
	d := seedDownloadWithAddedVia(t, db, database.KindTorrent, "arr-1", database.AddedViaArr)
	token := srv.sessions.create("alice", RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+d.ID, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (admin can access a Managed download)", rec.Code)
	}
}

func TestHandleListDownloads_MemberForcedToManualOnly(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, nil, nil, settings)
	seedDownloadWithAddedVia(t, db, database.KindTorrent, "arr-1", database.AddedViaArr)
	seedDownloadWithAddedVia(t, db, database.KindTorrent, "manual-1", database.AddedViaManual)
	token := srv.sessions.create("bob", RoleMember)

	// Even explicitly asking for added_via=arr, a member only ever gets
	// Manual rows back.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads?added_via=arr", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var got []map[string]any
	json.NewDecoder(rec.Body).Decode(&got)
	if len(got) != 1 || got[0]["added_via"] != "manual" {
		t.Errorf("results = %+v, want exactly the one Manual row regardless of the added_via=arr query", got)
	}
}

func TestHandleListDownloads_AdminSeesRequestedFilter(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, nil, nil, settings)
	seedDownloadWithAddedVia(t, db, database.KindTorrent, "arr-1", database.AddedViaArr)
	seedDownloadWithAddedVia(t, db, database.KindTorrent, "manual-1", database.AddedViaManual)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads?added_via=arr"))
	var got []map[string]any
	json.NewDecoder(rec.Body).Decode(&got)
	if len(got) != 1 || got[0]["added_via"] != "arr" {
		t.Errorf("results = %+v, want exactly the one arr row", got)
	}
}

// TestSetUserPassword_KeepsTheCallersOwnSession proves changing your own
// password doesn't log you out of the browser you changed it in.
//
// The handler passed "" as revokeUser's except-token, so it revoked every
// session for the account including the caller's own — directly contradicting
// its own comment, and the same shape as the self-demote session bug. Caught
// live during burn-in: the request immediately after a self password change
// came back 401.
func TestSetUserPassword_KeepsTheCallersOwnSession(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})

	token := srv.sessions.create("member1", RoleMember)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/users/member1/password",
		strings.NewReader(`{"password":"a-new-password-123"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, ok := srv.sessions.lookup(token); !ok {
		t.Error("the caller's own session was revoked by their own password change")
	}
}

// TestSetUserPassword_RevokesTheTargetsOtherSessions is the other half: an
// admin resetting someone else's password must end that account's sessions,
// since whoever holds them no longer knows the password.
func TestSetUserPassword_RevokesTheTargetsOtherSessions(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{})

	victim := srv.sessions.create("member1", RoleMember)
	admin := srv.sessions.create("admin1", RoleAdmin)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/users/member1/password",
		strings.NewReader(`{"password":"reset-by-an-admin-123"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: admin})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, ok := srv.sessions.lookup(victim); ok {
		t.Error("the target's session survived an admin resetting their password")
	}
	if _, ok := srv.sessions.lookup(admin); !ok {
		t.Error("the admin's own session was revoked by resetting someone else's password")
	}
}

// TestAddUser_RejectsAnUnrecognisedRole proves a bogus role is refused
// rather than silently downgraded. config.AddUser maps anything that isn't
// exactly "admin" to "member" — the right fail-safe for storage, but as an
// API answer it meant `"role":"Admin"` created a member account and returned
// 200. PUT .../role already refused the same input; this brings POST /users
// into line. Found during burn-in.
func TestAddUser_RejectsAnUnrecognisedRole(t *testing.T) {
	for _, role := range []string{"superuser", "Admin", "ADMIN", "administrator", "root"} {
		t.Run(role, func(t *testing.T) {
			srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
			token := srv.sessions.create("admin1", RoleAdmin)
			body := `{"username":"newbie","password":"long-enough-123","role":"` + role + `"}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/users", strings.NewReader(body))
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("role %q: status = %d, want 400 (got %s)", role, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAddUser_AcceptsValidAndOmittedRoles guards the other side: the two
// real roles still work, and omitting role entirely still means member.
func TestAddUser_AcceptsValidAndOmittedRoles(t *testing.T) {
	for _, body := range []string{
		`{"username":"a","password":"long-enough-123","role":"admin"}`,
		`{"username":"b","password":"long-enough-123","role":"member"}`,
		`{"username":"c","password":"long-enough-123"}`,
	} {
		srv, _ := newTestServer(t, nil, nil, &fakeSettings{})
		token := srv.sessions.create("admin1", RoleAdmin)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/users", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("body %s rejected: %s", body, rec.Body.String())
		}
	}
}
