// Package torbox is a thin client for the TorBox API
// (https://api.torbox.app), covering the torrent and usenet endpoints needed
// to implement debrid.TorrentProvider and debrid.UsenetProvider. Endpoint
// paths and field names are cross-checked against the official Go SDK
// (https://github.com/TorBox-App/torbox-sdk-go) rather than guessed; see
// docs/providers.md for the endpoint list.
package torbox

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
	defaultBaseURL = "https://api.torbox.app"
	apiVersion     = "v1"
)

// APIError is returned for both HTTP-level failures (non-2xx status) and
// application-level failures (200 OK but "success": false in the envelope).
type APIError struct {
	StatusCode int
	Detail     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("torbox: %s (status %d)", e.Detail, e.StatusCode)
}

// Unwrap lets errors.Is(err, debrid.ErrRateLimited) recognize a 429
// specifically, through however many fmt.Errorf("...: %w", err) layers a
// caller wraps this in (every provider adapter method here does exactly
// that) — internal/importer uses this to back off its own polling for a
// rate limit specifically, without needing to import this package or know
// about *APIError at all (see docs/providers.md). Every other status code
// unwraps to nil, ending the chain there — this is deliberately the only
// distinction *APIError itself exposes; anything more specific than "was
// this a rate limit" is still available via a type assertion to *APIError
// for a caller that actually needs it (see client_test.go).
func (e *APIError) Unwrap() error {
	if e.StatusCode == http.StatusTooManyRequests {
		return debrid.ErrRateLimited
	}
	if isUnsupportedHostDetail(e.Detail) {
		return debrid.ErrHostNotSupported
	}
	return nil
}

// isUnsupportedHostDetail recognises TorBox's "this file host isn't one of
// ours" rejection, which arrives as a plain 500 with the reason only in the
// message: "The site you are trying to download from is not supported."
//
// Matching on prose is fragile and deliberately narrow because of it —
// broad matching would swallow unrelated failures into a sentinel that
// makes callers stop retrying. If TorBox ever reworks the wording this
// quietly stops classifying, which degrades to today's behaviour (a plain
// provider error) rather than misclassifying something else.
func isUnsupportedHostDetail(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "site you are trying to download from is not supported")
}

// defaultRequestTimeout is NewClient's starting value — see
// WithRequestTimeout and config.Config.ProviderRequestTimeoutSeconds, which
// is what actually keeps this live-changeable rather than fixed at
// construction (cmd/acervinode's liveSettings rebuilds a fresh Client via
// this option whenever the setting changes, the same way it already does
// for the API key itself).
const defaultRequestTimeout = 30 * time.Second

// Client is a low-level TorBox API client: one HTTP call in, one decoded
// response out. It knows nothing about debrid.TorrentProvider or
// debrid.UsenetProvider — see provider.go and usenet_provider.go for the
// adapters that translate between the two.
type Client struct {
	baseURL        string
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
}

// NewClient builds a Client authenticated with apiKey against the real
// TorBox API. Use the With* options to point it at a test server instead.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		// No client-wide Timeout — do() derives a per-request deadline from
		// requestTimeout instead, the same reasoning as
		// internal/importer.fetchFile's own per-request context.WithTimeout:
		// a *http.Client's own Timeout field is fixed at construction, and
		// this needs to be a fresh, possibly-different value on every Client
		// rebuild (a changed provider_request_timeout_seconds setting) without
		// requiring anything downstream to know the client was rebuilt.
		httpClient:     &http.Client{},
		requestTimeout: defaultRequestTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at a different base URL — used in tests to
// target an httptest.Server instead of the real TorBox API.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = baseURL }
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithRequestTimeout overrides how long a single TorBox API call (list,
// status, add, delete, account — every one of them, all funneled through
// do()) may run before being cancelled. Unlike internal/importer's own
// idle/stall fetch timeout, this is a plain total-request deadline: TorBox
// API responses are small JSON payloads, not multi-gigabyte files, so
// there's no legitimate "slow but actively trickling for 30+ seconds"
// scenario to protect against the way there is for a file download — a
// flat deadline is the right tool here.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.requestTimeout = d }
}

// searchBudget is how long TorBox itself should be allowed to spend on a
// call that does real work before answering (currently only torrentinfo,
// which searches the BitTorrent network for metadata).
//
// Deliberately a few seconds *under* our own request deadline. Both sides
// otherwise race to the same limit and ours usually wins, which replaces
// TorBox's own explanation of what went wrong with a bare "context deadline
// exceeded". Measured against the real API: a cold hash takes TorBox ~33s to
// give up on, comfortably past the 30s default here, so this is the ordinary
// path for any torrent TorBox hasn't seen before — not an edge case.
func (c *Client) searchBudget() int {
	const headroom = 5 * time.Second
	if c.requestTimeout <= headroom {
		// Too tight to reserve headroom from; let TorBox use its own
		// default rather than asking for something absurd.
		return 0
	}
	return int((c.requestTimeout - headroom).Seconds())
}

func (c *Client) url(path string) string {
	return c.baseURL + "/" + apiVersion + "/api" + path
}

// envelope matches the {success, detail, data} shape every TorBox JSON
// response uses.
type envelope[T any] struct {
	Success bool   `json:"success"`
	Detail  string `json:"detail"`
	Data    T      `json:"data"`
}

func (c *Client) doGet(ctx context.Context, path string, query url.Values, out any) error {
	u := c.url(path)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.do(req, out)
}

func (c *Client) doPostJSON(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.do(req, out)
}

// doPostForm POSTs as application/x-www-form-urlencoded — confirmed against
// TorBox's real OpenAPI spec that createwebdownload uses this (not multipart
// like createtorrent/createusenetdownload, which also accept a file upload;
// Web Downloads is link-only, nothing to upload).
func (c *Client) doPostForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.do(req, out)
}

// doMultipart POSTs fields as multipart form values, plus an optional file
// part (fileFieldName is ignored if fileData is empty) — the shape TorBox's
// createtorrent/createusenetdownload endpoints require for magnet/link vs.
// file uploads.
func (c *Client) doMultipart(ctx context.Context, path string, fields map[string]string, fileFieldName, filename string, fileData []byte, out any) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := writer.WriteField(k, v); err != nil {
			return fmt.Errorf("write field %s: %w", k, err)
		}
	}
	if len(fileData) > 0 {
		part, err := writer.CreateFormFile(fileFieldName, filename)
		if err != nil {
			return fmt.Errorf("create form file: %w", err)
		}
		if _, err := part.Write(fileData); err != nil {
			return fmt.Errorf("write form file: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	ctx, cancel := context.WithTimeout(req.Context(), c.requestTimeout)
	defer cancel()

	resp, err := c.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("torbox request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("torbox read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errEnv struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &errEnv)
		detail := errEnv.Detail
		if detail == "" {
			detail = string(data)
		}
		return &APIError{StatusCode: resp.StatusCode, Detail: detail}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("torbox decode response: %w", err)
	}
	return nil
}

// checkSuccess turns a 200-OK-but-"success":false envelope into an error.
func checkSuccess(success bool, detail string) error {
	if success {
		return nil
	}
	if detail == "" {
		detail = "unknown error"
	}
	return &APIError{StatusCode: http.StatusOK, Detail: detail}
}
