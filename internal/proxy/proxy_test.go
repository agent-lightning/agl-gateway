package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/keys"
	"github.com/agent-lightning/agl-gateway/internal/pricing"
	"github.com/agent-lightning/agl-gateway/internal/store"
	"github.com/agent-lightning/agl-gateway/internal/usage"
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
	if _, err := st.CreateKey("dev", keys.Hash(plain), display, []string{"up"}, config.StartFirst, config.OrderRoundRobin); err != nil {
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
	if l.APIType != "openai_chat" {
		t.Errorf("api_type = %q, want openai_chat", l.APIType)
	}
	wantCost := 600*2.5e-6 + 200*1.5e-5 + 400*2.5e-7
	if l.Cost < wantCost-1e-12 || l.Cost > wantCost+1e-12 {
		t.Errorf("cost = %v, want %v", l.Cost, wantCost)
	}
	if l.Streaming {
		t.Error("expected non-streaming")
	}
	if l.RequestContentType != "application/json" || l.ResponseContentType != "application/json" {
		t.Errorf("content types = %q/%q", l.RequestContentType, l.ResponseContentType)
	}
	// Request-line metadata is recorded independently of payload capture.
	if l.Method != http.MethodPost || l.Path != "/v1/chat/completions" {
		t.Errorf("method/path = %q %q", l.Method, l.Path)
	}
	if l.ClientAddr == "" || l.RequestBytes != int64(len(`{"model":"gpt-5.4"}`)) {
		t.Errorf("client/request bytes = %q / %d", l.ClientAddr, l.RequestBytes)
	}
	if l.ResponseBytes == 0 {
		t.Errorf("response bytes = %d, want > 0", l.ResponseBytes)
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
	if logs[0].RequestContentType != "application/json" || logs[0].ResponseContentType != "text/event-stream" {
		t.Errorf("content types = %q/%q", logs[0].RequestContentType, logs[0].ResponseContentType)
	}
}

func TestPayloadCaptureStoresRawAndAssembledStream(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"chatcmpl_1","created":123,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	p, st, key := harness(t, up)
	p.cfg.PayloadCapture = config.PayloadCapture{Enabled: true, AssembleStreams: true}

	body := `{"model":"gpt-5.4","stream":true}`
	rec := do(t, p, key, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	logs, _ := st.QueryLogs(store.LogFilter{IncludePayloads: true})
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	l := logs[0]
	if string(l.RawRequest) != body {
		t.Errorf("raw request = %q, want %q", l.RawRequest, body)
	}
	if string(l.RawResponse) != rec.Body.String() {
		t.Errorf("raw response = %q, want client body %q", l.RawResponse, rec.Body.String())
	}
	var assembled struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(l.AssembledResponse, &assembled); err != nil {
		t.Fatalf("assembled response is not JSON: %v\n%s", err, l.AssembledResponse)
	}
	if len(assembled.Choices) != 1 || assembled.Choices[0].Message.Content != "hello" {
		t.Errorf("assembled = %s", l.AssembledResponse)
	}
}

func TestPayloadCaptureUnknownStreamKeepsRawOnly(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"delta\":\"x\"}\n\n")
	})
	p, st, key := harness(t, up)
	p.cfg.PayloadCapture = config.PayloadCapture{Enabled: true, AssembleStreams: true}

	rec := do(t, p, key, "/v1/custom", `{"model":"gpt-5.4","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	logs, _ := st.QueryLogs(store.LogFilter{IncludePayloads: true})
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if string(logs[0].RawResponse) != rec.Body.String() {
		t.Errorf("raw response = %q, want %q", logs[0].RawResponse, rec.Body.String())
	}
	if len(logs[0].AssembledResponse) != 0 {
		t.Errorf("assembled unknown stream = %s, want empty", logs[0].AssembledResponse)
	}
	if logs[0].APIType != "" {
		t.Errorf("api_type for unknown path = %q, want empty", logs[0].APIType)
	}
}

// TestPayloadCaptureRecordsAssembleErrorButStillMeters drives a malformed Anthropic stream (a
// content block delta with no preceding start) so the SDK accumulator gives up. The raw response
// is still captured, the failure reason is recorded, and usage from message_start is still metered.
func TestPayloadCaptureRecordsAssembleError(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":7,"output_tokens":1}}}`+"\n\n")
		fl.Flush()
		// Delta for an index with no content_block_start: an out-of-order gap the SDK rejects.
		fmt.Fprint(w, "event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"oops"}}`+"\n\n")
		fl.Flush()
	})
	p, st, key := harness(t, up)
	p.cfg.PayloadCapture = config.PayloadCapture{Enabled: true, AssembleStreams: true}

	rec := do(t, p, key, "/v1/messages", `{"model":"claude","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	logs, _ := st.QueryLogs(store.LogFilter{IncludePayloads: true})
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	l := logs[0]
	if l.APIType != "anthropic_messages" {
		t.Errorf("api_type = %q, want anthropic_messages", l.APIType)
	}
	if l.AssembleError == "" || len(l.AssembledResponse) != 0 {
		t.Errorf("expected assemble_error set and no assembled body, got error=%q assembled=%s", l.AssembleError, l.AssembledResponse)
	}
	if string(l.RawResponse) != rec.Body.String() {
		t.Errorf("raw response not captured: %q", l.RawResponse)
	}
	// Usage from message_start survives the assembly failure (metering is best-effort, non-blocking).
	if l.InputTokens != 7 {
		t.Errorf("input tokens = %d, want 7 (from message_start)", l.InputTokens)
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

// A LiteLLM tag-config 401 is a known transient bug: retry it, and a later success passes
// through normally.
func TestRetryOnLiteLLMTagBug401(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Not allowed to access model due to tags configuration. Passed model=gpt-5.4-mini and tags=['gcr']"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one retry)", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != http.StatusOK || logs[0].Attempts != 2 {
		t.Errorf("expected one 200 log with 2 attempts, got %+v", logs)
	}
}

// An unrelated 401 must NOT be retried, and its body must pass through unchanged.
func TestNoRetryOnGeneric401(t *testing.T) {
	var calls atomic.Int32
	const body = `{"error":{"message":"invalid api key"}}`
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, body)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry)", got)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q (must pass through unchanged)", rec.Body.String(), body)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != http.StatusUnauthorized || logs[0].Attempts != 1 {
		t.Errorf("expected one 401 log with 1 attempt, got %+v", logs)
	}
}

// A LiteLLM/Azure "operation is unsupported" 400 is a known transient flake: retry it, and a
// later success passes through normally.
func TestRetryOnAzureUnsupported400(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"litellm.BadRequestError: AzureException - The requested operation is unsupported.. Received Model Group=gpt-5.2-codex Available Model Group Fallbacks=None"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.2-codex"}`)
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one retry)", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != http.StatusOK || logs[0].Attempts != 2 {
		t.Errorf("expected one 200 log with 2 attempts, got %+v", logs)
	}
}

