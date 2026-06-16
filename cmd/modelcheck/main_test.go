package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolvePath(t *testing.T) {
	cases := []struct {
		explicit, model, want string
	}{
		{"", "claude-opus-4-8", "/v1/messages"},
		{"", "Claude-Sonnet", "/v1/messages"}, // case-insensitive
		{"", "gpt-5.4", "/v1/responses"},
		{"", "mock-large", "/v1/responses"},
		{"/v1/chat/completions", "claude-opus-4-8", "/v1/chat/completions"}, // explicit wins
		{"/v1/chat/completions", "gpt-5.4", "/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := resolvePath(c.explicit, c.model); got != c.want {
			t.Errorf("resolvePath(%q,%q) = %q, want %q", c.explicit, c.model, got, c.want)
		}
	}
}

func TestBuildBody(t *testing.T) {
	// Responses endpoint: input + max_output_tokens, no messages/max_tokens.
	var resp map[string]any
	if err := json.Unmarshal(buildBody("/v1/responses", "gpt-5.4", 16, false), &resp); err != nil {
		t.Fatalf("responses body: %v", err)
	}
	if resp["input"] != "ping" || resp["max_output_tokens"] != float64(16) {
		t.Errorf("responses body = %v", resp)
	}
	if _, has := resp["messages"]; has {
		t.Errorf("responses body must not carry messages: %v", resp)
	}

	// Messages endpoint: messages + max_tokens, plus stream when requested.
	var msg map[string]any
	if err := json.Unmarshal(buildBody("/v1/messages", "claude-opus-4-8", 8, true), &msg); err != nil {
		t.Fatalf("messages body: %v", err)
	}
	if msg["max_tokens"] != float64(8) || msg["stream"] != true {
		t.Errorf("messages body = %v", msg)
	}
	if _, has := msg["messages"]; !has {
		t.Errorf("messages body must carry messages: %v", msg)
	}
}

