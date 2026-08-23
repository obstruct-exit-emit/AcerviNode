package alldebrid

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acervinode/acervinode/internal/debrid"
)

// newTestProvider serves handler as the AllDebrid API.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return NewProvider("alldebrid", "test-key", WithBaseURL(ts.URL))
}

// TestDo_ErrorEnvelopeIsAnError is the shape that most distinguishes
// AllDebrid from TorBox: a failure arrives as HTTP 200 with status "error"
// in the body, so nothing can be concluded from the status code alone.
func TestDo_ErrorEnvelopeIsAnError(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"error","error":{"code":"MAGNET_INVALID_ID","message":"This magnet ID does not exists or is invalid"}}`))
	})

	_, err := p.List(context.Background())
	if err == nil {
		t.Fatal("List() error = nil for an error envelope returned as HTTP 200")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *APIError", err)
	}
	if apiErr.Code != "MAGNET_INVALID_ID" {
		t.Errorf("code = %q, want MAGNET_INVALID_ID", apiErr.Code)
	}
}

// A rate limit has to reach internal/importer as debrid.ErrRateLimited, or
// its backoff never engages for this provider — see docs/providers.md.
func TestAPIError_RateLimitUnwrapsToDebridSentinel(t *testing.T) {
	for _, code := range []string{"TOO_MANY_REQUESTS", "MAGNET_TOO_MANY_ACTIVE"} {
		err := error(&APIError{Code: code, Message: "slow down"})
		if !errors.Is(err, debrid.ErrRateLimited) {
			t.Errorf("code %q does not unwrap to ErrRateLimited", code)
		}
	}
	other := error(&APIError{Code: "MAGNET_INVALID_ID"})
	if errors.Is(other, debrid.ErrRateLimited) {
		t.Error("a non-rate-limit code unwrapped to ErrRateLimited")
	}
}

// TestAddMagnet_PerMagnetErrorIsReported covers AllDebrid's batch shape: the
// endpoint takes several magnets, so one failing is reported *inside* a
// successful envelope rather than as a failed request.
func TestAddMagnet_PerMagnetErrorIsReported(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"magnets":[{"magnet":"m","error":{"code":"MAGNET_INVALID_URI","message":"bad magnet"}}]}}`))
	})

	if _, err := p.AddMagnet(context.Background(), "magnet:?xt=urn:btih:abc", debrid.AddOptions{}); err == nil {
		t.Fatal("AddMagnet() error = nil for a magnet the batch reported as failed")
	}
}

func TestAddMagnet_ReturnsID(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v4/magnet/upload" {
			t.Errorf("path = %s", got)
		}
		w.Write([]byte(`{"status":"success","data":{"magnets":[{"id":703365472,"hash":"ABC","name":"Big Buck Bunny","size":10,"ready":true}]}}`))
	})

	id, err := p.AddMagnet(context.Background(), "magnet:?xt=urn:btih:abc", debrid.AddOptions{})
	if err != nil {
		t.Fatalf("AddMagnet() error = %v", err)
	}
	if id != "703365472" {
		t.Errorf("id = %q, want 703365472", id)
	}
}

