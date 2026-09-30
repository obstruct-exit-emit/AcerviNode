package torbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cloudflare502 is the shape of what an edge in front of TorBox returns during
// an outage: a full HTML page, several KB, not JSON.
var cloudflare502 = `<!DOCTYPE html>
<html lang="en-US">
<head>
<title>api.torbox.app | 502: Bad gateway</title>
<meta charset="UTF-8" />
<style>` + strings.Repeat("body{margin:0;padding:0}\n", 200) + `</style>
</head>
<body><div class="cf-error-details"><h1>Bad gateway</h1>
<p>The web server reported a bad gateway error.</p></div></body></html>`

func errorFor(t *testing.T, status int, contentType, body string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	c := NewClient("test-api-key", WithBaseURL(server.URL))
	return c.doGet(context.Background(), "/anything", nil, nil)
}

// TestAPIError_HTMLBodyIsReducedToItsTitle.
//
// On a non-2xx with no JSON detail the client used the entire raw body as the
// error. During an outage that is a multi-KB HTML page, and it does not stay in
// a log line: importer.handleFailure stores the error as the download's
// error_message, and the SABnzbd shim sends that to Sonarr as fail_message —
// so every download retrying through an outage showed a wall of HTML in the
// *arr Activity view.
func TestAPIError_HTMLBodyIsReducedToItsTitle(t *testing.T) {
	err := errorFor(t, http.StatusBadGateway, "text/html", cloudflare502)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502 (still intact for anything branching on it)", apiErr.StatusCode)
	}
	if strings.Contains(apiErr.Detail, "<") || strings.Contains(apiErr.Detail, "\n") {
		t.Errorf("Detail = %q, want no markup and no newlines", apiErr.Detail)
	}
	if !strings.Contains(apiErr.Detail, "502: Bad gateway") {
		t.Errorf("Detail = %q, want the page's own title, which is the useful part", apiErr.Detail)
	}
	if len(apiErr.Detail) > maxErrorDetail+len("…") {
		t.Errorf("Detail is %d bytes, want it bounded by %d", len(apiErr.Detail), maxErrorDetail)
	}
}

// TestAPIError_PlainTextBodyIsKeptReadable — a short plain-text reason is
// exactly what the fallback exists to surface, and isUnsupportedHostDetail
// matches on prose, so it must come through intact rather than being treated
// like markup.
func TestAPIError_PlainTextBodyIsKeptReadable(t *testing.T) {
	const msg = "The site you are trying to download from is not supported."
	err := errorFor(t, http.StatusInternalServerError, "text/plain", "  "+msg+"\n")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.Detail != msg {
		t.Errorf("Detail = %q, want %q", apiErr.Detail, msg)
	}
	if !isUnsupportedHostDetail(apiErr.Detail) {
		t.Error("the unsupported-host classifier no longer recognises the message")
	}
}

// TestAPIError_JSONDetailIsUntouched pins the path real TorBox errors take.
func TestAPIError_JSONDetailIsUntouched(t *testing.T) {
	err := errorFor(t, http.StatusBadRequest, "application/json",
		`{"success":false,"detail":"Invalid torrent. Please check it and try again."}`)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.Detail != "Invalid torrent. Please check it and try again." {
		t.Errorf("Detail = %q, want the JSON detail verbatim", apiErr.Detail)
	}
}

// TestAPIError_LongPlainTextIsBounded — and a long non-HTML body is still cut
// down, on a rune boundary so a multi-byte character is never split.
func TestAPIError_LongPlainTextIsBounded(t *testing.T) {
	long := strings.Repeat("é", maxErrorDetail*2)
	err := errorFor(t, http.StatusInternalServerError, "text/plain", long)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if n := len([]rune(apiErr.Detail)); n > maxErrorDetail+1 {
		t.Errorf("Detail is %d runes, want at most %d plus an ellipsis", n, maxErrorDetail)
	}
	if !strings.HasSuffix(apiErr.Detail, "…") {
		t.Errorf("Detail = %q…, want an ellipsis marking the cut", apiErr.Detail[:20])
	}
	if !strings.HasPrefix(apiErr.Detail, "é") || strings.ContainsRune(apiErr.Detail, '�') {
		t.Error("truncation split a multi-byte character")
	}
}

// TestAPIError_EmptyBodyStillNamesTheStatus — nothing to summarise must not
// produce an empty, unhelpful message.
func TestAPIError_EmptyBodyStillNamesTheStatus(t *testing.T) {
	err := errorFor(t, http.StatusServiceUnavailable, "text/plain", "")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want it to name the status code", err)
	}
}
