// Package portal serves a single-page, master-key-protected inspection and management UI.
package portal

import (
	"embed"
	"net/http"
)

//go:embed index.html
var assets embed.FS

// Handler serves the portal SPA at /portal.
func Handler() http.Handler {
	page, _ := assets.ReadFile("index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
}
