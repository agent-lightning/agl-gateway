// Command modelcheck exercises every model of every configured provider through a running
// agl-gateway. It reads the provider/model inventory from the gateway's control plane
// (GET /admin/providers), mints a temporary gateway key bound to those providers, then sends
// one small request per (provider, model), pinning the provider with the X-AGL-Provider
// header. Results stream as they complete and the process exits non-zero if any model failed.
//
// Usage:
//
//	modelcheck -url http://localhost:8080 -master-key mk-...
//	AGL_MASTER_KEY=mk-... modelcheck -provider openai -max-tokens 8
//
// The request path and body are chosen per model by default: models whose name starts with
// "claude" are sent as an Anthropic Messages request to /v1/messages, everything else as an
// OpenAI Responses request to /v1/responses. Override the path for every model with -path.
// The per-model logic is shared with the gateway's own /admin/test endpoint via internal/probe.
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
	"text/tabwriter"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/probe"
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

func main() {
	url := flag.String("url", "http://localhost:8080", "base URL of the running gateway")
	masterKey := flag.String("master-key", os.Getenv("AGL_MASTER_KEY"), "gateway master key (or set AGL_MASTER_KEY)")
	path := flag.String("path", "", "request path for every model (default: auto — /v1/messages for claude*, else /v1/responses)")
	providerFilter := flag.String("provider", "", "only test this provider (default: all)")
	exclude := flag.String("exclude", "gpt-image*", "comma-separated model-name globs to skip (e.g. gpt-image*,*-audio)")
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

	// Run inside a closure that returns an exit code, so the deferred deletion of the
	// temporary key runs even when we finish with failures (os.Exit skips defers).
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

		// Build the job list, applying the exclude globs, and learn column widths up front so
		// the streamed lines align.
		patterns := probe.ParsePatterns(*exclude)
		jobs := make([]probe.Job, 0, total)
		provW, modelW := 0, 0
		skipped := 0
		for _, p := range providers {
			for _, m := range p.Models {
				if probe.Excluded(patterns, m) {
					skipped++
					continue
				}
				jobs = append(jobs, probe.Job{Provider: p.Name, Model: m})
				provW = max(provW, len(p.Name))
				modelW = max(modelW, len(m))
			}
		}
		if len(jobs) == 0 {
			fmt.Printf("No models to test: all %d discovered model(s) were excluded by %q\n", total, *exclude)
			return 0
		}

		workers := *concurrency
		if workers > len(jobs) {
			workers = len(jobs)
		}
		skipNote := ""
		if skipped > 0 {
			skipNote = fmt.Sprintf(" (skipped %d matching %q)", skipped, *exclude)
		}
		fmt.Printf("Testing %d model(s)%s across %d provider(s) via %s — path: %s — concurrency %d\n\n",
			len(jobs), skipNote, len(names), base, pathDesc, workers)

		// Fan out across a bounded worker pool, streaming each result as it completes.
		send := gatewaySender(client, base, gwKey)
		done, failures := 0, 0
		counterW := len(strconv.Itoa(len(jobs)))
		results := probe.Pool(jobs, workers, func(j probe.Job) probe.Result {
			return probe.Probe(context.Background(), j, *path, *maxTokens, *stream, send)
		}, func(r probe.Result) {
			done++
			if !r.OK {
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

// gatewaySender delivers each probe through the running gateway: it posts to base+path with
// the temporary gateway key and pins the provider via X-AGL-Provider.
func gatewaySender(client *http.Client, base, gwKey string) probe.Sender {
	return func(ctx context.Context, provider, path string, body []byte) (int, string, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
		if err != nil {
			return 0, "", nil, err
		}
		req.Header.Set("Authorization", "Bearer "+gwKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AGL-Provider", provider)

		resp, err := client.Do(req)
		if err != nil {
			return 0, "", nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, resp.Header.Get("X-AGL-Attempts"), b, nil
	}
}

// printProgress writes one aligned line for a completed probe, prefixed with a running
// [done/total] counter so progress is visible as results stream in.
func printProgress(done, total, counterW, provW, modelW int, r probe.Result) {
	mark := "ok  "
	if !r.OK {
		mark = "FAIL"
	}
	status := "—"
	if r.Status > 0 {
		status = strconv.Itoa(r.Status)
	}
	fmt.Printf("[%*d/%d] %s  %-*s  %-*s  %-13s  %3s  %6dms  %s\n",
		counterW, done, total, mark, provW, r.Provider, modelW, r.Model, r.Endpoint, status, r.DurationMS, clip(r.Detail, 100))
}

// clip shortens a one-line detail for the fixed-width terminal table; the portal shows the
// full text. (probe.Summarize no longer truncates, so display width is each caller's choice.)
func clip(s string, n int) string {
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

// printFailures prints a sorted table of just the failed probes, since streamed lines arrive
// in completion order and failures are easy to lose in a long run.
func printFailures(results []probe.Result) {
	fails := make([]probe.Result, 0)
	for _, r := range results {
		if !r.OK {
			fails = append(fails, r)
		}
	}
	if len(fails) == 0 {
		return
	}
	sort.Slice(fails, func(i, j int) bool {
		if fails[i].Provider != fails[j].Provider {
			return fails[i].Provider < fails[j].Provider
		}
		return fails[i].Model < fails[j].Model
	})
	fmt.Println("\nFailures:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tMODEL\tENDPOINT\tHTTP\tTRIES\tDETAIL")
	for _, r := range fails {
		tries := r.Attempts
		if tries == "" {
			tries = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", r.Provider, r.Model, r.Endpoint, r.Status, tries, clip(r.Detail, 100))
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "modelcheck: "+format+"\n", args...)
	os.Exit(2)
}
