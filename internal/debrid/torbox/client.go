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
	"time"
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

// Client is a low-level TorBox API client: one HTTP call in, one decoded
// response out. It knows nothing about debrid.TorrentProvider or
// debrid.UsenetProvider — see provider.go and usenet_provider.go for the
// adapters that translate between the two.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient builds a Client authenticated with apiKey against the real
// TorBox API. Use the With* options to point it at a test server instead.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:    defaultBaseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
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
	resp, err := c.httpClient.Do(req)
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
