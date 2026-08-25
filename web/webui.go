// Package web embeds the built frontend (this directory's own dist/,
// produced by `npm run build` — see docs/development.md) into the Go
// binary. A committed dist/.gitkeep keeps `go build` working on a fresh
// clone before the frontend has ever been built; Handler simply serves
// nothing useful at "/" until a real build replaces it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded frontend as a single-page app: real static
// assets are served directly, and any other path falls back to index.html
// so client-side routing survives a hard refresh or a deep link.
//
// Caching is the load-bearing part. Vite fingerprints every asset, so
// index.html is the only file whose *contents* change at a fixed URL —
// which makes it the one file a browser must never reuse without asking.
// Left to its own devices a browser applies heuristic caching to a response
// with no cache headers, so a deployed change simply wouldn't arrive: the
// stale index would go on requesting the previous bundle indefinitely.
// Observed exactly that after a deploy.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err != nil {
			// A missing *asset* is a genuine 404, not a route. Falling back
			// to index.html here handed the browser HTML with a JavaScript
			// file's name: it fails on a MIME mismatch rather than a clear
			// "this build no longer has that file", which is exactly the
			// wrong answer for a stale page asking for a bundle that has
			// been replaced.
			if isAssetPath(path) {
				http.NotFound(w, r)
				return
			}
			req := r.Clone(r.Context())
			req.URL.Path = "/"
			setCacheHeaders(w, "index.html")
			fileServer.ServeHTTP(w, req)
			return
		}
		setCacheHeaders(w, path)
		fileServer.ServeHTTP(w, r)
	}), nil
}

// isAssetPath reports whether a path is asking for a build artefact rather
// than a client-side route. Vite emits everything fingerprinted under
// assets/, and routes never carry a file extension.
func isAssetPath(path string) bool {
	return strings.HasPrefix(path, "assets/") || strings.Contains(pathBase(path), ".")
}

// pathBase is the last segment of path, without importing path/filepath for
// one call on an already-slash-separated URL path.
func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// setCacheHeaders splits the two kinds of file this serves.
//
// Fingerprinted assets can be cached hard and forever: their name changes
// whenever their contents do, so a cached copy is never wrong. index.html
// cannot be cached at all — its URL is fixed while its contents change every
// build, and it is what points at the current fingerprints. "no-cache" still
// allows a conditional request, so an unchanged index costs a 304 rather
// than a full download.
func setCacheHeaders(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
