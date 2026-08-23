package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// fakeProvider satisfies torrentAdder, usenetAdder, and webDownloadAdder —
// every test that previously only exercised Delete keeps working unchanged
// (the extra methods are simply unused in those cases), and add-download
// tests configure addID/addErr/statusResp/statusErr as needed.
type fakeProvider struct {
	providerName string

	deleteCalled bool
	deleteID     debrid.ProviderDownloadID
	deleteFiles  bool
	deleteErr    error

	addID         debrid.ProviderDownloadID
	addErr        error
	addedMagnet   string
	addedURL      string
	addedLink     string
	addedFilename string
	addedFile     []byte

	statusResp debrid.DownloadStatus
	statusErr  error

	filesResp []debrid.DownloadFile
	filesErr  error

	linkResp      string
	linkErr       error
	linkRequested string // fileID RequestDownloadLink was last called with

	zipLinkResp string
	zipLinkErr  error
	zipLinkID   debrid.ProviderDownloadID // id RequestZipDownloadLink was last called with

	checkCachedResp   map[string]bool
	checkCachedErr    error
	checkCachedHashes []string // hashes CheckCached was last called with
	torrentInfoResp   debrid.TorrentInfo
	torrentInfoErr    error
	torrentInfoHash   string // hash TorrentInfo was last called with
}

func (f *fakeProvider) Name() string {
	if f.providerName == "" {
		return "fake"
	}
	return f.providerName
}

func (f *fakeProvider) Delete(_ context.Context, id debrid.ProviderDownloadID, deleteFiles bool) error {
	f.deleteCalled = true
	f.deleteID = id
	f.deleteFiles = deleteFiles
	return f.deleteErr
}

func (f *fakeProvider) AddMagnet(_ context.Context, magnetURI string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedMagnet = magnetURI
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddTorrentFile(_ context.Context, filename string, data []byte, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedFilename = filename
	f.addedFile = data
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddNZBURL(_ context.Context, link string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedURL = link
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddNZBFile(_ context.Context, filename string, data []byte, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedFilename = filename
	f.addedFile = data
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) AddLink(_ context.Context, link string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	f.addedLink = link
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeProvider) Status(_ context.Context, _ debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	if f.statusErr != nil {
		return debrid.DownloadStatus{}, f.statusErr
	}
	return f.statusResp, nil
}

func (f *fakeProvider) Files(_ context.Context, _ debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.filesResp, nil
}

func (f *fakeProvider) RequestDownloadLink(_ context.Context, _ debrid.ProviderDownloadID, fileID string) (string, error) {
	f.linkRequested = fileID
	if f.linkErr != nil {
		return "", f.linkErr
	}
	return f.linkResp, nil
}

func (f *fakeProvider) RequestZipDownloadLink(_ context.Context, id debrid.ProviderDownloadID) (string, error) {
	f.zipLinkID = id
	if f.zipLinkErr != nil {
		return "", f.zipLinkErr
	}
	return f.zipLinkResp, nil
}

func (f *fakeProvider) CheckCached(_ context.Context, hashes []string) (map[string]bool, error) {
	f.checkCachedHashes = hashes
	if f.checkCachedErr != nil {
		return nil, f.checkCachedErr
	}
	return f.checkCachedResp, nil
}

func (f *fakeProvider) TorrentInfo(_ context.Context, hash string) (debrid.TorrentInfo, error) {
	f.torrentInfoHash = hash
	if f.torrentInfoErr != nil {
		return debrid.TorrentInfo{}, f.torrentInfoErr
	}
	return f.torrentInfoResp, nil
}

type fakeSettings struct {
	configured bool
	setCalls   []string
	setErr     error
	apiKey     string
	regenCalls int
	regenErr   error
	general    GeneralInfo

	testLatencyMs int64
	testErr       error

	updateCalls []GeneralUpdate
	updateErr   error
	restartReq  bool

	torrentCategories []string
	usenetCategories  []string
	addCategoryCalls  []addCategoryRequest
	addCategoryErr    error

	categoryPaths       map[string]string
	setCategoryPathCall *setCategoryPathRequest
	setCategoryPathErr  error
	removeCategoryCall  string
	removeCategoryErr   error

	accountStatus debrid.AccountStatus
	accountErr    error

	statusResult StatusInfo
	statusErr    error

	// Auth: a simple in-memory user list — enough to exercise
	// requireAuth/requireAdmin/downloadByID's role checks and the user-
	// management handlers without needing a real config.Config.
	users          []UserAccount
	userHashes     map[string]string // username -> password hash
	setupNeeded    bool
	setupErr       error
	addUserErr     error
	removeUserErr  error
	setPasswordErr error
	setRoleErr     error
	setDefaultErr  error

	supervisedBySystemd bool
	restartCalls        int
	restartErr          error
	regenCertCalls      int
	regenCertErr        error

	deleteLocalFilesCalls []string // download IDs passed to DeleteLocalFiles, in order
	deleteLocalFilesErr   error
	cancelFetchCalls      []string // download IDs passed to CancelFetch, in order
}

func (f *fakeSettings) AuthEnabled() bool { return len(f.users) > 0 }
func (f *fakeSettings) SetupNeeded() bool { return f.setupNeeded }

func (f *fakeSettings) Setup(_ context.Context, username, passwordHash string) error {
	if f.setupErr != nil {
		return f.setupErr
	}
	f.users = append(f.users, UserAccount{Username: username, Role: RoleAdmin, Default: true})
	if f.userHashes == nil {
		f.userHashes = map[string]string{}
	}
	f.userHashes[username] = passwordHash
	f.setupNeeded = false
	return nil
}

func (f *fakeSettings) FindUser(username string) (passwordHash, role string, found bool) {
	for _, u := range f.users {
		if strings.EqualFold(u.Username, username) {
			return f.userHashes[u.Username], u.Role, true
		}
	}
	return "", "", false
}

func (f *fakeSettings) ListUsers() []UserAccount { return f.users }

func (f *fakeSettings) AddUser(_ context.Context, username, passwordHash, role string) error {
	if f.addUserErr != nil {
		return f.addUserErr
	}
	if role != RoleAdmin {
		role = RoleMember
	}
	f.users = append(f.users, UserAccount{Username: username, Role: role})
	if f.userHashes == nil {
		f.userHashes = map[string]string{}
	}
	f.userHashes[username] = passwordHash
	return nil
}

func (f *fakeSettings) RemoveUser(_ context.Context, username string) error {
	if f.removeUserErr != nil {
		return f.removeUserErr
	}
	for i, u := range f.users {
		if strings.EqualFold(u.Username, username) {
			f.users = append(f.users[:i], f.users[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no user named %q", username)
}

func (f *fakeSettings) SetUserPassword(_ context.Context, username, passwordHash string) error {
	if f.setPasswordErr != nil {
		return f.setPasswordErr
	}
	if f.userHashes == nil {
		f.userHashes = map[string]string{}
	}
	f.userHashes[username] = passwordHash
	return nil
}

func (f *fakeSettings) SetUserRole(_ context.Context, username, role string) error {
	if f.setRoleErr != nil {
		return f.setRoleErr
	}
	for i, u := range f.users {
		if strings.EqualFold(u.Username, username) {
			f.users[i].Role = role
			return nil
		}
	}
	return fmt.Errorf("no user named %q", username)
}

func (f *fakeSettings) SetDefaultUser(_ context.Context, username string) error {
	if f.setDefaultErr != nil {
		return f.setDefaultErr
	}
	for i := range f.users {
		f.users[i].Default = strings.EqualFold(f.users[i].Username, username)
		if f.users[i].Default {
			f.users[i].Role = RoleAdmin
		}
	}
	return nil
}

func (f *fakeSettings) SupervisedBySystemd() bool { return f.supervisedBySystemd }

func (f *fakeSettings) RequestRestart(_ context.Context) error {
	f.restartCalls++
	return f.restartErr
}

func (f *fakeSettings) RegenerateCertificate(_ context.Context) error {
	f.regenCertCalls++
	return f.regenCertErr
}

func (f *fakeSettings) DeleteLocalFiles(d *database.Download) error {
	f.deleteLocalFilesCalls = append(f.deleteLocalFilesCalls, d.ID)
	return f.deleteLocalFilesErr
}

func (f *fakeSettings) CancelFetch(id string) {
	f.cancelFetchCalls = append(f.cancelFetchCalls, id)
}

func (f *fakeSettings) TorBoxConfigured() bool { return f.configured }

func (f *fakeSettings) SetTorBoxAPIKey(_ context.Context, apiKey string) error {
	f.setCalls = append(f.setCalls, apiKey)
	if f.setErr != nil {
		return f.setErr
	}
	f.configured = true
	return nil
}

func (f *fakeSettings) TestTorBoxConnection(_ context.Context) (int64, error) {
	return f.testLatencyMs, f.testErr
}

// APIKey defaults to "secret" (matching authedRequest below) so tests that
// don't care about auth can pass a zero-value *fakeSettings.
func (f *fakeSettings) APIKey() string {
	if f.apiKey == "" {
		return "secret"
	}
	return f.apiKey
}

func (f *fakeSettings) RegenerateAPIKey(_ context.Context) (string, error) {
	f.regenCalls++
	if f.regenErr != nil {
		return "", f.regenErr
	}
	f.apiKey = "regenerated-key"
	return f.apiKey, nil
}

func (f *fakeSettings) General() GeneralInfo { return f.general }

func (f *fakeSettings) UpdateGeneral(_ context.Context, update GeneralUpdate) (bool, error) {
	f.updateCalls = append(f.updateCalls, update)
	if f.updateErr != nil {
		return false, f.updateErr
	}
	f.general = GeneralInfo{
		APIKey: f.general.APIKey, Port: update.Port, DataDir: update.DataDir,
		DownloadDir: update.DownloadDir, LogLevel: update.LogLevel,
		ImportIntervalSeconds: update.ImportIntervalSeconds, ImportMaxRetries: update.ImportMaxRetries,
	}
	return f.restartReq, nil
}

func (f *fakeSettings) Categories() (torrent []string, usenet []string) {
	return f.torrentCategories, f.usenetCategories
}

func (f *fakeSettings) AddCategory(protocol, name string) error {
	f.addCategoryCalls = append(f.addCategoryCalls, addCategoryRequest{Protocol: protocol, Name: name})
	return f.addCategoryErr
}

func (f *fakeSettings) CategoryPaths() map[string]string {
	return f.categoryPaths
}

func (f *fakeSettings) SetCategoryPath(_ context.Context, category, path string) error {
	req := setCategoryPathRequest{Category: category, Path: path}
	f.setCategoryPathCall = &req
	if f.setCategoryPathErr != nil {
		return f.setCategoryPathErr
	}
	if f.categoryPaths == nil {
		f.categoryPaths = map[string]string{}
	}
	if path == "" {
		delete(f.categoryPaths, category)
	} else {
		f.categoryPaths[category] = path
	}
	return nil
}

func (f *fakeSettings) RemoveCategory(_ context.Context, category string) error {
	f.removeCategoryCall = category
	if f.removeCategoryErr != nil {
		return f.removeCategoryErr
	}
	delete(f.categoryPaths, category)
	return nil
}

func (f *fakeSettings) AccountStatus(_ context.Context) (debrid.AccountStatus, error) {
	if f.accountErr != nil {
		return debrid.AccountStatus{}, f.accountErr
	}
	return f.accountStatus, nil
}

func (f *fakeSettings) Status(_ context.Context) (StatusInfo, error) {
	if f.statusErr != nil {
		return StatusInfo{}, f.statusErr
	}
	return f.statusResult, nil
}

func newTestServer(t *testing.T, torrentProvider torrentAdder, usenetProvider usenetAdder, settings Settings) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if settings == nil {
		settings = &fakeSettings{}
	}
	return NewServer("dev", db, torrentProvider, usenetProvider, nil, settings), db
}

// newTestServerWithWebDownload is newTestServer's counterpart for exercising
// the Web Downloads endpoints specifically — kept separate rather than
// adding a fourth parameter to newTestServer, which every one of its ~75
// existing call sites would otherwise need to pass nil for.
func newTestServerWithWebDownload(t *testing.T, webDownloadProvider webDownloadAdder, settings Settings) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if settings == nil {
		settings = &fakeSettings{}
	}
	return NewServer("dev", db, nil, nil, webDownloadProvider, settings), db
}

func authedRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func TestHandleHealth_NoAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleVersion_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/version"))
	if rec.Code != http.StatusOK {
		t.Errorf("correct key: status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("version response body is empty")
	}
}

func TestHandleProviders_ReturnsConfigured(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{configured: true})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "torbox") {
		t.Errorf("body = %q, want it to mention torbox", got)
	}
}

// TestHandleProviders_UnconfiguredReturnsEmptyArray guards against a real
// bug found during manual testing: a nil slice marshals to JSON `null`,
// which the embedded UI's `providers.length` check would throw on.
func TestHandleProviders_UnconfiguredReturnsEmptyArray(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{configured: false})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want [] (not null)", got)
	}
}

func TestHandleGetProviderSettings(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, &fakeSettings{configured: true})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/providers"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]providerSettingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got["torbox"].Configured {
		t.Errorf("got = %+v, want torbox.configured = true", got)
	}
	// The actual key must never appear in the response body.
	if strings.Contains(rec.Body.String(), "api_key") {
		t.Errorf("response leaked a field named api_key: %s", rec.Body.String())
	}
}

