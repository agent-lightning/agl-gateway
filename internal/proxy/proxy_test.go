package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiki/agl-gateway/internal/config"
	"github.com/kiki/agl-gateway/internal/keys"
	"github.com/kiki/agl-gateway/internal/pricing"
	"github.com/kiki/agl-gateway/internal/store"
)

func testPricing() *pricing.Table {
	return pricing.New([]config.ModelPricing{
		{Model: "gpt-5.4", InputCostPerToken: 2.5e-6, OutputCostPerToken: 1.5e-5, CacheReadInputTokenCost: 2.5e-7},
	})
}

// harness wires a Proxy to an in-memory store and an upstream test server.
func harness(t *testing.T, upstream http.Handler) (*Proxy, *store.Store, string) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}},
		Providers: []config.Provider{{Name: "up", BaseURL: srv.URL, APIKey: "upstream-secret"}},
	}
	p := New(cfg, st, testPricing(), srv.Client(), nil)

	plain, display, _ := keys.Generate()
	if _, err := st.CreateKey("dev", keys.Hash(plain), display, []string{"up"}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return p, st, plain
}

func do(t *testing.T, p *Proxy, key, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestForwardNonStreaming(t *testing.T) {
	var gotAuth, gotPath string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":400}}}`)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/chat/completions", `{"model":"gpt-5.4"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer upstream-secret" {
		t.Errorf("upstream Authorization = %q, want upstream secret", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}

	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	l := logs[0]
	if l.Model != "gpt-5.4" || l.InputTokens != 600 || l.OutputTokens != 200 || l.CacheReadTokens != 400 {
		t.Errorf("unexpected log usage: %+v", l)
	}
	wantCost := 600*2.5e-6 + 200*1.5e-5 + 400*2.5e-7
	if l.Cost < wantCost-1e-12 || l.Cost > wantCost+1e-12 {
		t.Errorf("cost = %v, want %v", l.Cost, wantCost)
	}
	if l.Streaming {
		t.Error("expected non-streaming")
	}
}

func TestForwardStreamingSSE(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/chat/completions", `{"model":"gpt-5.4","stream":true}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("stream body not forwarded: %q", rec.Body.String())
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || !logs[0].Streaming {
		t.Fatalf("expected 1 streaming log, got %+v", logs)
	}
	if logs[0].InputTokens != 10 || logs[0].OutputTokens != 5 {
		t.Errorf("stream usage = %+v", logs[0])
	}
}

func TestRetryOnServerErrorThenSuccess(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after retries", rec.Code)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("upstream calls = %d, want 3 (2 failures + success)", got)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != 200 {
		t.Errorf("expected one 200 log, got %+v", logs)
	}
}

func TestRetryExhaustedReturnsUpstreamError(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	// MaxRetries=3 -> 4 total attempts, final 502 passed through.
	if got := calls.Load(); got != 4 {
		t.Errorf("upstream calls = %d, want 4", got)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != http.StatusBadGateway {
		t.Errorf("expected one 502 log, got %+v", logs)
	}
}

func TestNetworkErrorReturns502(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}},
		// Unroutable upstream: connection refused.
		Providers: []config.Provider{{Name: "dead", BaseURL: "http://127.0.0.1:1"}},
	}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	p := New(cfg, st, testPricing(), client, nil)
	plain, display, _ := keys.Generate()
	st.CreateKey("dev", keys.Hash(plain), display, []string{"dead"})

	rec := do(t, p, plain, "/v1/x", `{"model":"gpt-5.4"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].Error == "" {
		t.Errorf("expected one error log, got %+v", logs)
	}
}

func TestAuthFailures(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	p, _, _ := harness(t, up)

	if rec := do(t, p, "", "/v1/x", `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing key status = %d, want 401", rec.Code)
	}
	if rec := do(t, p, "sk-gw-bogus", "/v1/x", `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad key status = %d, want 401", rec.Code)
	}
}

func TestUnknownProviderNamesIgnored(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	p, st, _ := harness(t, up)
	// Key bound to one bogus and one real provider -> must route to the real one.
	plain, display, _ := keys.Generate()
	st.CreateKey("mix", keys.Hash(plain), display, []string{"ghost", "up"})

	if rec := do(t, p, plain, "/v1/x", `{"model":"gpt-5.4"}`); rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
