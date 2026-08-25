// Package alldebrid implements debrid.TorrentProvider against AllDebrid's
// v4 API.
//
// Torrent-only, deliberately. AllDebrid does debrid hoster links, but
// through /v4/link/unlock — a synchronous call that hands back a direct URL
// with no pollable object behind it. AcerviNode's web-download model is
// "add, then track it until it's ready", and there is nothing on
// AllDebrid's side to track: a saved link is either unlockable or it isn't.
// Supporting webdl here would mean inventing that lifecycle locally, which
// is guesswork rather than integration, so this registers no web-download
// capability at all (see cmd/acervinode's knownProviderCapabilities). It
// has no usenet service of any kind.
//
// Two shapes here differ from TorBox and are worth knowing before reading
// the rest:
//
//   - Every response is wrapped in {"status": "success"|"error", ...}, and
//     an error still arrives as HTTP 200. Nothing can be concluded from the
//     status code alone — see decode.
//   - A magnet's files are not part of its status. Listing is one call
//     (/v4.1/magnet/status) and files are another (/v4/magnet/files), and a
//     file's "link" is itself locked: it has to go through
//     /v4/link/unlock before anything can be downloaded from it.
package alldebrid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

const (
	defaultBaseURL = "https://api.alldebrid.com"
	// agent identifies the calling application. AllDebrid requires it on
	// every request and rejects calls without one.
	agent = "acervinode"
)

// defaultRequestTimeout matches internal/debrid/torbox's own default, and is
// overridden per-client by WithRequestTimeout so the live
// provider_request_timeout_seconds setting applies to every provider alike.
const defaultRequestTimeout = 30 * time.Second

// APIError is an AllDebrid application-level failure — status "error" in the
// envelope, which arrives as HTTP 200. Code is AllDebrid's own machine
// -readable string (e.g. "MAGNET_INVALID_ID", "AUTH_BAD_APIKEY").
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("alldebrid: %s (%s)", e.Message, e.Code)
}

// Unwrap maps AllDebrid's own rate-limit codes onto debrid.ErrRateLimited,
// so internal/importer backs off for this provider exactly as it does for
// any other without knowing anything about AllDebrid — see
// docs/providers.md. Every other code ends the chain here.
func (e *APIError) Unwrap() error {
	switch e.Code {
	case rateLimitedCode, "TOO_MANY_REQUESTS", "MAGNET_TOO_MANY_ACTIVE":
		return debrid.ErrRateLimited
	case "LINK_HOST_NOT_SUPPORTED", "LINK_HOST_UNAVAILABLE":
		// A clean, stable code — AllDebrid's supported set is small and
		// plan-dependent (five hosts on a trial), so this is an ordinary
		// answer rather than an edge case.
		return debrid.ErrHostNotSupported
	}
	return nil
}

// rateLimitedCode is what send records for an HTTP 429, so a status-level
// rate limit and a body-level one produce the same error to callers.
//
// Deliberately narrow: AllDebrid's quota errors (FREE_TRIAL_LIMIT_REACHED,
// MUST_BE_PREMIUM, LINK_HOST_LIMIT_REACHED) are *not* mapped here. Backing
// off would hide them behind a silent pause and a "rate-limited until"
// banner, when they are things only the account holder can resolve — much
// better surfaced as the error text on the download itself.
const rateLimitedCode = "HTTP_429"

// MAGNET_TOO_MANY_ACTIVE is mapped, by contrast, because it is a
// concurrency cap (30 active magnets) that clears on its own as transfers
// finish — waiting is exactly the right response.

// Client is a low-level AllDebrid API client: one call in, one decoded
// response out. It knows nothing about debrid.TorrentProvider — see
// provider.go for the adapter.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// Option customises a Client at construction.
type Option func(*Client)

// WithRequestTimeout bounds every HTTP call this client makes.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithBaseURL points the client at a different host — for tests.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// NewClient builds a client for apiKey.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: defaultRequestTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// envelope is the wrapper every AllDebrid response arrives in.
type envelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// doUpload performs one multipart file upload and decodes data into out —
// the only request shape `do` can't express, since AllDebrid's .torrent
// endpoint takes a real file part rather than a form field.
func (c *Client) doUpload(ctx context.Context, path, fieldName, filename string, data []byte, out any) error {
	if c.apiKey == "" {
		return debrid.ErrNoProvider
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(fieldName, filename)
	if err != nil {
		return fmt.Errorf("alldebrid upload: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("alldebrid upload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("alldebrid upload: %w", err)
	}

	// agent goes in the query string here: it is a normal request
	// parameter, and putting it in the multipart body alongside the file
	// would be a second place to get it wrong.
	endpoint := c.baseURL + path + "?agent=" + url.QueryEscape(agent)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("alldebrid upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	return c.send(req, out)
}

// do performs one request and decodes data into out.
//
// form is sent as an application/x-www-form-urlencoded body for POST, or as
// the query string for GET. Either way the agent parameter is added here so
// no caller has to remember it.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	if c.apiKey == "" {
		return debrid.ErrNoProvider
	}
	if form == nil {
		form = url.Values{}
	}
	form.Set("agent", agent)

	endpoint := c.baseURL + path
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form.Encode())
	} else {
		endpoint += "?" + form.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("alldebrid request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	return c.send(req, out)
}

// send performs req and decodes the envelope into out — shared by do and
// doUpload so both get identical error handling.
func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("alldebrid request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("alldebrid read response: %w", err)
	}
	// 503 is how AllDebrid actually sheds load. Confirmed live: 80
	// concurrent requests produced no 429s at all but 21 nginx "503 Service
	// Temporarily Unavailable" pages — its edge turns requests away before
	// they ever reach the application, so the response is HTML rather than
	// the usual envelope and carries no error code to match on. Treated as
	// a rate limit so internal/importer backs off, which is the right
	// response whether the cause is load shedding or a brief outage:
	// either way the answer is to stop asking for a while.
	//
	// Narrower than "any 5xx" on purpose — 500/502/504 read more like a
	// genuine fault than a deliberate "not now", and reporting those as a
	// rate limit would put a misleading "rate-limited until" banner in
	// front of a real outage.
	if resp.StatusCode == http.StatusServiceUnavailable {
		return &APIError{Code: rateLimitedCode, Message: "service temporarily unavailable"}
	}
	// A transport-level failure is still worth distinguishing: an HTML
	// error page or a 502 from a proxy never parses as the envelope, and
	// "unexpected end of JSON" would be a poor way to report it.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("alldebrid: upstream returned status %d", resp.StatusCode)
	}
	// Rate limiting is signalled by HTTP status, not only by an error code
	// in the envelope: AllDebrid caps requests at 12/second and 600/minute
	// and answers 429 when you cross either. Relying on the body alone
	// would miss a 429 that carries no code — or no parseable body at all,
	// which is exactly what a limiter in front of the application tends to
	// return — and internal/importer would keep hammering instead of
	// backing off. Checked before parsing so the shape of the body can't
	// affect whether the backoff happens.
	if resp.StatusCode == http.StatusTooManyRequests {
		return &APIError{Code: rateLimitedCode, Message: "too many requests"}
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("alldebrid decode response: %w", err)
	}
	if env.Status != "success" {
		if env.Error != nil {
			return &APIError{Code: env.Error.Code, Message: env.Error.Message}
		}
		return fmt.Errorf("alldebrid: request failed with no error detail")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("alldebrid decode data: %w", err)
	}
	return nil
}