func TestHandleSetTorBoxAPIKey(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/providers/torbox", strings.NewReader(`{"api_key":"new-torbox-key"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if len(settings.setCalls) != 1 || settings.setCalls[0] != "new-torbox-key" {
		t.Errorf("SetTorBoxAPIKey calls = %v, want one call with new-torbox-key", settings.setCalls)
	}
}

func TestHandleSetTorBoxAPIKey_RejectsEmptyKey(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/providers/torbox", strings.NewReader(`{"api_key":""}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if len(settings.setCalls) != 0 {
		t.Errorf("SetTorBoxAPIKey should not have been called for an empty key")
	}
}

func TestHandleSetTorBoxAPIKey_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/providers/torbox", strings.NewReader(`{"api_key":"x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetGeneralSettings(t *testing.T) {
	settings := &fakeSettings{general: GeneralInfo{
		APIKey: "secret", Port: 7846, DataDir: "./data", DownloadDir: "./downloads",
		LogLevel: "info", ImportIntervalSeconds: 10, ImportMaxRetries: 5,
	}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/general"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got GeneralInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != settings.general {
		t.Errorf("got = %+v, want %+v", got, settings.general)
	}
}

func TestHandleGetGeneralSettings_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings/general", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRegenerateAPIKey(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/api-key/regenerate"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if settings.regenCalls != 1 {
		t.Errorf("RegenerateAPIKey calls = %d, want 1", settings.regenCalls)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["api_key"] != "regenerated-key" {
		t.Errorf("api_key = %q, want regenerated-key", got["api_key"])
	}
}

func TestHandleRegenerateAPIKey_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/api-key/regenerate", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRestartServer(t *testing.T) {
	settings := &fakeSettings{supervisedBySystemd: true}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/system/restart"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if settings.restartCalls != 1 {
		t.Errorf("RequestRestart calls = %d, want 1", settings.restartCalls)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["restarting"] != true {
		t.Errorf("restarting = %v, want true", got["restarting"])
	}
	if got["supervised"] != true {
		t.Errorf("supervised = %v, want true", got["supervised"])
	}
}

// TestHandleRestartServer_ReportsUnsupervised proves the response tells the
// caller the truth when nothing will actually bring the process back — see
// Settings.SupervisedBySystemd's own doc comment on why this matters.
func TestHandleRestartServer_ReportsUnsupervised(t *testing.T) {
	settings := &fakeSettings{supervisedBySystemd: false}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/system/restart"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["supervised"] != false {
		t.Errorf("supervised = %v, want false", got["supervised"])
	}
}

func TestHandleRestartServer_ErrorPropagates(t *testing.T) {
	settings := &fakeSettings{restartErr: fmt.Errorf("no trigger wired")}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/system/restart"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRestartServer_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/system/restart", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRestartServer_MemberForbidden(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)
	token := srv.sessions.create("bob", RoleMember)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/system/restart", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (member must not be able to restart)", rec.Code)
	}
	if settings.restartCalls != 0 {
		t.Error("RequestRestart was called despite the member being forbidden")
	}
}

func TestHandleRegenerateCertificate(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/tls/regenerate"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if settings.regenCertCalls != 1 {
		t.Errorf("RegenerateCertificate calls = %d, want 1", settings.regenCertCalls)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["restart_required"] != true {
		t.Errorf("restart_required = %v, want true", got["restart_required"])
	}
}

func TestHandleRegenerateCertificate_ErrorPropagates(t *testing.T) {
	settings := &fakeSettings{regenCertErr: fmt.Errorf("a custom tls_cert_file/tls_key_file is configured")}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/tls/regenerate"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRegenerateCertificate_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/tls/regenerate", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleTestTorBoxConnection_Success(t *testing.T) {
	settings := &fakeSettings{testLatencyMs: 123}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/providers/torbox/test"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if got["latency_ms"] != float64(123) {
		t.Errorf("latency_ms = %v, want 123", got["latency_ms"])
	}
}

func TestHandleTestTorBoxConnection_Failure(t *testing.T) {
	settings := &fakeSettings{testErr: errors.New("connection test failed: torbox: not configured")}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/settings/providers/torbox/test"))
	// A failed *connection test* is still a successful API call (200) — the
	// failure is reported in the body, matching handleTestTorBoxConnection's
	// "ok": false shape rather than an HTTP error status.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if got["error"] == nil || got["error"] == "" {
		t.Error("error message missing from failed connection test response")
	}
}

