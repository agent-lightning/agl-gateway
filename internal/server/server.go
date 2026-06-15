// Package server composes the gateway's HTTP surface: the master-key control plane under
// /admin/, an optional inspection portal under /portal, a health check, and the catch-all
// data-plane proxy for everything else.
package server

import (
	"net/http"

	"github.com/kiki/agl-gateway/internal/admin"
	"github.com/kiki/agl-gateway/internal/proxy"
)

// New returns the top-level HTTP handler. portal may be nil to disable the web portal.
func New(p *proxy.Proxy, a *admin.Admin, portal http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Control plane (more specific than "/", so it wins for /admin/* paths).
	mux.Handle("/admin/", a.Handler())

	if portal != nil {
		mux.Handle("/portal", portal)
		mux.Handle("/portal/", portal)
	}

	// Data plane: everything else is proxied based on the inbound API key.
	mux.Handle("/", p)

	return mux
}