func TestSummarize(t *testing.T) {
	// OpenAI-style usage.
	if got := summarize(true, []byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`)); got != "in=7 out=3" {
		t.Errorf("openai usage summary = %q", got)
	}
	// Responses/Anthropic-style usage.
	if got := summarize(true, []byte(`{"usage":{"input_tokens":5,"output_tokens":2}}`)); got != "in=5 out=2" {
		t.Errorf("responses usage summary = %q", got)
	}
	// Success without parseable usage.
	if got := summarize(true, []byte(`{}`)); got != "ok" {
		t.Errorf("empty success summary = %q", got)
	}
	// Error body: message extracted.
	if got := summarize(false, []byte(`{"error":{"message":"model not found"}}`)); got != "model not found" {
		t.Errorf("error summary = %q", got)
	}
	// Error without structured body: raw fallback.
	if got := summarize(false, []byte("boom")); got != "boom" {
		t.Errorf("raw error summary = %q", got)
	}
}

func TestRunProbesConcurrencyAndCompleteness(t *testing.T) {
	// Each probe sleeps briefly while the server tracks peak in-flight requests. With a pool
	// of 4 workers over 12 jobs, peak concurrency must exceed 1 (proving parallelism) and
	// every job must produce a result.
	var inflight, peak int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)

	jobs := make([]job, 0, 12)
	for i := 0; i < 12; i++ {
		jobs = append(jobs, job{provider: "p", model: "m"})
	}

	var mu sync.Mutex
	callbackCount := 0
	results := runProbes(srv.Client(), srv.URL, "/v1/responses", "k", jobs, 8, 4, false, func(r result) {
		mu.Lock()
		callbackCount++
		mu.Unlock()
	})

	if len(results) != 12 || callbackCount != 12 {
		t.Fatalf("results=%d callbacks=%d, want 12 each", len(results), callbackCount)
	}
	for _, r := range results {
		if !r.ok {
			t.Fatalf("probe failed: %+v", r)
		}
	}
	if peak < 2 {
		t.Errorf("peak in-flight = %d, want >1 (probes did not run in parallel)", peak)
	}
	if peak > 4 {
		t.Errorf("peak in-flight = %d, want <=4 (worker pool not bounded)", peak)
	}
}

func TestRunProbesClampsWorkers(t *testing.T) {
	// workers below 1 must still run every job exactly once.
	srv := fakeGatewaySimple(t)
	jobs := []job{{provider: "p", model: "a"}, {provider: "p", model: "b"}}
	results := runProbes(srv.Client(), srv.URL, "/v1/responses", "k", jobs, 8, 0, false, nil)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
}

// fakeGatewaySimple serves a minimal probe endpoint that always succeeds.
func fakeGatewaySimple(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGlobMatchAndExcluded(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"gpt-image*", "gpt-image-1", true},
		{"gpt-image*", "gpt-image-1-mini", true},
		{"gpt-image*", "gpt-image", true},
		{"gpt-image*", "gpt-5.4", false},
		{"gpt-image*", "my-gpt-image-1", false}, // prefix-anchored
		{"*-audio", "gpt-4o-audio", true},
		{"*-audio", "gpt-4o", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxc", false},
		{"gpt-5.4", "gpt-5.4", true}, // no wildcard: exact
		{"gpt-5.4", "gpt-5.4-mini", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}

	patterns := []string{"gpt-image*", "*-audio"}
	if !excluded(patterns, "gpt-image-1") || !excluded(patterns, "gpt-4o-audio") {
		t.Error("excluded should match either pattern")
	}
	if excluded(patterns, "gpt-5.4") {
		t.Error("gpt-5.4 should not be excluded")
	}
	if excluded(nil, "anything") {
		t.Error("no patterns should exclude nothing")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("a   b\nc", 80); got != "a b c" {
		t.Errorf("truncate collapses whitespace = %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := truncate(long, 10); len([]rune(got)) != 10 {
		t.Errorf("truncate length = %d, want 10", len([]rune(got)))
	}
}

// fakeGateway stands in for a running agl-gateway: the control-plane endpoints modelcheck
// needs plus the data-plane probe endpoints, recording the pinned provider header.
func fakeGateway(t *testing.T, deleted *bool, sawProvider *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/providers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"openai","models":["gpt-5.4"]},{"name":"anthropic","models":["claude-opus-4-8"]}]`))
	})
	mux.HandleFunc("POST /admin/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":42,"key":"sk-gw-temp"}`))
	})
	mux.HandleFunc("DELETE /admin/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "42" {
			*deleted = true
		}
		w.WriteHeader(http.StatusNoContent)
	})
	probe := func(w http.ResponseWriter, r *http.Request) {
		*sawProvider = r.Header.Get("X-AGL-Provider")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-AGL-Attempts", "1")
		w.Write([]byte(`{"usage":{"input_tokens":4,"output_tokens":1}}`))
	}
	mux.HandleFunc("POST /v1/responses", probe)
	mux.HandleFunc("POST /v1/messages", probe)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestControlPlaneHelpers(t *testing.T) {
	var deleted bool
	var sawProvider string
	srv := fakeGateway(t, &deleted, &sawProvider)
	client := &http.Client{Timeout: 2 * time.Second}

	provs, err := fetchProviders(client, srv.URL, "mk")
	if err != nil {
		t.Fatalf("fetchProviders: %v", err)
	}
	if len(provs) != 2 || provs[0].Name != "openai" || provs[1].Models[0] != "claude-opus-4-8" {
		t.Fatalf("providers = %+v", provs)
	}

	id, key, err := createKey(client, srv.URL, "mk", []string{"openai", "anthropic"})
	if err != nil || id != 42 || key != "sk-gw-temp" {
		t.Fatalf("createKey = (%d,%q,%v)", id, key, err)
	}

	if err := deleteKey(client, srv.URL, "mk", id); err != nil {
		t.Fatalf("deleteKey: %v", err)
	}
	if !deleted {
		t.Error("deleteKey did not reach the gateway")
	}
}

func TestProbeChoosesEndpointAndPinsProvider(t *testing.T) {
	var deleted bool
	var sawProvider string
	srv := fakeGateway(t, &deleted, &sawProvider)
	client := &http.Client{Timeout: 2 * time.Second}

	// Non-claude model -> /v1/responses, provider pinned.
	res := probe(client, srv.URL, "", "sk-gw-temp", "openai", "gpt-5.4", 16, false)
	if res.endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", res.endpoint)
	}
	if !res.ok || res.status != 200 || res.detail != "in=4 out=1" {
		t.Errorf("probe result = %+v", res)
	}
	if sawProvider != "openai" {
		t.Errorf("gateway saw X-AGL-Provider = %q, want openai", sawProvider)
	}

	// Claude model -> /v1/messages.
	res = probe(client, srv.URL, "", "sk-gw-temp", "anthropic", "claude-opus-4-8", 16, false)
	if res.endpoint != "/v1/messages" {
		t.Errorf("endpoint = %q, want /v1/messages", res.endpoint)
	}
	if sawProvider != "anthropic" {
		t.Errorf("gateway saw X-AGL-Provider = %q, want anthropic", sawProvider)
	}

	// Explicit override path is honored.
	res = probe(client, srv.URL, "/v1/messages", "sk-gw-temp", "openai", "gpt-5.4", 16, false)
	if res.endpoint != "/v1/messages" {
		t.Errorf("override endpoint = %q, want /v1/messages", res.endpoint)
	}
}