func TestHandleTestTorBoxConnection_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/settings/providers/torbox/test", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleUpdateGeneralSettings(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	body := `{"port":7846,"data_dir":"./data","download_dir":"./new-downloads","log_level":"debug","import_interval_seconds":15,"import_max_retries":3,"min_fetch_file_size_bytes":2048,"max_fetch_file_size_bytes":104857600,"include_file_regex":"\\.mkv$","exclude_file_regex":"sample","stuck_download_timeout_minutes":90,"cleanup_error_after_days":7}`
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/general", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(settings.updateCalls) != 1 {
		t.Fatalf("UpdateGeneral calls = %d, want 1", len(settings.updateCalls))
	}
	got := settings.updateCalls[0]
	if got.DownloadDir != "./new-downloads" || got.LogLevel != "debug" || got.ImportIntervalSeconds != 15 || got.ImportMaxRetries != 3 {
		t.Errorf("UpdateGeneral called with %+v", got)
	}
	// Sent through a real JSON body, not a Go struct literal — proves the
	// json tags themselves are correct, not just that the Go field
	// assignment works (a struct literal wouldn't catch a missing/
	// misspelled tag the way decoding real JSON does).
	if got.MinFetchFileSizeBytes != 2048 || got.MaxFetchFileSizeBytes != 104857600 ||
		got.IncludeFileRegex != `\.mkv$` || got.ExcludeFileRegex != "sample" ||
		got.StuckDownloadTimeoutMinutes != 90 || got.CleanupErrorAfterDays != 7 {
		t.Errorf("UpdateGeneral called with %+v, want the new fields decoded from JSON", got)
	}

	var resp map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["restart_required"] {
		t.Error("restart_required = true, want false (fakeSettings default)")
	}
}

func TestHandleUpdateGeneralSettings_ReportsRestartRequired(t *testing.T) {
	settings := &fakeSettings{restartReq: true}
	srv, _ := newTestServer(t, nil, nil, settings)

	body := `{"port":9999,"data_dir":"./data","download_dir":"./downloads","log_level":"info","import_interval_seconds":10,"import_max_retries":5}`
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/general", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["restart_required"] {
		t.Error("restart_required = false, want true")
	}
}

func TestHandleUpdateGeneralSettings_RejectsInvalid(t *testing.T) {
	settings := &fakeSettings{updateErr: errors.New("invalid log_level")}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/general", strings.NewReader(`{"log_level":"bogus"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUpdateGeneralSettings_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/general", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetCategories(t *testing.T) {
	settings := &fakeSettings{torrentCategories: []string{"movies", "tv"}, usenetCategories: []string{"*"}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/categories"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got categoriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Torrent) != 2 || len(got.Usenet) != 1 {
		t.Errorf("got = %+v", got)
	}
}

func TestHandleGetCategories_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings/categories", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAddCategory(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/settings/categories", strings.NewReader(`{"protocol":"torrent","name":"movies"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if len(settings.addCategoryCalls) != 1 || settings.addCategoryCalls[0].Protocol != "torrent" || settings.addCategoryCalls[0].Name != "movies" {
		t.Errorf("AddCategory calls = %+v", settings.addCategoryCalls)
	}
}

func TestHandleAddCategory_RejectsInvalid(t *testing.T) {
	settings := &fakeSettings{addCategoryErr: errors.New("unknown protocol")}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/settings/categories", strings.NewReader(`{"protocol":"bogus","name":"x"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAddCategory_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/settings/categories", strings.NewReader(`{"protocol":"torrent","name":"x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetCategories_IncludesPaths(t *testing.T) {
	settings := &fakeSettings{categoryPaths: map[string]string{"movies": "/mnt/movies"}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/categories"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got categoriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Paths["movies"] != "/mnt/movies" {
		t.Errorf("got.Paths = %v, want movies -> /mnt/movies", got.Paths)
	}
}

func TestHandleSetCategoryPath(t *testing.T) {
	settings := &fakeSettings{}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/categories/path", strings.NewReader(`{"category":"movies","path":"/mnt/movies"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if settings.setCategoryPathCall == nil || settings.setCategoryPathCall.Category != "movies" || settings.setCategoryPathCall.Path != "/mnt/movies" {
		t.Errorf("SetCategoryPath call = %+v", settings.setCategoryPathCall)
	}
}

func TestHandleSetCategoryPath_RejectsInvalid(t *testing.T) {
	settings := &fakeSettings{setCategoryPathErr: errors.New("category must not be empty")}
	srv, _ := newTestServer(t, nil, nil, settings)

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/categories/path", strings.NewReader(`{"category":"","path":"/mnt/x"}`))
	req.Header.Set("Authorization", "Bearer secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSetCategoryPath_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/settings/categories/path", strings.NewReader(`{"category":"movies","path":"/mnt/movies"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRemoveCategory(t *testing.T) {
	settings := &fakeSettings{categoryPaths: map[string]string{"movies": "/mnt/movies"}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/settings/categories/movies"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if settings.removeCategoryCall != "movies" {
		t.Errorf("RemoveCategory call = %q, want %q", settings.removeCategoryCall, "movies")
	}
	if _, ok := settings.categoryPaths["movies"]; ok {
		t.Errorf("categoryPaths = %v, want no movies entry", settings.categoryPaths)
	}
}

func TestHandleRemoveCategory_RejectsInvalid(t *testing.T) {
	settings := &fakeSettings{removeCategoryErr: errors.New("category must not be empty")}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/settings/categories/movies"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRemoveCategory_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/settings/categories/movies", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func seedDownload(t *testing.T, db *database.DB, kind database.Kind, providerDownloadID string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: "dl-" + providerDownloadID, Provider: "fake", ProviderDownloadID: providerDownloadID,
		Kind: kind, Hash: "hash-" + providerDownloadID, Name: "Test Download",
		Category: "tv-sonarr", SizeBytes: 1024, State: database.StateDownloading, Progress: 0.5,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}
	return d
}

func TestHandleListDownloads(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	seedDownload(t, db, database.KindTorrent, "p1")
	seedDownload(t, db, database.KindUsenet, "p2")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

// TestHandleListDownloads_IncludesLiveStatus proves the native API surfaces
// the same fast-moving fields (ETA, torrent swarm info, usenet phase) the
// compat shims already did — read from database.DB's own in-memory
// LiveStatus cache, not a new synchronous provider call this handler makes
// itself (see toDownloadResponse's own doc comment for why).
func TestHandleListDownloads_IncludesLiveStatus(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	db.RefreshFromProvider(context.Background(), []*database.Download{d}, []debrid.DownloadStatus{
		{ID: debrid.ProviderDownloadID("p1"), State: debrid.StateDownloading, Progress: 0.5,
			ETASeconds: 754, Seeders: 3, Leechers: 1, DownloadSpeedBytes: 191117},
	}, time.Now())

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	item := got[0]
	if item.ETASeconds != 754 || item.Seeders != 3 || item.Leechers != 1 || item.DownloadSpeedBytes != 191117 {
		t.Errorf("live fields = %+v, want ETASeconds=754 Seeders=3 Leechers=1 DownloadSpeedBytes=191117", item)
	}
}

// TestHandleListDownloads_FiltersByAddedVia proves ?added_via=arr|manual
// scopes the response to just the web UI's Managed or Manual tab, and that
// an unrecognized/omitted value returns everything unfiltered.
func TestHandleListDownloads_FiltersByAddedVia(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	ctx := context.Background()

	arrDL := &database.Download{
		ID: "dl-arr-1", Provider: "fake", ProviderDownloadID: "arr-1", Kind: database.KindTorrent,
		Name: "Arr Download", State: database.StateDownloading, AddedVia: database.AddedViaArr,
	}
	manualDL := &database.Download{
		ID: "dl-manual-1", Provider: "fake", ProviderDownloadID: "manual-1", Kind: database.KindTorrent,
		Name: "Manual Download", State: database.StateDownloading, AddedVia: database.AddedViaManual,
	}
	if err := db.InsertDownload(ctx, arrDL); err != nil {
		t.Fatalf("InsertDownload(arr) error = %v", err)
	}
	if err := db.InsertDownload(ctx, manualDL); err != nil {
		t.Fatalf("InsertDownload(manual) error = %v", err)
	}

	get := func(query string) []downloadResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads"+query))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", query, rec.Code)
		}
		var got []downloadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	if got := get("?added_via=arr"); len(got) != 1 || got[0].ID != "dl-arr-1" {
		t.Errorf("?added_via=arr = %+v, want just dl-arr-1", got)
	}
	if got := get("?added_via=manual"); len(got) != 1 || got[0].ID != "dl-manual-1" {
		t.Errorf("?added_via=manual = %+v, want just dl-manual-1", got)
	}
	if got := get(""); len(got) != 2 {
		t.Errorf("no filter = %d results, want 2 (unfiltered)", len(got))
	}
	if got := get("?added_via=bogus"); len(got) != 2 {
		t.Errorf("?added_via=bogus = %d results, want 2 (unrecognized value ignored, unfiltered)", len(got))
	}
}

func TestHandleListDownloads_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/downloads", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetDownload(t *testing.T) {
	// Files come from a live provider query now (see filesForDownload), not
	// a local cache — found via a real bug: the local download_files table
	// was defined but never actually populated anywhere, so GET
	// /api/v1/downloads/{id} always returned files: [] in production.
	provider := &fakeProvider{filesResp: []debrid.DownloadFile{
		{ProviderFileID: "f1", Path: "movie.mkv", SizeBytes: 1024},
	}}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		downloadResponse
		Files []downloadFileResponse `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != d.ID {
		t.Errorf("ID = %q, want %q", got.ID, d.ID)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "movie.mkv" || got.Files[0].ProviderFileID != "f1" {
		t.Errorf("files = %+v", got.Files)
	}
}

// TestHandleGetDownload_ExposesCachedAt proves cached_at round-trips through
// the native API once a row has actually reached provider_completed — and
// stays absent until then, distinct from completed_at (files on disk).
func TestHandleGetDownload_ExposesCachedAt(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	var beforeCached downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &beforeCached); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if beforeCached.CachedAt != nil {
		t.Errorf("cached_at = %v before provider_completed, want nil", beforeCached.CachedAt)
	}

	if err := db.UpdateDownloadStatus(context.Background(), d.ID, database.StateProviderCompleted, 1.0, d.SizeBytes, nil, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	var afterCached downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &afterCached); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if afterCached.CachedAt == nil {
		t.Error("cached_at = nil after provider_completed, want set")
	}
}

// TestHandleGetDownload_ReportsLiveFetchProgress proves the fix for a real
// UX gap: a Managed download's reported progress used to freeze at 1.0 the
// moment the provider itself finished, for however long
// internal/importer's own local file transfer to disk actually took — see
// database.DB.SetFetchProgress's own doc comment. A live fetch progress
// substitutes for d.Progress in the response while provider_completed;
// falls back to d.Progress once nothing's being tracked (e.g. before the
// fetch starts, or once ready_for_import).
func TestHandleGetDownload_ReportsLiveFetchProgress(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")
	ctx := context.Background()
	if err := db.UpdateDownloadStatus(ctx, d.ID, database.StateProviderCompleted, 1.0, d.SizeBytes, nil, ""); err != nil {
		t.Fatalf("UpdateDownloadStatus() error = %v", err)
	}

	db.SetFetchProgress(d.ID, 0.35)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	var withFetchProgress downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &withFetchProgress); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if withFetchProgress.Progress != 0.35 {
		t.Errorf("progress = %v, want 0.35 (live fetch progress substituted in)", withFetchProgress.Progress)
	}

	db.ClearFetchProgress(d.ID)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	var withoutFetchProgress downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &withoutFetchProgress); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if withoutFetchProgress.Progress != 1.0 {
		t.Errorf("progress = %v, want 1.0 (d.Progress, no fetch progress tracked)", withoutFetchProgress.Progress)
	}
}

// TestHandleGetDownload_ExposesHasSource proves has_source reflects whether
// the row's (never directly exposed) Source is non-empty — what the web UI
// gates its Re-add button on, since resubmitting requires a stored original
// link (see handleReAddDownload's own Source=="" check).
func TestHandleGetDownload_ExposesHasSource(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	noSource := seedDownload(t, db, database.KindTorrent, "p1")

	withSource := &database.Download{
		ID: "dl-p2-with-source", Provider: "fake", ProviderDownloadID: "p2",
		Kind: database.KindTorrent, Hash: "hash-p2", Name: "Test Download",
		State: database.StateDownloading, Source: "magnet:?xt=urn:btih:abc123",
	}
	if err := db.InsertDownload(context.Background(), withSource); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+noSource.ID))
	var gotNoSource downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gotNoSource); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotNoSource.HasSource {
		t.Error("has_source = true for a row with no stored Source, want false")
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+withSource.ID))
	var gotWithSource downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gotWithSource); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotWithSource.HasSource {
		t.Error("has_source = false for a row with a stored Source, want true")
	}

	// A usenet download added via an uploaded .nzb file has no Source URL,
	// but does have a stored SourceFile — has_source should reflect that too.
	withSourceFile := &database.Download{
		ID: "dl-p3-with-source-file", Provider: "fake", ProviderDownloadID: "p3",
		Kind: database.KindUsenet, Hash: "hash-p3", Name: "Test Download",
		State:          database.StateDownloading,
		SourceFile:     []byte("fake nzb bytes"),
		SourceFileName: "release.nzb",
	}
	if err := db.InsertDownload(context.Background(), withSourceFile); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+withSourceFile.ID))
	var gotWithSourceFile downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gotWithSourceFile); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotWithSourceFile.HasSource {
		t.Error("has_source = false for a row with a stored SourceFile, want true")
	}
}

