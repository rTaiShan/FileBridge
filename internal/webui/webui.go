// Package webui serves the local, read-only FileBridge dashboard.
package webui

import (
	_ "embed"
	"net/http"
)

// dashboard is intentionally a single embedded document so the client remains
// a self-contained executable with no static files to install alongside it.
//
//go:embed dashboard.html
var dashboard []byte

//go:embed favicon.svg
var favicon []byte

// Handler serves the dashboard from /_bridge/ui/.
func Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/_bridge/ui/favicon.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(favicon)
		return
	case "/_bridge/ui/":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(dashboard)
}
