package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/keys"
	"github.com/agent-lightning/agl-gateway/internal/pricing"
	"github.com/agent-lightning/agl-gateway/internal/store"
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

func TestModelMappingRewritesAndLogs(t *testing.T) {
	var gotModel string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		gotModel = b.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 0, BaseDelay: time.Millisecond}},
		Providers: []config.Provider{{
			Name: "up", BaseURL: srv.URL, APIKey: "x",
			ModelMap: map[string]string{"alias": "gpt-5.4"},
		}},
	}
	p := New(cfg, st, testPricing(), srv.Client(), nil)
	plain, display, _ := keys.Generate()
	st.CreateKey("dev", keys.Hash(plain), display, []string{"up"})

	rec := do(t, p, plain, "/v1/chat/completions", `{"model":"alias","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotModel != "gpt-5.4" {
		t.Errorf("upstream received model %q, want gpt-5.4 (mapped)", gotModel)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].Model != "alias" || logs[0].MappedModel != "gpt-5.4" {
		t.Fatalf("log model/mapped = %+v", logs[0])
	}
	// Requested "alias" is unpriced here, so cost falls back to the mapped model's price.
	if logs[0].Cost == 0 {
		t.Error("expected non-zero cost via mapped-model fallback pricing")
	}
}

func TestPricingPrefersRequestedAlias(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1000,"completion_tokens":0}}`)
	})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	// Both the requested alias and the mapped model are priced, at different rates.
	prices := pricing.New([]config.ModelPricing{
		{Model: "alias", InputCostPerToken: 1e-6},
		{Model: "gpt-5.4", InputCostPerToken: 9e-6},
	})
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 0}},
		Providers: []config.Provider{{Name: "up", BaseURL: srv.URL, APIKey: "x",
			ModelMap: map[string]string{"alias": "gpt-5.4"}}},
	}
	p := New(cfg, st, prices, srv.Client(), nil)
	plain, display, _ := keys.Generate()
	st.CreateKey("dev", keys.Hash(plain), display, []string{"up"})

	do(t, p, plain, "/v1/x", `{"model":"alias","messages":[]}`)
	logs, _ := st.QueryLogs(store.LogFilter{})
	// 1000 input tokens billed at the alias rate (1e-6), not the mapped rate (9e-6).
	if want := 1000 * 1e-6; logs[0].Cost < want-1e-12 || logs[0].Cost > want+1e-12 {
		t.Errorf("cost = %v, want %v (requested-alias rate)", logs[0].Cost, want)
	}
}

func TestAttemptsLoggedAndHeaders(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(headerAttempts); got != "3" {
		t.Errorf("X-AGL-Attempts = %q, want 3", got)
	}
	if got := rec.Header().Get(headerProvider); got != "up" {
		t.Errorf("X-AGL-Provider = %q, want up", got)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if logs[0].Attempts != 3 {
		t.Errorf("logged attempts = %d, want 3", logs[0].Attempts)
	}
}

func TestProviderFailureErrorIsClear(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}},
		Providers: []config.Provider{{Name: "dead", BaseURL: "http://127.0.0.1:1"}},
	}
	p := New(cfg, st, testPricing(), &http.Client{Timeout: 500 * time.Millisecond}, nil)
	plain, display, _ := keys.Generate()
	st.CreateKey("dev", keys.Hash(plain), display, []string{"dead"})

	rec := do(t, p, plain, "/v1/x", `{"model":"gpt-5.4"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if rec.Header().Get(headerErrorSource) != sourceProvider {
		t.Errorf("error source = %q, want provider", rec.Header().Get(headerErrorSource))
	}
	if rec.Header().Get(headerAttempts) != "3" {
		t.Errorf("attempts header = %q, want 3", rec.Header().Get(headerAttempts))
	}
	var body struct {
		Error struct {
			Message  string `json:"message"`
			Source   string `json:"source"`
			Provider string `json:"provider"`
			Attempts int    `json:"attempts"`
		} `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Source != sourceProvider || body.Error.Attempts != 3 || body.Error.Provider != "dead" {
		t.Errorf("error body = %+v", body.Error)
	}
	if !strings.Contains(body.Error.Message, "3 attempt") || !strings.Contains(body.Error.Message, "dead") {
		t.Errorf("message not clear: %q", body.Error.Message)
	}
	// Logged with the failure reason and attempt count.
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].Attempts != 3 || logs[0].Error == "" {
		t.Errorf("failure log = %+v", logs)
	}
}

func TestUpstreamErrorMarkedProviderSource(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"message":"upstream exploded","type":"server_error"}}`)
	})
	p, st, key := harness(t, up)
	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get(headerErrorSource) != sourceProvider {
		t.Errorf("error source = %q, want provider", rec.Header().Get(headerErrorSource))
	}
	// The upstream's own error body must be recorded, not just the generic summary.
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 {
		t.Fatalf("logs = %d", len(logs))
	}
	if !strings.Contains(logs[0].Error, "upstream exploded") {
		t.Errorf("logged error missing upstream body: %q", logs[0].Error)
	}
	if !strings.Contains(logs[0].Error, "HTTP 502") {
		t.Errorf("logged error missing status summary: %q", logs[0].Error)
	}
}

func TestProviderSelectionHeader(t *testing.T) {
	// Two upstreams; a key bound to both. The X-AGL-Provider header must pin the choice,
	// and the header must not leak to the upstream.
	var aHits, bHits atomic.Int32
	var gotControlHeader string
	mk := func(hits *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if v := r.Header.Get("X-AGL-Provider"); v != "" {
				gotControlHeader = v
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		})
	}
	srvA := httptest.NewServer(mk(&aHits))
	srvB := httptest.NewServer(mk(&bHits))
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 0}},
		Providers: []config.Provider{
			{Name: "a", BaseURL: srvA.URL, APIKey: "x"},
			{Name: "b", BaseURL: srvB.URL, APIKey: "x"},
		},
	}
	p := New(cfg, st, testPricing(), srvA.Client(), nil)
	plain, display, _ := keys.Generate()
	st.CreateKey("dev", keys.Hash(plain), display, []string{"a", "b"})

	send := func(provider string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(`{"model":"gpt-5.4"}`))
		req.Header.Set("Authorization", "Bearer "+plain)
		if provider != "" {
			req.Header.Set("X-AGL-Provider", provider)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// Pin provider "b": only B should be hit, and it must not see the control header.
	for i := 0; i < 5; i++ {
		if rec := send("b"); rec.Code != 200 {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
	}
	if aHits.Load() != 0 || bHits.Load() != 5 {
		t.Errorf("hits a=%d b=%d, want a=0 b=5", aHits.Load(), bHits.Load())
	}
	if gotControlHeader != "" {
		t.Errorf("upstream saw control header X-AGL-Provider=%q, want it stripped", gotControlHeader)
	}

	// An unbound provider is a 400 gateway error.
	rec := send("ghost")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unbound provider status = %d, want 400", rec.Code)
	}
	if rec.Header().Get(headerErrorSource) != sourceGateway {
		t.Errorf("error source = %q, want gateway", rec.Header().Get(headerErrorSource))
	}
}

func TestUpstream4xxBodyCaptured(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model `+"`gpt-5.5`"+` is not supported","code":"model_not_found"}}`)
	})
	p, st, key := harness(t, up)
	rec := do(t, p, key, "/v1/chat/completions", `{"model":"gpt-5.5"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	// Client still receives the upstream body verbatim.
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Errorf("client body = %q", rec.Body.String())
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if !strings.Contains(logs[0].Error, "model_not_found") {
		t.Errorf("logged error missing upstream reason: %q", logs[0].Error)
	}
}