// TestList_UsesV41 pins the endpoint version: /v4/magnet/status is
// discontinued and answers with an error envelope, so calling it would
// break every poll.
func TestList_UsesV41(t *testing.T) {
	var path string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"status":"success","data":{"magnets":[]}}`))
	})
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if path != "/v4.1/magnet/status" {
		t.Errorf("path = %q, want /v4.1/magnet/status (v4 is discontinued)", path)
	}
}

func TestMapStatusCode(t *testing.T) {
	cases := []struct {
		code int
		want debrid.DownloadState
	}{
		{0, debrid.StateDownloading}, // In Queue
		{1, debrid.StateDownloading}, // Downloading
		{2, debrid.StateDownloading}, // Compressing / Moving
		{3, debrid.StateDownloading}, // Uploading
		{4, debrid.StateCompleted},   // Ready
		{5, debrid.StateError},       // Upload fail
		{9, debrid.StateError},       // Internal error
		{15, debrid.StateError},      // No peer
		{99, debrid.StateError},      // unknown: not something to call healthy
		{-1, debrid.StateError},      // ditto
	}
	for _, c := range cases {
		if got := mapStatusCode(c.code); got != c.want {
			t.Errorf("mapStatusCode(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

// A cached magnet arrives Ready having transferred nothing, so deriving
// progress from bytes alone would report a finished download as untouched.
func TestMagnetProgress(t *testing.T) {
	if got := magnetProgress(magnet{StatusCode: 4, Size: 100, Downloaded: 0}); got != 1 {
		t.Errorf("progress of a Ready magnet = %v, want 1", got)
	}
	if got := magnetProgress(magnet{StatusCode: 1, Size: 100, Downloaded: 25}); got != 0.25 {
		t.Errorf("progress = %v, want 0.25", got)
	}
	if got := magnetProgress(magnet{StatusCode: 1, Size: 0, Downloaded: 0}); got != 0 {
		t.Errorf("progress with unknown size = %v, want 0", got)
	}
}

// TestFiles_FlattensFolders covers AllDebrid's nested file shape: a folder
// carries its children in "e" and has no link of its own, so the tree has
// to be walked or a nested torrent reports no files at all.
func TestFiles_FlattensFolders(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"magnets":[{"id":"1","files":[
			{"n":"Season 1","e":[
				{"n":"ep1.mkv","s":10,"l":"https://alldebrid.com/f/aaa"},
				{"n":"Subs","e":[{"n":"ep1.srt","s":2,"l":"https://alldebrid.com/f/bbb"}]}
			]},
			{"n":"readme.txt","s":1,"l":"https://alldebrid.com/f/ccc"}
		]}]}}`))
	})

	files, err := p.Files(context.Background(), "1")
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	want := map[string]int64{
		"Season 1/ep1.mkv":      10,
		"Season 1/Subs/ep1.srt": 2,
		"readme.txt":            1,
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(files), len(want), files)
	}
	for _, f := range files {
		size, ok := want[f.Path]
		if !ok {
			t.Errorf("unexpected file path %q", f.Path)
			continue
		}
		if f.SizeBytes != size {
			t.Errorf("%s size = %d, want %d", f.Path, f.SizeBytes, size)
		}
		// The file id is the locked link, which is what unlocking needs.
		if f.ProviderFileID == "" {
			t.Errorf("%s has no provider file id", f.Path)
		}
	}
}

// RequestDownloadLink is passed the locked link as the file id, since
// AllDebrid has no separate per-file identifier.
func TestRequestDownloadLink_UnlocksTheFileLink(t *testing.T) {
	var sentLink string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sentLink = r.PostForm.Get("link")
		w.Write([]byte(`{"status":"success","data":{"link":"https://cdn.example/dl/file.mp4","filename":"file.mp4"}}`))
	})

	got, err := p.RequestDownloadLink(context.Background(), "1", "https://alldebrid.com/f/aaa")
	if err != nil {
		t.Fatalf("RequestDownloadLink() error = %v", err)
	}
	if sentLink != "https://alldebrid.com/f/aaa" {
		t.Errorf("unlocked %q, want the locked file link", sentLink)
	}
	if got != "https://cdn.example/dl/file.mp4" {
		t.Errorf("link = %q", got)
	}
}

// CheckCached answers from the account, since AllDebrid removed its
// instant-availability endpoint. A hash that is present and Ready is
// cached; anything else reports false rather than guessing.
func TestCheckCached_AnswersFromTheAccount(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"magnets":[
			{"id":1,"hash":"AAA","statusCode":4,"filename":"ready"},
			{"id":2,"hash":"BBB","statusCode":1,"filename":"still downloading"}
		]}}`))
	})

	got, err := p.CheckCached(context.Background(), []string{"aaa", "bbb", "ccc"})
	if err != nil {
		t.Fatalf("CheckCached() error = %v", err)
	}
	if !got["aaa"] {
		t.Error("a Ready magnet should report cached")
	}
	if got["bbb"] {
		t.Error("a still-downloading magnet should not report cached")
	}
	if got["ccc"] {
		t.Error("a hash absent from the account should not report cached")
	}
}

// Unsupported capabilities report an error rather than faking something
// locally — see debrid.TorrentProvider's own guidance.
func TestUnsupportedCapabilitiesError(t *testing.T) {
	p := NewProvider("alldebrid", "k")
	if _, err := p.AddTorrentFile(context.Background(), "x.torrent", nil, debrid.AddOptions{}); err == nil {
		t.Error("AddTorrentFile() error = nil, want an explicit not-supported error")
	}
	if _, err := p.RequestZipDownloadLink(context.Background(), "1"); err == nil {
		t.Error("RequestZipDownloadLink() error = nil, want an explicit not-supported error")
	}
}

// TestAccount_MapsPlanTiers covers the tier labels, which have to be
// checked most-specific-first: an Ultimate account is also premium, so
// checking premium first would flatten every tier into one label.
func TestAccount_MapsPlanTiers(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"isUltimate":true,"isTrial":true}`, "Ultimate (trial)"},
		{`{"isUltimate":true}`, "Ultimate"},
		{`{"isPremium":true,"isTrial":true}`, "Premium (trial)"},
		{`{"isPremium":true}`, "Premium"},
		{`{}`, "Free"},
	}
	for _, c := range cases {
		p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":"success","data":{"user":` + c.body + `}}`))
		})
		got, err := p.Account(context.Background())
		if err != nil {
			t.Fatalf("Account() error = %v", err)
		}
		if got.PlanName != c.want {
			t.Errorf("plan for %s = %q, want %q", c.body, got.PlanName, c.want)
		}
	}
}

