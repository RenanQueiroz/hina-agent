// Package webui embeds the built web client (web/dist) and serves it as a
// single-page app: real assets are served directly; any other path falls back
// to index.html so client-side routing (/login, /admin, …) works on reload.
//
// web/dist is committed so `go build ./...` and CI work without a Node build;
// run `npm --prefix web run build` to refresh it.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler serving the embedded SPA.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	index, _ := fs.ReadFile(sub, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, statErr := fs.Stat(sub, p); statErr == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}