func TestHandleGetDownload_ExposesRetryInfo(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	nextRetry := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	if err := db.UpdateDownloadRetry(context.Background(), d.ID, 2, nextRetry, "connection reset"); err != nil {
		t.Fatalf("UpdateDownloadRetry() error = %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", got.RetryCount)
	}
	if got.NextRetryAt == nil {
		t.Fatal("NextRetryAt is nil, want it set")
	}
	if got.ErrorMessage != "connection reset" {
		t.Errorf("ErrorMessage = %q, want connection reset", got.ErrorMessage)
	}
}

func TestHandleGetDownload_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/does-not-exist"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHandleGetDownload_FilesUnavailableIsNotAHardError proves a provider
// error while listing files (e.g. still queued, nothing to list yet)
// degrades to an empty file list rather than failing the whole download
// detail response.
// TestHandleGetDownload_FilesUnavailableIsNotAHardError also proves the
// underlying reason isn't discarded (files_error) — a download with an
// empty file list because the provider genuinely has no record of it
// anymore (e.g. deleted directly through the provider's own site — a real,
// observed case for a Manual/discovered download) needs to look different
// from one that's just not processed yet, or the web UI has no way to tell
// a permanently-gone download apart from a still-queued one.
func TestHandleGetDownload_FilesUnavailableIsNotAHardError(t *testing.T) {
	provider := &fakeProvider{filesErr: errors.New("torbox: torrent 64235095 not found")}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		downloadResponse
		Files      []downloadFileResponse `json:"files"`
		FilesError string                 `json:"files_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Files == nil || len(got.Files) != 0 {
		t.Errorf("files = %v, want an empty (not null) array", got.Files)
	}
	if got.FilesError != "torbox: torrent 64235095 not found" {
		t.Errorf("files_error = %q, want the underlying provider error preserved", got.FilesError)
	}
}

// TestHandleGetDownload_NoFilesErrorWhenFilesSucceed proves files_error is
// omitted entirely (not just empty) on the ordinary success path.
func TestHandleGetDownload_NoFilesErrorWhenFilesSucceed(t *testing.T) {
	provider := &fakeProvider{filesResp: []debrid.DownloadFile{{ProviderFileID: "1", Path: "movie.mkv", SizeBytes: 1024}}}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "files_error") {
		t.Errorf("body contains files_error on the success path, want it omitted entirely: %s", rec.Body.String())
	}
}

func TestHandleGetFileLink(t *testing.T) {
	provider := &fakeProvider{linkResp: "https://cdn.torbox.app/movie.mkv"}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/files/f1/link"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if provider.linkRequested != "f1" {
		t.Errorf("RequestDownloadLink called with fileID %q, want f1", provider.linkRequested)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["url"] != "https://cdn.torbox.app/movie.mkv" {
		t.Errorf("url = %q", got["url"])
	}
}

func TestHandleGetFileLink_Usenet(t *testing.T) {
	provider := &fakeProvider{linkResp: "https://cdn.torbox.app/episode.mkv"}
	srv, db := newTestServer(t, nil, provider, nil)
	d := seedDownload(t, db, database.KindUsenet, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/files/f1/link"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetFileLink_ProviderError(t *testing.T) {
	provider := &fakeProvider{linkErr: errors.New("torbox: link expired")}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/files/f1/link"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleGetFileLink_NoProviderConfigured(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/files/f1/link"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleGetFileLink_DownloadNotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/does-not-exist/files/f1/link"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGetFileLink_RequiresAuth(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/files/f1/link", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGetZipLink(t *testing.T) {
	provider := &fakeProvider{zipLinkResp: "https://cdn.torbox.app/all.zip"}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/zip-link"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if provider.zipLinkID != "p1" {
		t.Errorf("RequestZipDownloadLink called with id %q, want p1", provider.zipLinkID)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["url"] != "https://cdn.torbox.app/all.zip" {
		t.Errorf("url = %q", got["url"])
	}
}

func TestHandleGetZipLink_Usenet(t *testing.T) {
	provider := &fakeProvider{zipLinkResp: "https://cdn.torbox.app/all.zip"}
	srv, db := newTestServer(t, nil, provider, nil)
	d := seedDownload(t, db, database.KindUsenet, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/zip-link"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetZipLink_ProviderError(t *testing.T) {
	provider := &fakeProvider{zipLinkErr: errors.New("torbox: zip generation failed")}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/zip-link"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleGetZipLink_NoProviderConfigured(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/zip-link"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleGetZipLink_DownloadNotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/downloads/does-not-exist/zip-link"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGetZipLink_RequiresAuth(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+d.ID+"/zip-link", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRetryDownload(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")
	if err := db.UpdateDownloadRetry(context.Background(), d.ID, 5, time.Now().Add(-time.Hour), "gave up"); err != nil {
		t.Fatalf("seed UpdateDownloadRetry() error = %v", err)
	}
	if err := db.UpdateDownloadStatus(context.Background(), d.ID, database.StateError, 0, 0, nil, "gave up"); err != nil {
		t.Fatalf("seed UpdateDownloadStatus() error = %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/retry"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != database.StateProviderCompleted {
		t.Errorf("state = %q, want provider_completed", got.State)
	}
	if got.RetryCount != 0 {
		t.Errorf("retry_count = %d, want 0", got.RetryCount)
	}
	if got.ErrorMessage != "" {
		t.Errorf("error_message = %q, want cleared", got.ErrorMessage)
	}
}

func TestHandleRetryDownload_RejectsNonErrorState(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1") // seedDownload defaults to StateDownloading

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/retry"))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestHandleRetryDownload_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/does-not-exist/retry"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleRetryDownload_RequiresAuth(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/retry", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// seedErroredDownload seeds a download in StateError with a non-empty
// Source, the precondition for both retry and re-add — mirrors seedDownload
// but gives the caller control over Source and Kind, since re-add tests
// need it.
func seedErroredDownload(t *testing.T, db *database.DB, kind database.Kind, providerDownloadID, source string) *database.Download {
	t.Helper()
	d := &database.Download{
		ID: "dl-" + providerDownloadID, Provider: "torbox", ProviderDownloadID: providerDownloadID,
		Kind: kind, Hash: "hash-" + providerDownloadID, Name: "Test Download",
		Category: "tv-sonarr", SizeBytes: 1024, State: database.StateError, Progress: 1.0,
		Source: source,
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}
	return d
}

func TestHandleReAddDownload_Torrent(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "new-torrent-id"}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedErroredDownload(t, db, database.KindTorrent, "old-torrent-id", testMagnet)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedMagnet != testMagnet {
		t.Errorf("AddMagnet called with %q, want %q", provider.addedMagnet, testMagnet)
	}
	if provider.deleteID != "old-torrent-id" {
		t.Errorf("best-effort Delete called with %q, want old-torrent-id", provider.deleteID)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != database.StateQueued {
		t.Errorf("state = %q, want queued", got.State)
	}

	row, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if row.ProviderDownloadID != "new-torrent-id" {
		t.Errorf("ProviderDownloadID = %q, want new-torrent-id", row.ProviderDownloadID)
	}
}

func TestHandleReAddDownload_Usenet(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "new-nzb-id"}
	srv, db := newTestServer(t, nil, provider, nil)
	const nzbURL = "https://example.com/release.nzb"
	d := seedErroredDownload(t, db, database.KindUsenet, "old-nzb-id", nzbURL)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedURL != nzbURL {
		t.Errorf("AddNZBURL called with %q, want %q", provider.addedURL, nzbURL)
	}
}

// TestHandleReAddDownload_UsenetFileFallback proves a usenet download with
// no Source (added via an uploaded .nzb file, not a URL) falls back to
// resubmitting the stored file bytes via AddNZBFile instead of AddNZBURL —
// see database.Download.SourceFile.
func TestHandleReAddDownload_UsenetFileFallback(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "new-nzb-id"}
	srv, db := newTestServer(t, nil, provider, nil)

	d := &database.Download{
		ID: "dl-file-based", Provider: "torbox", ProviderDownloadID: "old-nzb-id",
		Kind: database.KindUsenet, Hash: "hash-old-nzb-id", Name: "Test Download",
		State: database.StateError, Progress: 1.0,
		SourceFile: []byte("fake nzb bytes"), SourceFileName: "release.nzb",
	}
	if err := db.InsertDownload(context.Background(), d); err != nil {
		t.Fatalf("seed InsertDownload() error = %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedURL != "" {
		t.Errorf("AddNZBURL should not have been called, got url=%q", provider.addedURL)
	}
	if provider.addedFilename != "release.nzb" || string(provider.addedFile) != "fake nzb bytes" {
		t.Errorf("AddNZBFile called with filename=%q data=%q, want release.nzb/fake nzb bytes", provider.addedFilename, provider.addedFile)
	}
}

func TestHandleReAddDownload_RejectsNonErrorState(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1") // seedDownload defaults to StateDownloading

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestHandleReAddDownload_RejectsEmptySource(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedErroredDownload(t, db, database.KindTorrent, "p1", "") // added via file upload, no source stored

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleReAddDownload_ProviderError(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: rate limited")}
	srv, db := newTestServer(t, provider, nil, nil)
	d := seedErroredDownload(t, db, database.KindTorrent, "p1", testMagnet)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleReAddDownload_NoProviderConfigured(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedErroredDownload(t, db, database.KindTorrent, "p1", testMagnet)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleReAddDownload_ConflictsOnDedupedProviderID(t *testing.T) {
	// The re-add resolves to a provider_download_id already tracked under a
	// different local row — must not silently corrupt either row.
	provider := &fakeProvider{providerName: "torbox", addID: "already-tracked-id"}
	srv, db := newTestServer(t, provider, nil, nil)
	// Must share the provider name ("torbox", matching provider.Name()) with
	// the row under test — seedDownload hardcodes provider "fake", which
	// wouldn't collide with anything since GetDownloadByProviderID keys on
	// (provider, provider_download_id) together.
	other := &database.Download{
		ID: "dl-already-tracked", Provider: "torbox", ProviderDownloadID: "already-tracked-id",
		Kind: database.KindTorrent, Hash: "hash-other", Name: "Other Download", State: database.StateDownloading,
	}
	if err := db.InsertDownload(context.Background(), other); err != nil {
		t.Fatalf("seed InsertDownload(other) error = %v", err)
	}
	d := seedErroredDownload(t, db, database.KindTorrent, "old-id", testMagnet)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	// Neither row should have been mutated by the failed re-add.
	row, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if row.ProviderDownloadID != "old-id" {
		t.Errorf("ProviderDownloadID = %q, want unchanged old-id", row.ProviderDownloadID)
	}
}

func TestHandleReAddDownload_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/downloads/does-not-exist/readd"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleReAddDownload_RequiresAuth(t *testing.T) {
	srv, db := newTestServer(t, nil, nil, nil)
	d := seedErroredDownload(t, db, database.KindTorrent, "p1", testMagnet)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/downloads/"+d.ID+"/readd", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleDeleteDownload(t *testing.T) {
	torrentDeleter := &fakeProvider{}
	srv, db := newTestServer(t, torrentDeleter, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID+"?deleteFiles=true"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if !torrentDeleter.deleteCalled {
		t.Error("expected torrent provider Delete to be called")
	}
	if torrentDeleter.deleteID != debrid.ProviderDownloadID("p1") {
		t.Errorf("provider id = %q, want p1", torrentDeleter.deleteID)
	}
	if !torrentDeleter.deleteFiles {
		t.Error("deleteFiles should have been true")
	}

	got, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got != nil {
		t.Errorf("download still present after delete: %+v", got)
	}
}

// TestHandleDeleteDownload_DeleteFilesTrueRemovesLocalFiles proves
// deleteFiles=true actually removes local files, not just the provider-side
// copy — before DeleteLocalFiles existed, the provider.Delete call above was
// the only thing deleteFiles affected, and TorBox's own Delete implementation
// ignores that flag entirely (it only ever deletes the provider-side copy),
// so local disk was never touched by this endpoint at all.
func TestHandleDeleteDownload_DeleteFilesTrueRemovesLocalFiles(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, &fakeProvider{}, nil, settings)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID+"?deleteFiles=true"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(settings.deleteLocalFilesCalls) != 1 || settings.deleteLocalFilesCalls[0] != d.ID {
		t.Errorf("DeleteLocalFiles calls = %v, want exactly [%s]", settings.deleteLocalFilesCalls, d.ID)
	}
}

// TestHandleDeleteDownload_DeleteFilesFalseSkipsLocalFileRemoval proves an
// omitted/false deleteFiles never touches local disk — a delete that only
// meant to stop tracking a download (e.g. an *arr app's routine post-import
// cleanup) shouldn't silently remove files the user still wants.
func TestHandleDeleteDownload_DeleteFilesFalseSkipsLocalFileRemoval(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, &fakeProvider{}, nil, settings)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(settings.deleteLocalFilesCalls) != 0 {
		t.Errorf("DeleteLocalFiles calls = %v, want none", settings.deleteLocalFilesCalls)
	}
}

// TestHandleDeleteDownload_CancelsInFlightFetch proves a delete always
// interrupts internal/importer's in-flight fetch for the download first —
// unconditionally, even when deleteFiles isn't set — before touching
// anything else. Without this, a fetch goroutine already mid-write for this
// exact download would have no way to know it was just deleted and would
// keep writing, potentially recreating whatever local-file cleanup a
// deleteFiles=true request just performed. See internal/importer's own
// TestCancelFetch_StopsInFlightFetch for proof CancelFetch itself works;
// this only proves the handler actually calls it, and calls it first.
func TestHandleDeleteDownload_CancelsInFlightFetch(t *testing.T) {
	settings := &fakeSettings{}
	srv, db := newTestServer(t, &fakeProvider{}, nil, settings)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(settings.cancelFetchCalls) != 1 || settings.cancelFetchCalls[0] != d.ID {
		t.Errorf("CancelFetch calls = %v, want exactly [%s]", settings.cancelFetchCalls, d.ID)
	}
}

// TestHandleDeleteDownload_RecordsDeletedTombstone proves a real delete
// through the API records a tombstone — see
// database.RecordDeletedDownload/RecentlyDeletedDownloads, which
// internal/importer's discoverManual checks to avoid re-adopting a
// just-deleted item as a fresh "discovery" if the provider's own listing
// endpoints haven't caught up with the delete yet.
func TestHandleDeleteDownload_RecordsDeletedTombstone(t *testing.T) {
	srv, db := newTestServer(t, &fakeProvider{}, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	tombstoned, err := db.RecentlyDeletedDownloads(context.Background(), d.Provider, d.Kind)
	if err != nil {
		t.Fatalf("RecentlyDeletedDownloads() error = %v", err)
	}
	if !tombstoned["p1"] {
		t.Errorf("RecentlyDeletedDownloads() = %v, want it to contain p1", tombstoned)
	}
}

func TestHandleDeleteDownload_UsenetKindUsesUsenetProvider(t *testing.T) {
	torrentDeleter := &fakeProvider{}
	usenetDeleter := &fakeProvider{}
	srv, db := newTestServer(t, torrentDeleter, usenetDeleter, nil)
	d := seedDownload(t, db, database.KindUsenet, "p2")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if torrentDeleter.deleteCalled {
		t.Error("torrent provider should not have been called for a usenet download")
	}
	if !usenetDeleter.deleteCalled {
		t.Error("usenet provider should have been called")
	}
}

func TestHandleDeleteDownload_ProviderErrorStillCleansUpLocally(t *testing.T) {
	failing := &fakeProvider{deleteErr: context.DeadlineExceeded}
	srv, db := newTestServer(t, failing, nil, nil)
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 even though the provider call failed", rec.Code)
	}

	got, err := db.GetDownloadByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if got != nil {
		t.Errorf("download still present after delete despite provider error: %+v", got)
	}
}

// multipartRequest builds a POST request against target with the given form
// fields, and optionally one file part — mirrors how a real browser's
// FormData (or *arr apps' own add calls) would submit it.
func multipartRequest(t *testing.T, target string, fields map[string]string, fileField, filename string, fileData []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if fileData != nil {
		part, err := writer.CreateFormFile(fileField, filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(fileData); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, target, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

const testMagnet = "magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=Some.Release.Name"

func TestHandleAddTorrent_Magnet(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "999", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet, "category": "movies"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedMagnet != testMagnet {
		t.Errorf("AddMagnet called with %q, want %q", provider.addedMagnet, testMagnet)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Protocol != "torrent" {
		t.Errorf("protocol = %q, want torrent", got.Protocol)
	}
	if got.Name != "Some.Release.Name" {
		t.Errorf("name = %q, want the magnet's dn (fallback, since provider status wasn't available yet)", got.Name)
	}
	if got.Hash != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("hash = %q, want the magnet's infohash, lowercased", got.Hash)
	}
	if got.Category != "movies" {
		t.Errorf("category = %q, want movies", got.Category)
	}

	d, err := db.GetDownloadByID(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if d == nil {
		t.Fatal("download was not persisted")
	}
	if d.ProviderDownloadID != "999" || d.Provider != "torbox" {
		t.Errorf("provider_download_id/provider = %q/%q, want 999/torbox", d.ProviderDownloadID, d.Provider)
	}
}

// TestHandleAddTorrent_DedupedByProviderReturnsExisting proves adding a
// magnet whose provider dedupes to an already-tracked torrent_id (e.g.
// TorBox recognizing an already-cached hash and handing back the same ID as
// an earlier add) doesn't 500 on the (provider, provider_download_id) UNIQUE
// constraint — it returns the existing row with 200 instead of creating a
// duplicate. Found via a real TorBox add during manual verification.
func TestHandleAddTorrent_DedupedByProviderReturnsExisting(t *testing.T) {
	provider := &fakeProvider{addID: "999", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, provider, nil, nil)

	first := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, first)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first add status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var firstResp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	second := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, second)
	if rec.Code != http.StatusOK {
		t.Fatalf("second add status = %d, want 200 (already tracked), body=%s", rec.Code, rec.Body.String())
	}
	var secondResp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if secondResp.ID != firstResp.ID {
		t.Errorf("second add ID = %q, want the same as the first add (%q)", secondResp.ID, firstResp.ID)
	}

	rows, err := db.ListAllDownloads(context.Background())
	if err != nil {
		t.Fatalf("ListAllDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("len(rows) = %d, want 1 (no duplicate row created)", len(rows))
	}
}

func TestHandleAddTorrent_UsesProviderStatusWhenAvailable(t *testing.T) {
	provider := &fakeProvider{
		addID: "999",
		statusResp: debrid.DownloadStatus{
			Name: "Real Name From Provider", Hash: "REALHASH", SizeBytes: 12345,
			Progress: 0.5, State: debrid.StateDownloading,
		},
	}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Real Name From Provider" {
		t.Errorf("name = %q, want the provider's real name, not the magnet-derived fallback", got.Name)
	}
	if got.Hash != "realhash" {
		t.Errorf("hash = %q, want the provider's hash (lowercased)", got.Hash)
	}
	if got.State != database.StateDownloading {
		t.Errorf("state = %q, want downloading", got.State)
	}
}

func TestHandleAddTorrent_File(t *testing.T) {
	provider := &fakeProvider{addID: "999", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"category": "tv"}, "file", "release.torrent", []byte("fake torrent bytes"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedFilename != "release.torrent" || string(provider.addedFile) != "fake torrent bytes" {
		t.Errorf("AddTorrentFile called with filename=%q data=%q", provider.addedFilename, provider.addedFile)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "release.torrent" {
		t.Errorf("name = %q, want the uploaded filename (fallback)", got.Name)
	}
}

func TestHandleAddTorrent_RequiresMagnetOrFile(t *testing.T) {
	srv, _ := newTestServer(t, &fakeProvider{}, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"category": "tv"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAddTorrent_NoProviderConfigured(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddTorrent_ProviderReturnsErrNoProvider(t *testing.T) {
	provider := &fakeProvider{addErr: debrid.ErrNoProvider}
	srv, _ := newTestServer(t, provider, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddTorrent_ProviderError(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: rate limited")}
	srv, _ := newTestServer(t, provider, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleAddTorrent_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, &fakeProvider{}, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestHandleAddTorrent_AddedViaArr_AdminCreatesManaged proves an admin can
// explicitly add a Managed download directly (not just via an *arr app
// through the compat shims) — the download lands with added_via=arr and its
// requested category, which internal/importer will auto-fetch to
// download_dir/the category's override the same as if Sonarr/Radarr had
// added it.
func TestHandleAddTorrent_AddedViaArr_AdminCreatesManaged(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "999", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet, "category": "tv", "added_via": "arr"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AddedVia != string(database.AddedViaArr) {
		t.Errorf("added_via = %q, want arr", got.AddedVia)
	}
	if got.Category != "tv" {
		t.Errorf("category = %q, want tv", got.Category)
	}

	d, err := db.GetDownloadByID(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if d.AddedVia != database.AddedViaArr {
		t.Errorf("persisted AddedVia = %q, want arr", d.AddedVia)
	}
}

// TestHandleAddTorrent_AddedViaArr_MemberForbidden proves a member can't add
// a Managed download by asking the API directly, even though the web UI
// never shows them the option at all — server-side enforcement, not just a
// hidden UI control, matching how member scoping is enforced everywhere
// else (see downloadByID/handleListDownloads). Nothing is added at all: the
// request is rejected before the provider is ever called.
func TestHandleAddTorrent_AddedViaArr_MemberForbidden(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "999"}
	srv, _ := newTestServer(t, provider, nil, nil)
	token := srv.sessions.create("bob", RoleMember)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet, "added_via": "arr"}, "", "", nil)
	req.Header.Del("Authorization")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if provider.addedMagnet != "" {
		t.Error("AddMagnet was called despite the member being forbidden — nothing should reach the provider")
	}
}

// TestHandleAddTorrent_AddedViaOmitted_MemberStillAllowed proves the new
// added_via enforcement doesn't accidentally take away a member's existing
// ability to add Manual downloads (the one thing they're scoped to) —
// omitting the field (or sending "manual") stays exactly as unrestricted as
// before this change.
func TestHandleAddTorrent_AddedViaOmitted_MemberStillAllowed(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "999", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServer(t, provider, nil, nil)
	token := srv.sessions.create("bob", RoleMember)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	req.Header.Del("Authorization")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AddedVia != string(database.AddedViaManual) {
		t.Errorf("added_via = %q, want manual", got.AddedVia)
	}
}

func TestHandleAddUsenet_URL(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "nzb-1", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, nil, provider, nil)

	const nzbURL = "https://example.com/release.nzb"
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": nzbURL, "category": "movies"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedURL != nzbURL {
		t.Errorf("AddNZBURL called with %q, want %q", provider.addedURL, nzbURL)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Protocol != "usenet" {
		t.Errorf("protocol = %q, want usenet", got.Protocol)
	}
	if got.Name != nzbURL {
		t.Errorf("name = %q, want the URL (fallback, since provider status wasn't available yet)", got.Name)
	}

	d, err := db.GetDownloadByID(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if d == nil || d.ProviderDownloadID != "nzb-1" || d.Kind != database.KindUsenet {
		t.Errorf("persisted row = %+v, want provider_download_id=nzb-1 kind=usenet", d)
	}
}

func TestHandleAddUsenet_File(t *testing.T) {
	provider := &fakeProvider{addID: "nzb-2", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServer(t, nil, provider, nil)

	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"category": "tv"}, "file", "release.nzb", []byte("fake nzb bytes"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedFilename != "release.nzb" || string(provider.addedFile) != "fake nzb bytes" {
		t.Errorf("AddNZBFile called with filename=%q data=%q", provider.addedFilename, provider.addedFile)
	}
}

// TestHandleAddUsenet_File_StoresSourceFile proves the uploaded bytes are
// persisted on the row (not just forwarded to the provider) — what makes
// Re-add possible later for a download that has no Source URL at all. See
// database.Download.SourceFile.
func TestHandleAddUsenet_File_StoresSourceFile(t *testing.T) {
	provider := &fakeProvider{addID: "nzb-3", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServer(t, nil, provider, nil)

	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"category": "tv"}, "file", "release.nzb", []byte("fake nzb bytes"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HasSource {
		t.Error("has_source = false right after a file-based add, want true")
	}

	filename, data, err := db.GetSourceFile(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetSourceFile() error = %v", err)
	}
	if filename != "release.nzb" || string(data) != "fake nzb bytes" {
		t.Errorf("GetSourceFile() = %q/%q, want release.nzb/fake nzb bytes", filename, data)
	}
}

func TestHandleAddUsenet_RequiresURLOrFile(t *testing.T) {
	srv, _ := newTestServer(t, nil, &fakeProvider{}, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"category": "tv"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAddUsenet_NoProviderConfigured(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": "https://example.com/x.nzb"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddUsenet_ProviderError(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: rate limited")}
	srv, _ := newTestServer(t, nil, provider, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": "https://example.com/x.nzb"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleAddUsenet_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, &fakeProvider{}, nil)
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": "https://example.com/x.nzb"}, "", "", nil)
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestHandleAddUsenet_AddedViaArr_AdminCreatesManaged confirms
// resolveAddedVia's wiring reached this handler too, not just torrent's —
// see TestHandleAddTorrent_AddedViaArr_AdminCreatesManaged/_MemberForbidden
// for the actual admin/member enforcement tests, which are the same shared
// function underneath and don't need repeating per kind.
func TestHandleAddUsenet_AddedViaArr_AdminCreatesManaged(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "nzb-1", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServer(t, nil, provider, nil)

	const nzbURL = "https://example.com/release.nzb"
	req := multipartRequest(t, "/api/v1/downloads/usenet", map[string]string{"url": nzbURL, "category": "tv", "added_via": "arr"}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AddedVia != string(database.AddedViaArr) {
		t.Errorf("added_via = %q, want arr", got.AddedVia)
	}
	if got.Category != "tv" {
		t.Errorf("category = %q, want tv", got.Category)
	}
}

// formURLEncodedRequest builds a request handleAddWebDownload can parse via
// r.ParseForm() — unlike torrent/usenet adds, this endpoint is genuinely
// link-only (no file-upload variant, matching TorBox's own createwebdownload
// endpoint), so it takes a plain application/x-www-form-urlencoded body
// rather than multipart.
func formURLEncodedRequest(t *testing.T, target string, fields map[string]string) *http.Request {
	t.Helper()
	values := url.Values{}
	for k, v := range fields {
		if v != "" {
			values.Set(k, v)
		}
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func TestHandleAddWebDownload_Link(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "123", statusErr: errors.New("not indexed yet")}
	srv, db := newTestServerWithWebDownload(t, provider, nil)

	const link = "https://mega.nz/folder/abc123"
	req := formURLEncodedRequest(t, "/api/v1/downloads/webdl", map[string]string{"link": link, "category": "movies"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if provider.addedLink != link {
		t.Errorf("AddLink called with %q, want %q", provider.addedLink, link)
	}

	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Protocol != "webdl" {
		t.Errorf("protocol = %q, want webdl", got.Protocol)
	}
	if got.Name != link {
		t.Errorf("name = %q, want the link (fallback, since provider status wasn't available yet)", got.Name)
	}

	d, err := db.GetDownloadByID(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetDownloadByID() error = %v", err)
	}
	if d == nil || d.ProviderDownloadID != "123" || d.Kind != database.KindWebDL || d.Source != link {
		t.Errorf("persisted row = %+v, want provider_download_id=123 kind=webdl source=%s", d, link)
	}
}

func TestHandleAddWebDownload_RequiresLink(t *testing.T) {
	srv, _ := newTestServerWithWebDownload(t, &fakeProvider{}, nil)
	req := formURLEncodedRequest(t, "/api/v1/downloads/webdl", map[string]string{"category": "movies"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAddWebDownload_NoProviderConfigured(t *testing.T) {
	srv, _ := newTestServerWithWebDownload(t, nil, nil)
	req := formURLEncodedRequest(t, "/api/v1/downloads/webdl", map[string]string{"link": "https://mega.nz/folder/abc123"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAddWebDownload_ProviderError(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: unsupported hoster")}
	srv, _ := newTestServerWithWebDownload(t, provider, nil)
	req := formURLEncodedRequest(t, "/api/v1/downloads/webdl", map[string]string{"link": "https://mega.nz/folder/abc123"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleAddWebDownload_RequiresAuth(t *testing.T) {
	srv, _ := newTestServerWithWebDownload(t, &fakeProvider{}, nil)
	req := formURLEncodedRequest(t, "/api/v1/downloads/webdl", map[string]string{"link": "https://mega.nz/folder/abc123"})
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestHandleAddWebDownload_AddedViaArr_AdminCreatesManaged confirms
// resolveAddedVia's wiring reached this handler too — see
// TestHandleAddTorrent_AddedViaArr_AdminCreatesManaged/_MemberForbidden for
// the actual admin/member enforcement tests.
func TestHandleAddWebDownload_AddedViaArr_AdminCreatesManaged(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox", addID: "webdl-1", statusErr: errors.New("not indexed yet")}
	srv, _ := newTestServerWithWebDownload(t, provider, nil)

	req := formURLEncodedRequest(t, "/api/v1/downloads/webdl", map[string]string{
		"link": "https://mega.nz/folder/abc123", "category": "movies", "added_via": "arr",
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var got downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AddedVia != string(database.AddedViaArr) {
		t.Errorf("added_via = %q, want arr", got.AddedVia)
	}
	if got.Category != "movies" {
		t.Errorf("category = %q, want movies", got.Category)
	}
}

// --- Check cached & torrent info previews -----------------------------------

func TestHandleCheckCachedTorrent_Cached(t *testing.T) {
	hash := "abcdef0123456789abcdef0123456789abcdef01" // testMagnet's hash, lowercased
	provider := &fakeProvider{checkCachedResp: map[string]bool{hash: true}}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/check-cached?magnet="+url.QueryEscape(testMagnet))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got checkCachedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Cached {
		t.Error("cached = false, want true")
	}
	if len(provider.checkCachedHashes) != 1 || provider.checkCachedHashes[0] != hash {
		t.Errorf("CheckCached called with %v, want [%s] (the magnet's own lowercased infohash)", provider.checkCachedHashes, hash)
	}
}

func TestHandleCheckCachedTorrent_NotCached(t *testing.T) {
	provider := &fakeProvider{checkCachedResp: map[string]bool{}}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/check-cached?magnet="+url.QueryEscape(testMagnet))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var got checkCachedResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Cached {
		t.Error("cached = true, want false")
	}
}

func TestHandleCheckCachedTorrent_RequiresValidMagnet(t *testing.T) {
	provider := &fakeProvider{}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/check-cached?magnet=not-a-magnet")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (magnet has no valid btih hash)", rec.Code)
	}
}

func TestHandleCheckCachedTorrent_NoProviderConfigured(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/check-cached?magnet="+url.QueryEscape(testMagnet))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestHandleCheckCachedUsenet_HashesTheURL proves the query goes through
// md5Hex, not the raw URL — TorBox's usenet checkcached endpoint expects an
// MD5 of the link, unlike torrent's real infohash (see md5Hex's own doc
// comment).
func TestHandleCheckCachedUsenet_HashesTheURL(t *testing.T) {
	const nzbURL = "https://example.test/release.nzb"
	wantHash := md5Hex(nzbURL)
	provider := &fakeProvider{checkCachedResp: map[string]bool{wantHash: true}}
	srv, _ := newTestServer(t, nil, provider, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/usenet/check-cached?url="+url.QueryEscape(nzbURL))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got checkCachedResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Cached {
		t.Error("cached = false, want true")
	}
	if len(provider.checkCachedHashes) != 1 || provider.checkCachedHashes[0] != wantHash {
		t.Errorf("CheckCached called with %v, want [%s] (md5 of the URL)", provider.checkCachedHashes, wantHash)
	}
}

// TestHandleCheckCachedWebDownload_HashesTheLink is
// TestHandleCheckCachedUsenet_HashesTheURL's Web Downloads counterpart.
func TestHandleCheckCachedWebDownload_HashesTheLink(t *testing.T) {
	const link = "https://mega.nz/folder/abc123"
	wantHash := md5Hex(link)
	provider := &fakeProvider{checkCachedResp: map[string]bool{wantHash: true}}
	srv, _ := newTestServerWithWebDownload(t, provider, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/webdl/check-cached?link="+url.QueryEscape(link))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got checkCachedResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Cached {
		t.Error("cached = false, want true")
	}
	if len(provider.checkCachedHashes) != 1 || provider.checkCachedHashes[0] != wantHash {
		t.Errorf("CheckCached called with %v, want [%s] (md5 of the link)", provider.checkCachedHashes, wantHash)
	}
}

func TestHandleTorrentInfo_Available(t *testing.T) {
	provider := &fakeProvider{torrentInfoResp: debrid.TorrentInfo{
		Name: "Preview.Me", Hash: "abc123", SizeBytes: 999, Seeds: 5, Peers: 2,
		Files: []debrid.TorrentInfoFile{{Path: "Preview.Me/a.mkv", SizeBytes: 900}},
	}}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/info?hash=abc123")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got torrentInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Available || got.Name != "Preview.Me" || got.SizeBytes != 999 || got.Seeds != 5 || got.Peers != 2 {
		t.Errorf("response = %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "Preview.Me/a.mkv" || got.Files[0].SizeBytes != 900 {
		t.Errorf("files = %+v", got.Files)
	}
	if provider.torrentInfoHash != "abc123" {
		t.Errorf("TorrentInfo called with hash %q, want abc123", provider.torrentInfoHash)
	}
}

// TestHandleTorrentInfo_ExtractsHashFromMagnet proves ?magnet= works too,
// not just a raw ?hash= — the "+ Add" form only ever has the magnet, never a
// bare hash, at the point it wants a preview.
func TestHandleTorrentInfo_ExtractsHashFromMagnet(t *testing.T) {
	provider := &fakeProvider{torrentInfoResp: debrid.TorrentInfo{Name: "Preview.Me"}}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/info?magnet="+url.QueryEscape(testMagnet))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if provider.torrentInfoHash != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("TorrentInfo called with hash %q, want the magnet's own lowercased infohash", provider.torrentInfoHash)
	}
}

// TestHandleTorrentInfo_NotAvailableIsRoutine proves a torrent TorBox
// couldn't find (or a provider without TorrentInfoProvider support) reports
// available:false with a 200, the same "available: false" convention as
// handleGetAccountStatus — not a hard error the UI would need special
// handling for.
func TestHandleTorrentInfo_NotAvailableIsRoutine(t *testing.T) {
	provider := &fakeProvider{torrentInfoErr: errors.New("torbox: torrent info: not found")}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/info?hash=0000")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unavailable is routine, not an error)", rec.Code)
	}
	var got torrentInfoResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Available {
		t.Error("available = true, want false")
	}
	if got.Error == "" {
		t.Error("error message = empty, want the underlying reason")
	}
}

func TestHandleTorrentInfo_RequiresHashOrMagnet(t *testing.T) {
	provider := &fakeProvider{}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := authedRequest(http.MethodGet, "/api/v1/downloads/torrent/info")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGetAccountStatus_Available(t *testing.T) {
	settings := &fakeSettings{accountStatus: debrid.AccountStatus{
		PlanName: "Pro", IsSubscribed: true, PremiumExpiresAt: "2027-01-01T00:00:00Z", TotalBytesDownloaded: 1024,
		CooldownUntil: "2026-08-04T04:29:02Z",
	}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/account"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got accountStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Available || got.PlanName != "Pro" || !got.IsSubscribed || got.TotalBytesDownloaded != 1024 {
		t.Errorf("response = %+v", got)
	}
	// CooldownUntil surfaces a real, live-found TorBox account restriction
	// that otherwise makes every download look frozen with no visible
	// explanation — see debrid.AccountStatus.CooldownUntil's own doc comment.
	if got.CooldownUntil != "2026-08-04T04:29:02Z" {
		t.Errorf("CooldownUntil = %q, want passed through unchanged", got.CooldownUntil)
	}
}

// TestHandleGetAccountStatus_Unavailable proves a provider error (not
// configured, or configured but doesn't support AccountProvider) is reported
// as a routine "available: false" rather than a hard HTTP error — see
// handleGetAccountStatus's doc comment.
func TestHandleGetAccountStatus_Unavailable(t *testing.T) {
	settings := &fakeSettings{accountErr: errors.New("debrid: no provider configured")}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/settings/account"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a provider error is routine, not fatal), body=%s", rec.Code, rec.Body.String())
	}

	var got accountStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available || got.Error == "" {
		t.Errorf("response = %+v, want available=false with an error message", got)
	}
}

func TestHandleGetAccountStatus_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings/account", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleStatus_ReturnsSettingsStatus(t *testing.T) {
	tick := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	rateLimited := tick.Add(5 * time.Minute)
	settings := &fakeSettings{statusResult: StatusInfo{
		LastTickAt: &tick,
		Kinds: map[string]KindStatus{
			"torrent": {LastSuccessfulListAt: &tick, ErrorCount: 2},
			"usenet":  {RateLimitedUntil: &rateLimited},
			"webdl":   {},
		},
	}}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/status"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got StatusInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LastTickAt == nil || !got.LastTickAt.Equal(tick) {
		t.Errorf("LastTickAt = %v, want %v", got.LastTickAt, tick)
	}
	if got.Kinds["torrent"].ErrorCount != 2 {
		t.Errorf("torrent ErrorCount = %d, want 2", got.Kinds["torrent"].ErrorCount)
	}
	if got.Kinds["usenet"].RateLimitedUntil == nil || !got.Kinds["usenet"].RateLimitedUntil.Equal(rateLimited) {
		t.Errorf("usenet RateLimitedUntil = %v, want %v", got.Kinds["usenet"].RateLimitedUntil, rateLimited)
	}
}

func TestHandleStatus_PropagatesError(t *testing.T) {
	settings := &fakeSettings{statusErr: errors.New("count downloads by state: db locked")}
	srv, _ := newTestServer(t, nil, nil, settings)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/v1/status"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleStatus_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestHandleAddTorrent_RateLimitedIsRetryable pins a provider rate limit to
// 429 rather than the generic 502 every other provider failure gets. It's a
// genuinely retryable, increasingly routine condition rather than an
// upstream fault: TorBox v9 set /createtorrent to 60/hour for uncached
// torrents, counted per API key across its servers since v8.4.1.
func TestHandleAddTorrent_RateLimitedIsRetryable(t *testing.T) {
	provider := &fakeProvider{addErr: fmt.Errorf("torbox: too many requests: %w", debrid.ErrRateLimited)}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}
}

// A non-rate-limit provider failure must still be a 502 — the 429 above is
// a specific carve-out, not a new default.
func TestHandleAddTorrent_OtherProviderErrorStays502(t *testing.T) {
	provider := &fakeProvider{addErr: errors.New("torbox: upstream exploded")}
	srv, _ := newTestServer(t, provider, nil, nil)

	req := multipartRequest(t, "/api/v1/downloads/torrent", map[string]string{"magnet": testMagnet}, "", "", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleDeleteDownload_SkipsProviderCallForAnotherProvidersDownload
// covers routing by the download's own provider rather than merely by its
// kind. Calling the configured provider with an id that belongs to a
// different account would at best fail and at worst act on an unrelated
// download that happens to share the id. The local row is still removed —
// the provider call has always been best-effort here.
func TestHandleDeleteDownload_SkipsProviderCallForAnotherProvidersDownload(t *testing.T) {
	provider := &fakeProvider{providerName: "torbox"}
	srv, db := newTestServer(t, provider, nil, nil)

	// seedDownload records Provider "fake"; the configured one is "torbox".
	d := seedDownload(t, db, database.KindTorrent, "p1")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/downloads/"+d.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if provider.deleteCalled {
		t.Error("provider Delete was called for a download belonging to a different provider")
	}
	if got, _ := db.GetDownloadByID(context.Background(), d.ID); got != nil {
		t.Error("local row should still be removed — the provider call is best-effort")
	}
}
