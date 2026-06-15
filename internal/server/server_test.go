package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-lightning/agl-gateway/internal/admin"
	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/pricing"
	"github.com/agent-lightning/agl-gateway/internal/proxy"
	"github.com/agent-lightning/agl-gateway/internal/store"
)

func TestRouting(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 0}},
		Providers: []config.Provider{{Name: "up", BaseURL: "http://127.0.0.1:1"}},
	}
	prices := pricing.New(nil)
	p := proxy.New(cfg, st, prices, nil, nil)
	a := admin.New(cfg, st, nil)

	portal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("PORTAL"))
	})
	h := New(p, a, portal)

	// Health check.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}

	// Admin requires master key.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/keys", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("admin without key = %d, want 401", rec.Code)
	}

	// Portal mounted.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/portal", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "PORTAL" {
		t.Errorf("portal = %d %q", rec.Code, rec.Body.String())
	}

	// Data-plane catch-all hits the proxy, which rejects a missing API key.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("proxy without key = %d, want 401", rec.Code)
	}
}
