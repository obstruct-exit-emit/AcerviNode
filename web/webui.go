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
			req := r.Clone(r.Context())
			req.URL.Path = "/"
			fileServer.ServeHTTP(w, req)
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}