// No key configured is debrid.ErrNoProvider, which the API layer maps to a
// 503 rather than a generic upstream failure.
func TestNoAPIKeyIsErrNoProvider(t *testing.T) {
	p := NewProvider("alldebrid", "")
	if _, err := p.List(context.Background()); !errors.Is(err, debrid.ErrNoProvider) {
		t.Errorf("List() error = %v, want ErrNoProvider", err)
	}
}

// The agent parameter is required by AllDebrid on every call; without it
// requests are rejected outright.
func TestEveryRequestSendsAgent(t *testing.T) {
	var seen bool
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		seen = r.URL.Query().Get("agent") != "" || r.PostForm.Get("agent") != ""
		w.Write([]byte(`{"status":"success","data":{"magnets":[]}}`))
	})
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if !seen {
		t.Error("no agent parameter was sent")
	}
}

// A 5xx from a proxy or gateway never parses as the envelope; reporting it
// as a decode failure would obscure what actually happened.
func TestUpstream5xxIsReportedAsSuch(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>gateway error</html>"))
	})
	_, err := p.List(context.Background())
	if err == nil {
		t.Fatal("List() error = nil for a 502")
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		t.Errorf("error = %v, want an upstream-status error rather than a JSON decode failure", err)
	}
}

// TestStatus_AcceptsBothMagnetShapes is the regression for a bug that
// silently disabled the fast per-download poll for AllDebrid: "magnets" is
// an array when listing, but a bare object when the query is filtered to a
// single id. Assuming the array shape made every Status call fail to
// decode, which looked like nothing at all — the bulk list still worked, so
// downloads still progressed, just a whole interval slower than they should
// have. Confirmed against the live API; not something the docs mention.
func TestStatus_AcceptsBothMagnetShapes(t *testing.T) {
	const object = `{"status":"success","data":{"magnets":{"id":703365472,"filename":"Big Buck Bunny","size":10,"hash":"ABC","status":"Ready","statusCode":4}}}`
	const array = `{"status":"success","data":{"magnets":[{"id":703365472,"filename":"Big Buck Bunny","size":10,"hash":"ABC","status":"Ready","statusCode":4}]}}`

	for name, body := range map[string]string{"object (id-filtered)": object, "array (unfiltered)": array} {
		p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		})
		got, err := p.Status(context.Background(), "703365472")
		if err != nil {
			t.Errorf("%s: Status() error = %v", name, err)
			continue
		}
		if got.ID != "703365472" {
			t.Errorf("%s: id = %q, want 703365472", name, got.ID)
		}
		if got.State != debrid.StateCompleted {
			t.Errorf("%s: state = %v, want completed", name, got.State)
		}
	}
}

