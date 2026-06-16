// Command modelcheck exercises every model of every configured provider through a running
// agl-gateway. It reads the provider/model inventory from the gateway's control plane
// (GET /admin/providers), mints a temporary gateway key bound to those providers, then sends
// one small request per (provider, model), pinning the provider with the X-AGL-Provider
// header. Results are printed as a table and the process exits non-zero if any model failed.
//
// Usage:
//
//	modelcheck -url http://localhost:8080 -master-key mk-...
//	AGL_MASTER_KEY=mk-... modelcheck -provider openai -max-tokens 8
//
// The request path and body are chosen per model by default: models whose name starts with
// "claude" are sent as an Anthropic Messages request to /v1/messages, everything else as an
// OpenAI Responses request to /v1/responses. Override the path for every model with -path
// (the body shape is then inferred from that path).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

type providerInfo struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	Error  string   `json:"error,omitempty"`
}

type createKeyResp struct {
	ID  int64  `json:"id"`
	Key string `json:"key"`
}

type result struct {
	provider string
	model    string
	endpoint string
	status   int
	attempts string
	dur      time.Duration
	detail   string
	ok       bool
}

func main() {
	url := flag.String("url", "http://localhost:8080", "base URL of the running gateway")
	masterKey := flag.String("master-key", os.Getenv("AGL_MASTER_KEY"), "gateway master key (or set AGL_MASTER_KEY)")
	path := flag.String("path", "", "request path for every model (default: auto — /v1/messages for claude*, else /v1/responses)")
	providerFilter := flag.String("provider", "", "only test this provider (default: all)")
	maxTokens := flag.Int("max-tokens", 16, "max_tokens for each probe request")
	stream := flag.Bool("stream", false, "send stream:true and read the SSE response")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	concurrency := flag.Int("concurrency", 8, "number of probes to run in parallel")
	flag.Parse()

	if *masterKey == "" {
		fatal("a master key is required: pass -master-key or set AGL_MASTER_KEY")
	}
	base := strings.TrimRight(*url, "/")

	client := &http.Client{Timeout: *timeout}

	providers, err := fetchProviders(client, base, *masterKey)
	if err != nil {
		fatal("could not list providers: %v", err)
	}
	if *providerFilter != "" {
		providers = filterProviders(providers, *providerFilter)
		if len(providers) == 0 {
			fatal("provider %q not found in gateway config", *providerFilter)
		}
	}

	// Collect the providers that actually have models to test.
	names := make([]string, 0, len(providers))
	total := 0
	for _, p := range providers {
		if p.Error != "" {
			fmt.Fprintf(os.Stderr, "warning: provider %q model discovery failed: %s\n", p.Name, p.Error)
		}
		if len(p.Models) > 0 {
			names = append(names, p.Name)
			total += len(p.Models)
		}
	}
	if total == 0 {
		fatal("no models to test across %d provider(s)", len(providers))
	}

	// Run the probes inside a closure that returns an exit code, so the deferred deletion of
	// the temporary key runs even when we finish with failures (os.Exit skips defers).
	os.Exit(func() int {
		// Mint a temporary key bound to every provider under test, deleted on the way out.
		keyID, gwKey, err := createKey(client, base, *masterKey, names)
		if err != nil {
			fatal("could not create temporary gateway key: %v", err)
		}
		defer func() {
			if err := deleteKey(client, base, *masterKey, keyID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not delete temporary key %d: %v\n", keyID, err)
			}
		}()

		pathDesc := "auto (/v1/messages for claude*, else /v1/responses)"
		if *path != "" {
			pathDesc = *path
		}

		// Flatten to a job list and learn column widths up front so streamed lines align.
		jobs := make([]job, 0, total)
		provW, modelW := 0, 0
		for _, p := range providers {
			for _, m := range p.Models {
				jobs = append(jobs, job{p.Name, m})
				provW = max(provW, len(p.Name))
				modelW = max(modelW, len(m))
			}
		}

		workers := *concurrency
		if workers < 1 {
			workers = 1
		}
		if workers > len(jobs) {
			workers = len(jobs)
		}
		fmt.Printf("Testing %d model(s) across %d provider(s) via %s — path: %s — concurrency %d\n\n",
			total, len(names), base, pathDesc, workers)

		// Fan out across a bounded worker pool, streaming each result as it completes.
		done, failures := 0, 0
		counterW := len(strconv.Itoa(len(jobs)))
		results := runProbes(client, base, *path, gwKey, jobs, *maxTokens, workers, *stream, func(r result) {
			done++
			if !r.ok {
				failures++
			}
			printProgress(done, len(jobs), counterW, provW, modelW, r)
		})

		fmt.Printf("\n%d passed, %d failed\n", len(results)-failures, failures)
		if failures > 0 {
			printFailures(results)
			return 1
		}
		return 0
	}())
}

// job is a single (provider, model) probe to run.
type job struct{ provider, model string }

