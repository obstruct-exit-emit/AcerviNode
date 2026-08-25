package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ServesIndexAtRoot(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Errorf("body doesn't look like the built index.html: %q", rec.Body.String())
	}
}

func TestHandler_FallsBackToIndexForUnknownPath(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/client/side/route", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Errorf("fallback body doesn't look like index.html: %q", rec.Body.String())
	}
}

// TestHandler_IndexIsNotCached is why a deployed change reaches anyone.
//
// Vite fingerprints every asset, so index.html is the only file whose
// contents change at a fixed URL — and it is what points at the current
// fingerprints. With no cache headers a browser applies heuristic caching to
// it, so a stale index goes on requesting the previous bundle indefinitely
// and the deploy silently never arrives. Observed exactly that: a UI change
// was live on the server and invisible in the browser.
func TestHandler_IndexIsNotCached(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	for _, path := range []string{"/", "/settings", "/some/deep/route"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
	}
}

// TestHandler_FingerprintedAssetsAreCachedHard is the other half: a file
// whose name changes with its contents can be cached forever, and should be,
// or every navigation re-downloads the whole bundle.
func TestHandler_FingerprintedAssetsAreCachedHard(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	assets, err := fs.Glob(distFS, "dist/assets/*")
	if err != nil || len(assets) == 0 {
		t.Skip("no built assets embedded; run npm run build")
	}
	target := "/" + strings.TrimPrefix(assets[0], "dist/")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d, want 200", target, rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("%s Cache-Control = %q, want it cached immutably", target, got)
	}
}

// TestHandler_MissingAssetIs404NotIndex proves a bundle that no longer
// exists says so.
//
// The SPA fallback used to answer these with index.html, handing the browser
// HTML under a .js name — which fails on a MIME mismatch instead of a clear
// "that file is gone". A stale page asking for a replaced bundle deserves
// the honest answer, and a 404 is also what tells a client its index is out
// of date.
func TestHandler_MissingAssetIs404NotIndex(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	for _, path := range []string{
		"/assets/index-OLDHASH.js",
		"/assets/index-OLDHASH.css",
		"/favicon-that-never-existed.svg",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 (got content-type %q)", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// TestHandler_RoutesStillFallBackToIndex guards the fix from going too far:
// a client-side route has no file extension and must still serve the app,
// or a deep link or hard refresh would 404.
func TestHandler_RoutesStillFallBackToIndex(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	for _, path := range []string{"/settings", "/downloads", "/deeply/nested/route"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 — client-side routes must serve the app", path, rec.Code)
		}
	}
}
