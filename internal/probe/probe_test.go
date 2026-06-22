package probe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestResolvePath(t *testing.T) {
	cases := []struct{ explicit, model, want string }{
		{"", "claude-opus-4-8", "/v1/messages"},
		{"", "Claude-Sonnet", "/v1/messages"}, // case-insensitive
		{"", "gpt-5.4", "/v1/responses"},
		{"", "gpt-5.2", "/v1/chat/completions"},
		{"", "GPT-5.2", "/v1/chat/completions"},                             // case-insensitive
		{"/v1/chat/completions", "claude-opus-4-8", "/v1/chat/completions"}, // explicit wins
	}
	for _, c := range cases {
		if got := ResolvePath(c.explicit, c.model); got != c.want {
			t.Errorf("ResolvePath(%q,%q) = %q, want %q", c.explicit, c.model, got, c.want)
		}
	}
}

func TestBuildBody(t *testing.T) {
	if b := string(BuildBody("/v1/responses", "gpt-5.4", 16, false)); !strings.Contains(b, `"input":"ping"`) ||
		!strings.Contains(b, `"max_output_tokens":16`) || strings.Contains(b, "messages") {
		t.Errorf("responses body = %s", b)
	}
	if b := string(BuildBody("/v1/messages", "claude-opus-4-8", 8, true)); !strings.Contains(b, `"messages"`) ||
		!strings.Contains(b, `"max_tokens":8`) || !strings.Contains(b, `"stream":true`) {
		t.Errorf("messages body = %s", b)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		ok   bool
		body string
		want string
	}{
		{true, `{"usage":{"prompt_tokens":7,"completion_tokens":3}}`, "in=7 out=3"},
		{true, `{"usage":{"input_tokens":5,"output_tokens":2}}`, "in=5 out=2"},
		{true, `{}`, "ok"},
		{false, `{"error":{"message":"model not found"}}`, "model not found"},
		{false, "boom", "boom"},
	}
	for _, c := range cases {
		if got := Summarize(c.ok, []byte(c.body)); got != c.want {
			t.Errorf("Summarize(%v,%s) = %q, want %q", c.ok, c.body, got, c.want)
		}
	}
}

func TestGlobMatchAndExcluded(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"gpt-image*", "gpt-image-1", true},
		{"gpt-image*", "gpt-image-1-mini", true},
		{"gpt-image*", "gpt-5.4", false},
		{"gpt-image*", "my-gpt-image-1", false}, // prefix-anchored
		{"*-audio", "gpt-4o-audio", true},
		{"*-audio", "gpt-4o", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxc", false},
		{"gpt-5.4", "gpt-5.4", true},
		{"gpt-5.4", "gpt-5.4-mini", false},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pattern, c.s); got != c.want {
			t.Errorf("GlobMatch(%q,%q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
	patterns := ParsePatterns(" gpt-image*, *-audio ,")
	if len(patterns) != 2 {
		t.Fatalf("ParsePatterns = %v, want 2 entries", patterns)
	}
	if !Excluded(patterns, "gpt-image-1") || !Excluded(patterns, "gpt-4o-audio") || Excluded(patterns, "gpt-5.4") {
		t.Error("Excluded did not match expected models")
	}
	if Excluded(nil, "anything") {
		t.Error("no patterns should exclude nothing")
	}
}

func TestProbeUsesSenderAndEndpoint(t *testing.T) {
	var gotProvider, gotPath string
	send := func(ctx context.Context, provider, path string, body []byte) (int, string, []byte, error) {
		gotProvider, gotPath = provider, path
		return 200, "2", []byte(`{"usage":{"input_tokens":4,"output_tokens":1}}`), nil
	}
	r := Probe(context.Background(), Job{Provider: "anthropic", Model: "claude-opus-4-8"}, "", 16, false, send)
	if gotProvider != "anthropic" || gotPath != "/v1/messages" {
		t.Errorf("sender saw provider=%q path=%q", gotProvider, gotPath)
	}
	if !r.OK || r.Status != 200 || r.Attempts != "2" || r.Detail != "in=4 out=1" || r.Endpoint != "/v1/messages" {
		t.Errorf("result = %+v", r)
	}

	// A sender error surfaces as a not-ok result carrying the error text.
	rerr := Probe(context.Background(), Job{Provider: "p", Model: "gpt-5.4"}, "", 16, false,
		func(ctx context.Context, provider, path string, body []byte) (int, string, []byte, error) {
			return 0, "", nil, errors.New("dial fail")
		})
	if rerr.OK || rerr.Detail != "dial fail" {
		t.Errorf("error result = %+v", rerr)
	}
}

func TestPoolConcurrencyAndCompleteness(t *testing.T) {
	var inflight, peak int32
	jobs := make([]Job, 12)
	for i := range jobs {
		jobs[i] = Job{Provider: "p", Model: "m"}
	}
	var mu sync.Mutex
	callbacks := 0
	// Rendezvous: the first two workers to enter both block until the second
	// arrives, guaranteeing they are in-flight simultaneously (deterministic
	// overlap, no reliance on busy-spin or wall-clock timing). Once released,
	// the closed channel lets every later call through immediately.
	var entered int32
	ready := make(chan struct{})
	results := Pool(jobs, 4, func(j Job) Result {
		cur := atomic.AddInt32(&inflight, 1)
		if atomic.AddInt32(&entered, 1) == 2 {
			close(ready)
		}
		<-ready
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		atomic.AddInt32(&inflight, -1)
		return Result{Provider: j.Provider, Model: j.Model, OK: true}
	}, func(r Result) {
		mu.Lock()
		callbacks++
		mu.Unlock()
	})

	if len(results) != 12 || callbacks != 12 {
		t.Fatalf("results=%d callbacks=%d, want 12 each", len(results), callbacks)
	}
	if peak < 2 {
		t.Errorf("peak in-flight = %d, want >1 (no parallelism)", peak)
	}
	if peak > 4 {
		t.Errorf("peak in-flight = %d, want <=4 (pool not bounded)", peak)
	}
}

func TestPoolClampsWorkers(t *testing.T) {
	results := Pool([]Job{{Provider: "p", Model: "a"}, {Provider: "p", Model: "b"}}, 0,
		func(j Job) Result { return Result{Model: j.Model, OK: true} }, nil)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
}