// runProbes fans the jobs out across a bounded pool of `workers` goroutines, invoking
// onResult for each completed probe in completion order, and returns every result. onResult
// (which may be nil) runs on the single collecting goroutine, so it needs no locking.
func runProbes(client *http.Client, base, path, gwKey string, jobs []job, maxTokens, workers int, stream bool, onResult func(result)) []result {
	if workers < 1 {
		workers = 1
	}
	jobCh := make(chan job)
	resCh := make(chan result)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				resCh <- probe(client, base, path, gwKey, j.provider, j.model, maxTokens, stream)
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

	results := make([]result, 0, len(jobs))
	for r := range resCh {
		if onResult != nil {
			onResult(r)
		}
		results = append(results, r)
	}
	return results
}

func probe(client *http.Client, base, explicitPath, gwKey, provider, model string, maxTokens int, stream bool) result {
	path := resolvePath(explicitPath, model)
	res := result{provider: provider, model: model, endpoint: path}

	payload := buildBody(path, model, maxTokens, stream)

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		res.detail = err.Error()
		return res
	}
	req.Header.Set("Authorization", "Bearer "+gwKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AGL-Provider", provider) // pin the bound provider

	resp, err := client.Do(req)
	res.dur = time.Since(start)
	if err != nil {
		res.detail = err.Error()
		return res
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	res.status = resp.StatusCode
	res.attempts = resp.Header.Get("X-AGL-Attempts")
	res.ok = resp.StatusCode == http.StatusOK
	res.detail = summarize(res.ok, body)
	return res
}

// resolvePath picks the request path for a model. An explicit -path wins; otherwise claude*
// models use Anthropic's /v1/messages and everything else uses OpenAI's /v1/responses.
func resolvePath(explicit, model string) string {
	if explicit != "" {
		return explicit
	}
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		return "/v1/messages"
	}
	return "/v1/responses"
}

// buildBody constructs a minimal request body whose shape matches the endpoint inferred from
// path: Anthropic Messages, OpenAI Responses, or (fallback) OpenAI Chat Completions.
func buildBody(path, model string, maxTokens int, stream bool) []byte {
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

// summarize extracts a short, human-readable note from a response body: token usage on
// success, or the provider/gateway error message on failure.
func summarize(ok bool, body []byte) string {
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
		return truncate(e.Error.Message, 80)
	}
	return truncate(strings.TrimSpace(string(body)), 80)
}

// printProgress writes one aligned line for a completed probe, prefixed with a running
// [done/total] counter so progress is visible as results stream in.
func printProgress(done, total, counterW, provW, modelW int, r result) {
	mark := "ok  "
	if !r.ok {
		mark = "FAIL"
	}
	status := "—"
	if r.status > 0 {
		status = strconv.Itoa(r.status)
	}
	fmt.Printf("[%*d/%d] %s  %-*s  %-*s  %-13s  %3s  %6dms  %s\n",
		counterW, done, total, mark, provW, r.provider, modelW, r.model, r.endpoint, status, r.dur.Milliseconds(), r.detail)
}

// printFailures prints a sorted table of just the failed probes, since streamed lines arrive
// in completion order and failures are easy to lose in a long run.
func printFailures(results []result) {
	fails := make([]result, 0)
	for _, r := range results {
		if !r.ok {
			fails = append(fails, r)
		}
	}
	if len(fails) == 0 {
		return
	}
	sort.Slice(fails, func(i, j int) bool {
		if fails[i].provider != fails[j].provider {
			return fails[i].provider < fails[j].provider
		}
		return fails[i].model < fails[j].model
	})
	fmt.Println("\nFailures:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tMODEL\tENDPOINT\tHTTP\tTRIES\tDETAIL")
	for _, r := range fails {
		tries := r.attempts
		if tries == "" {
			tries = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", r.provider, r.model, r.endpoint, r.status, tries, r.detail)
	}
	tw.Flush()
}

// --- control-plane helpers ---

func fetchProviders(client *http.Client, base, masterKey string) ([]providerInfo, error) {
	req, _ := http.NewRequest(http.MethodGet, base+"/admin/providers", nil)
	req.Header.Set("Authorization", "Bearer "+masterKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var provs []providerInfo
	if err := json.Unmarshal(body, &provs); err != nil {
		return nil, fmt.Errorf("decode providers: %w", err)
	}
	return provs, nil
}

func filterProviders(provs []providerInfo, name string) []providerInfo {
	for _, p := range provs {
		if p.Name == name {
			return []providerInfo{p}
		}
	}
	return nil
}

func createKey(client *http.Client, base, masterKey string, providers []string) (int64, string, error) {
	payload, _ := json.Marshal(map[string]any{
		"name":      "modelcheck-" + time.Now().Format("20060102-150405"),
		"providers": providers,
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/admin/keys", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+masterKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return 0, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ck createKeyResp
	if err := json.Unmarshal(body, &ck); err != nil {
		return 0, "", fmt.Errorf("decode key: %w", err)
	}
	return ck.ID, ck.Key, nil
}

func deleteKey(client *http.Client, base, masterKey string, id int64) error {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
		fmt.Sprintf("%s/admin/keys/%d", base, id), nil)
	req.Header.Set("Authorization", "Bearer "+masterKey)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "modelcheck: "+format+"\n", args...)
	os.Exit(2)
}
