// Package probe runs small "does this model work" requests against a gateway and interprets
// the outcome. It is shared by the modelcheck CLI (which sends over HTTP to a remote gateway)
// and the admin control plane (which sends in-process through the proxy), so it stays
// agnostic to how a request is delivered: callers supply a Sender.
//
// The per-model decisions live here — which endpoint a model maps to, what request body to
// send, and how to read usage or an error out of the response — so both callers behave
// identically.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Job is a single (provider, model) pair to probe.
type Job struct {
	Provider string
	Model    string
}

// Result is the outcome of probing one model. Fields are JSON-tagged so the admin endpoint
// can return them straight to the portal.
type Result struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Endpoint   string `json:"endpoint"`
	Status     int    `json:"status"`
	Attempts   string `json:"attempts,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail"`
	OK         bool   `json:"ok"`
}

// Sender delivers a prepared request for `provider` to `path` carrying `body`, and reports the
// resulting status code, the gateway's X-AGL-Attempts value (may be empty), and the response
// body. A transport-level failure is returned as err.
type Sender func(ctx context.Context, provider, path string, body []byte) (status int, attempts string, respBody []byte, err error)

// Probe resolves the endpoint and body for j.Model, delivers it via send, and returns the
// interpreted result. explicitPath overrides the per-model endpoint when non-empty.
func Probe(ctx context.Context, j Job, explicitPath string, maxTokens int, stream bool, send Sender) Result {
	path := ResolvePath(explicitPath, j.Model)
	r := Result{Provider: j.Provider, Model: j.Model, Endpoint: path}

	start := time.Now()
	status, attempts, body, err := send(ctx, j.Provider, path, BuildBody(path, j.Model, maxTokens, stream))
	r.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	r.Status = status
	r.Attempts = attempts
	r.OK = status == http.StatusOK
	r.Detail = Summarize(r.OK, body)
	return r
}

// Pool runs do() for each job across `workers` goroutines (clamped to >=1), invoking onResult
// — which may be nil — for each completed result on a single goroutine (so it needs no
// locking), and returns every result in completion order.
func Pool(jobs []Job, workers int, do func(Job) Result, onResult func(Result)) []Result {
	if workers < 1 {
		workers = 1
	}
	jobCh := make(chan Job)
	resCh := make(chan Result)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				resCh <- do(j)
			}
		}()
	}
	go func() {
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
	}()
	go func() {
		wg.Wait()
		close(resCh)
	}()

	out := make([]Result, 0, len(jobs))
	for r := range resCh {
		if onResult != nil {
			onResult(r)
		}
		out = append(out, r)
	}
	return out
}

// ResolvePath picks the request path for a model. An explicit override wins; otherwise claude*
// models use Anthropic's /v1/messages, gpt-5.2 uses OpenAI's /v1/chat/completions, and
// everything else uses OpenAI's /v1/responses.
func ResolvePath(explicit, model string) string {
	if explicit != "" {
		return explicit
	}
	lower := strings.ToLower(model)
	if strings.HasPrefix(lower, "claude") {
		return "/v1/messages"
	}
	if strings.HasPrefix(lower, "gpt-5.2") {
		return "/v1/chat/completions"
	}
	return "/v1/responses"
}

// BuildBody constructs a minimal request body whose shape matches the endpoint inferred from
// path: Anthropic Messages, OpenAI Responses, or (fallback) OpenAI Chat Completions.
func BuildBody(path, model string, maxTokens int, stream bool) []byte {
	var body map[string]any
	switch {
	case strings.Contains(path, "/responses"):
		body = map[string]any{"model": model, "input": "ping", "max_output_tokens": maxTokens}
	case strings.Contains(path, "/messages"):
		body = map[string]any{"model": model, "max_tokens": maxTokens,
			"messages": []map[string]string{{"role": "user", "content": "ping"}}}
	default: // chat completions and compatible
		body = map[string]any{"model": model, "max_tokens": maxTokens,
			"messages": []map[string]string{{"role": "user", "content": "ping"}}}
	}
	if stream {
		body["stream"] = true
	}
	b, _ := json.Marshal(body)
	return b
}

// Summarize extracts a short, human-readable note from a response body: token usage on
// success, or the provider/gateway error message on failure.
func Summarize(ok bool, body []byte) string {
	if ok {
		var u struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &u) == nil {
			in := u.Usage.PromptTokens
			if in == 0 {
				in = u.Usage.InputTokens
			}
			out := u.Usage.CompletionTokens
			if out == 0 {
				out = u.Usage.OutputTokens
			}
			if in > 0 || out > 0 {
				return fmt.Sprintf("in=%d out=%d", in, out)
			}
		}
		return "ok"
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return collapse(e.Error.Message)
	}
	return collapse(strings.TrimSpace(string(body)))
}

// collapse squeezes runs of whitespace to single spaces so a detail reads as one tidy line.
// It does not truncate — callers that render in a fixed width (e.g. the CLI table) clip it
// themselves, while the portal shows it in full.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ParsePatterns splits a comma-separated glob list, trimming blanks.
func ParsePatterns(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Excluded reports whether model matches any of the glob patterns.
func Excluded(patterns []string, model string) bool {
	for _, p := range patterns {
		if GlobMatch(p, model) {
			return true
		}
	}
	return false
}

// GlobMatch matches s against a pattern using `*` as a wildcard (any run of characters). A
// pattern with no `*` must match exactly. This is enough for model-name filters like
// "gpt-image*" or "*-audio" without pulling in path-glob semantics.
func GlobMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