// A magnet that genuinely isn't there is an error, not a zero-valued
// status that would read as a real download.
func TestStatus_MissingMagnetIsAnError(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"magnets":[]}}`))
	})
	if _, err := p.Status(context.Background(), "nope"); err == nil {
		t.Error("Status() error = nil for a magnet that isn't on the account")
	}
}

// TestAddTorrentFile covers the endpoint's one real trap: it returns its
// results under "files", not the "magnets" every other magnet endpoint
// uses. Decoding the wrong key yields no entries and an add that silently
// looks empty rather than failing.
func TestAddTorrentFile(t *testing.T) {
	var gotPath, gotContentType, gotFilename string
	var gotFileBody []byte
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			if fh := r.MultipartForm.File["files[]"]; len(fh) > 0 {
				gotFilename = fh[0].Filename
				f, _ := fh[0].Open()
				gotFileBody, _ = io.ReadAll(f)
				f.Close()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"files": []map[string]any{
					{"id": 703365472, "hash": "dd8255ec", "name": "Big Buck Bunny", "size": 276445467, "ready": true},
				},
			},
		})
	})

	id, err := p.AddTorrentFile(context.Background(), "bbb.torrent", []byte("d8:announce..."), debrid.AddOptions{})
	if err != nil {
		t.Fatalf("AddTorrentFile() error = %v", err)
	}
	if id != "703365472" {
		t.Errorf("id = %q, want 703365472", id)
	}
	if gotPath != "/v4/magnet/upload/file" {
		t.Errorf("path = %q, want /v4/magnet/upload/file", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", gotContentType)
	}
	if gotFilename != "bbb.torrent" {
		t.Errorf("uploaded filename = %q, want bbb.torrent", gotFilename)
	}
	if string(gotFileBody) != "d8:announce..." {
		t.Errorf("uploaded body = %q, want the .torrent bytes", gotFileBody)
	}
}

// A per-file error rides inside a successful envelope, since the endpoint
// takes a batch — an unsuccessful add is not an unsuccessful request.
func TestAddTorrentFile_PerFileErrorIsReported(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"files": []map[string]any{
					{"error": map[string]any{"code": "MAGNET_INVALID_FILE", "message": "not a torrent"}},
				},
			},
		})
	})

	if _, err := p.AddTorrentFile(context.Background(), "bad.torrent", []byte("nope"), debrid.AddOptions{}); err == nil {
		t.Fatal("AddTorrentFile() error = nil, want the per-file error surfaced")
	}
}

// TestHTTP429IsRateLimited covers rate limiting signalled by status rather
// than by an error code in the envelope. AllDebrid caps requests at
// 12/second and 600/minute and answers 429 when either is crossed; a
// limiter sitting in front of the application may return that with no
// parseable body at all. Matching only on the body code would miss it, and
// internal/importer would keep hammering instead of backing off.
func TestHTTP429IsRateLimited(t *testing.T) {
	for _, body := range []string{
		``,                          // no body at all
		`<html>rate limited</html>`, // an HTML page from a proxy
		`{"status":"error","error":{"code":"SOMETHING_ELSE","message":"x"}}`,
	} {
		p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(body))
		})

		_, err := p.List(context.Background())
		if err == nil {
			t.Fatalf("List() error = nil for a 429 with body %q", body)
		}
		if !errors.Is(err, debrid.ErrRateLimited) {
			t.Errorf("429 with body %q gave %v, want it to unwrap to ErrRateLimited", body, err)
		}
	}
}

// Quota errors are deliberately NOT rate limits: backing off would hide them
// behind a silent pause and a "rate-limited until" banner, when they are
// things only the account holder can resolve.
func TestQuotaErrorsAreNotRateLimits(t *testing.T) {
	for _, code := range []string{"FREE_TRIAL_LIMIT_REACHED", "MUST_BE_PREMIUM", "LINK_HOST_LIMIT_REACHED", "AUTH_BAD_APIKEY"} {
		p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"status": "error",
				"error":  map[string]any{"code": code, "message": "nope"},
			})
		})

		_, err := p.List(context.Background())
		if err == nil {
			t.Fatalf("List() error = nil for %s", code)
		}
		if errors.Is(err, debrid.ErrRateLimited) {
			t.Errorf("%s unwrapped to ErrRateLimited — it would be hidden behind a backoff instead of shown", code)
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("%s: error text %q should name the code so it's actionable", code, err)
		}
	}
}

// A concurrency cap is a rate limit: it clears on its own as transfers
// finish, so waiting is exactly the right response.
func TestTooManyActiveMagnetsIsRateLimited(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "error",
			"error":  map[string]any{"code": "MAGNET_TOO_MANY_ACTIVE", "message": "too many active"},
		})
	})

	if _, err := p.List(context.Background()); !errors.Is(err, debrid.ErrRateLimited) {
		t.Errorf("MAGNET_TOO_MANY_ACTIVE gave %v, want ErrRateLimited", err)
	}
}