// An unrelated 400 must NOT be retried, and its body must pass through unchanged. The
// "operation is unsupported" text without the litellm signature is the boundary case.
func TestNoRetryOnGeneric400(t *testing.T) {
	var calls atomic.Int32
	const body = `{"error":{"message":"AzureException - The requested operation is unsupported."}}`
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, body)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry)", got)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q (must pass through unchanged)", rec.Body.String(), body)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != http.StatusBadRequest || logs[0].Attempts != 1 {
		t.Errorf("expected one 400 log with 1 attempt, got %+v", logs)
	}
}

// A 408 from the upstream is transient and must be retried like 429/5xx.
func TestRetryOn408(t *testing.T) {
	var calls atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	p, st, key := harness(t, up)

	rec := do(t, p, key, "/v1/x", `{"model":"gpt-5.4"}`)
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one retry)", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].StatusCode != http.StatusOK || logs[0].Attempts != 2 {
		t.Errorf("expected one 200 log with 2 attempts, got %+v", logs)
	}
}

// Gateway-synthesized errors must carry a schema that both the OpenAI (/v1/responses) and
// Anthropic (/v1/messages) SDKs can deserialize: top-level type:"error" plus error.type and
// error.message, while keeping our diagnostics.
func TestGatewayErrorSchema(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	p, _, _ := harness(t, up)

	// Invalid key -> gateway-synthesized 401, never reaching the upstream.
	rec := do(t, p, "sk-gw-bogus", "/v1/messages", `{"model":"claude-x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type     string `json:"type"`
			Message  string `json:"message"`
			Source   string `json:"source"`
			Attempts int    `json:"attempts"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if body.Type != "error" {
		t.Errorf("top-level type = %q, want \"error\" (Anthropic schema)", body.Type)
	}
	if body.Error.Type != "authentication_error" {
		t.Errorf("error.type = %q, want authentication_error", body.Error.Type)
	}
	if body.Error.Message == "" {
		t.Errorf("error.message is empty")
	}
	if body.Error.Source != sourceGateway {
		t.Errorf("error.source = %q, want gateway", body.Error.Source)
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
	st.CreateKey("dev", keys.Hash(plain), display, []string{"dead"}, config.StartFirst, config.OrderRoundRobin)

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
	st.CreateKey("mix", keys.Hash(plain), display, []string{"ghost", "up"}, config.StartFirst, config.OrderRoundRobin)

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
	st.CreateKey("dev", keys.Hash(plain), display, []string{"up"}, config.StartFirst, config.OrderRoundRobin)

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
	st.CreateKey("dev", keys.Hash(plain), display, []string{"up"}, config.StartFirst, config.OrderRoundRobin)

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
	st.CreateKey("dev", keys.Hash(plain), display, []string{"dead"}, config.StartFirst, config.OrderRoundRobin)

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
	st.CreateKey("dev", keys.Hash(plain), display, []string{"a", "b"}, config.StartFirst, config.OrderRoundRobin)

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

func names(seq []*config.Provider) []string {
	out := make([]string, len(seq))
	for i, p := range seq {
		out[i] = p.Name
	}
	return out
}

func TestProviderSequence(t *testing.T) {
	valid := []*config.Provider{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	// first/round_robin is deterministic binding order.
	if got := names(providerSequence(valid, config.StartFirst, config.OrderRoundRobin)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("first/round_robin = %v, want [a b c]", got)
	}
	// first/random keeps the first-bound provider in front.
	for i := 0; i < 20; i++ {
		got := names(providerSequence(valid, config.StartFirst, config.OrderRandom))
		if got[0] != "a" {
			t.Errorf("first/random head = %q, want a", got[0])
		}
		assertPermutation(t, got)
	}
	// random orders preserve membership.
	for i := 0; i < 20; i++ {
		assertPermutation(t, names(providerSequence(valid, config.StartRandom, config.OrderRoundRobin)))
		assertPermutation(t, names(providerSequence(valid, config.StartRandom, config.OrderRandom)))
	}
	// A single provider is returned unchanged regardless of policy.
	one := []*config.Provider{{Name: "solo"}}
	if got := names(providerSequence(one, config.StartRandom, config.OrderRandom)); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Errorf("single = %v, want [solo]", got)
	}
}

func assertPermutation(t *testing.T, got []string) {
	t.Helper()
	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (%v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if !want[n] || seen[n] {
			t.Errorf("not a permutation of a,b,c: %v", got)
		}
		seen[n] = true
	}
}

func TestFailoverToNextProvider(t *testing.T) {
	// Provider "a" always fails with a retryable 503; "b" succeeds. A first/round_robin key
	// must fail over from a to b and report b as the serving provider.
	var aHits, bHits atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: "mk",
		Defaults:  config.Defaults{Retry: config.Retry{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}},
		Providers: []config.Provider{
			{Name: "a", BaseURL: srvA.URL, APIKey: "x"},
			{Name: "b", BaseURL: srvB.URL, APIKey: "x"},
		},
	}
	p := New(cfg, st, testPricing(), srvA.Client(), nil)
	plain, display, _ := keys.Generate()
	st.CreateKey("dev", keys.Hash(plain), display, []string{"a", "b"}, config.StartFirst, config.OrderRoundRobin)

	rec := do(t, p, plain, "/v1/x", `{"model":"gpt-5.4"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Errorf("hits a=%d b=%d, want a=1 b=1", aHits.Load(), bHits.Load())
	}
	if got := rec.Header().Get(headerProvider); got != "b" {
		t.Errorf("served provider header = %q, want b", got)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 1 || logs[0].Provider != "b" || logs[0].Attempts != 2 {
		t.Fatalf("log = %+v, want provider=b attempts=2", logs[0])
	}
}

// panicWriter panics on every Write, standing in for a metering accumulator that blows up on
// adversarial upstream bytes.
type panicWriter struct{}

func (panicWriter) Write(p []byte) (int, error) { panic("metering boom") }

// TestStreamBodyMeteringPanicDoesNotBreakStream proves the proxy-first guarantee: a panic in
// best-effort metering is contained by the meterPump's recover wall and never reaches the
// proxy goroutine, so the client still receives the full upstream body.
func TestStreamBodyMeteringPanicDoesNotBreakStream(t *testing.T) {
	const payload = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	pump := newMeterPump(panicWriter{}, nil, 0, slog.New(slog.DiscardHandler))

	_, gotByte, written := streamBody(rec, strings.NewReader(payload), pump, time.Now())
	pump.finish() // must not panic

	if !gotByte || written != int64(len(payload)) {
		t.Fatalf("written = %d, gotByte = %v; want %d/true", written, gotByte, len(payload))
	}
	if rec.Body.String() != payload {
		t.Fatalf("client body = %q, want full payload despite metering panic", rec.Body.String())
	}
}

// TestStreamBodyManyChunksNoLoss feeds far more chunks than meterQueueDepth and confirms the
// decoupled metering goroutine drains every one before finish() returns (blocking hand-off
// provides backpressure without dropping), so usage and payload capture stay exact.
func TestStreamBodyManyChunksNoLoss(t *testing.T) {
	var sb strings.Builder
	const n = 500 // >> meterQueueDepth
	for i := 0; i < n; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	full := sb.String()

	rec := httptest.NewRecorder()
	acc := usage.NewAccumulator(true)
	raw := newLimitedRecorder(1 << 30)
	pump := newMeterPump(acc, raw, 0, slog.New(slog.DiscardHandler))

	// A reader that yields one small chunk per Read, so the proxy hands off many times.
	_, _, written := streamBody(rec, &chunkReader{data: []byte(full)}, pump, time.Now())
	_, dropped := pump.finish()

	if dropped {
		t.Fatal("metering dropped chunks; blocking hand-off should never drop while consumer is alive")
	}
	if written != int64(len(full)) {
		t.Fatalf("written = %d, want %d", written, len(full))
	}
	if rec.Body.String() != full {
		t.Fatal("client body mismatch")
	}
	if string(raw.Bytes()) != full {
		t.Fatalf("metered raw bytes (%d) != full body (%d)", len(raw.Bytes()), len(full))
	}
}

// chunkReader returns the data in small slices, one per Read, to exercise the per-chunk
// hand-off path many times.
type chunkReader struct {
	data []byte
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + 8
	if end > len(c.data) {
		end = len(c.data)
	}
	n := copy(p, c.data[c.pos:end])
	c.pos += n
	return n, nil
}

// TestRequestBodyExceedsLimitReturns413 confirms an over-limit body is rejected before any
// upstream call, with no log row written.
func TestRequestBodyExceedsLimitReturns413(t *testing.T) {
	var hits int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	})
	p, st, key := harness(t, up)
	p.cfg.Server.MaxRequestBytes = 16

	rec := do(t, p, key, "/v1/chat/completions", `{"model":"gpt-5.4","pad":"xxxxxxxxxxxxxxxxxxxxxxxx"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("upstream hit %d times, want 0 (rejected before forward)", got)
	}
	if src := rec.Header().Get(headerErrorSource); src != sourceGateway {
		t.Errorf("error source = %q, want %q", src, sourceGateway)
	}
	logs, _ := st.QueryLogs(store.LogFilter{})
	if len(logs) != 0 {
		t.Errorf("expected no log rows for a pre-forward rejection, got %d", len(logs))
	}
}

// TestRequestBodyAtLimitPasses confirms a body within the configured cap forwards normally.
func TestRequestBodyAtLimitPasses(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	p, _, key := harness(t, up)
	body := `{"model":"gpt-5.4"}`
	p.cfg.Server.MaxRequestBytes = int64(len(body)) // exactly the body size is allowed

	rec := do(t, p, key, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
