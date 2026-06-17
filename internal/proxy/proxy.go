// Package proxy implements the gateway's data plane: it authenticates an inbound API key,
// routes the request to one of the key's bound providers, retries failed upstream attempts
// with exponential backoff + jitter, transparently streams the response (SSE or plain),
// and records best-effort request metadata and cost.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/keys"
	"github.com/agent-lightning/agl-gateway/internal/pricing"
	"github.com/agent-lightning/agl-gateway/internal/store"
	"github.com/agent-lightning/agl-gateway/internal/usage"
)

// Response headers the gateway adds so clients can see how a request was handled.
// headerProvider is dual-purpose: on a response it reports the provider that served the
// request; on a request it lets the client pin which of the key's bound providers to use.
const (
	headerProvider    = "X-AGL-Provider"
	headerAttempts    = "X-AGL-Attempts"
	headerErrorSource = "X-AGL-Error-Source"
)

// controlHeaderPrefix marks the gateway's own X-AGL-* headers. Such headers are stripped
// from a request before it is forwarded upstream so client-supplied control headers (e.g.
// the provider selector) never leak to the provider.
const controlHeaderPrefix = "X-Agl-" // canonicalized form of "X-AGL-"

// Error sources, reported to the client so it can tell a provider failure from a gateway one.
const (
	sourceGateway  = "gateway"
	sourceProvider = "provider"
)

// maxErrorBodyCapture bounds how much of an upstream error response body is copied into the
// request log. Provider error payloads (e.g. an OpenAI invalid_request_error JSON) are small.
const maxErrorBodyCapture = 2048

// hopByHop headers are connection-specific and must not be forwarded.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// Proxy is the routing/forwarding handler.
type Proxy struct {
	cfg    *config.Config
	store  *store.Store
	prices *pricing.Table
	client *http.Client
	logger *slog.Logger
}

// New constructs a Proxy. A nil client uses a sensible default; a nil logger discards logs.
func New(cfg *config.Config, st *store.Store, prices *pricing.Table, client *http.Client, logger *slog.Logger) *Proxy {
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Proxy{cfg: cfg, store: st, prices: prices, client: client, logger: logger}
}

// gatewayError writes a structured JSON error and the X-AGL-* headers describing whether
// the failure was the gateway's or an upstream provider's, and how many attempts were made.
// errorType maps an HTTP status to an error-type string that is valid in both the OpenAI
// (/v1/responses, /v1/chat/completions) and Anthropic (/v1/messages) error vocabularies, so
// typed SDK clients can deserialize a gateway-synthesized error without crashing.
func errorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestTimeout:
		return "timeout_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// gatewayError writes a gateway-synthesized JSON error. The body is shaped to satisfy both
// the OpenAI and Anthropic error schemas at once: a top-level `type:"error"` and an inner
// `error` object carrying `type` + `message` (the fields both SDKs read). The gateway's own
// diagnostics (source, attempts, provider) ride along on the inner object and the X-AGL-*
// headers; unknown fields are ignored by both SDKs.
func gatewayError(w http.ResponseWriter, status int, source, provider, msg string, attempts int) {
	w.Header().Set("Content-Type", "application/json")
	if provider != "" {
		w.Header().Set(headerProvider, provider)
	}
	if attempts > 0 {
		w.Header().Set(headerAttempts, strconv.Itoa(attempts))
	}
	w.Header().Set(headerErrorSource, source)
	w.WriteHeader(status)
	errObj := map[string]any{
		"type":     errorType(status),
		"message":  msg,
		"source":   source,
		"attempts": attempts,
	}
	if provider != "" {
		errObj["provider"] = provider
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": errObj})
}

// ServeHTTP authenticates, routes, forwards, and logs a single request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	presented := presentedKey(r)
	if presented == "" {
		gatewayError(w, http.StatusUnauthorized, sourceGateway, "", "agl-gateway: missing API key", 0)
		return
	}
	key, err := p.store.KeyByHash(keys.Hash(presented))
	if err != nil {
		p.logger.Error("key lookup failed", "err", err)
		gatewayError(w, http.StatusInternalServerError, sourceGateway, "", "agl-gateway: internal error", 0)
		return
	}
	if key == nil || key.Disabled {
		gatewayError(w, http.StatusUnauthorized, sourceGateway, "", "agl-gateway: invalid API key", 0)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		gatewayError(w, http.StatusBadRequest, sourceGateway, "", "agl-gateway: could not read request body", 0)
		return
	}
	model := usage.RequestModel(body)

	valid := p.validProviders(key)
	if len(valid) == 0 {
		p.logger.Error("key bound to no valid provider", "key_id", key.ID, "providers", key.Providers)
		gatewayError(w, http.StatusBadGateway, sourceGateway, "",
			"agl-gateway: no upstream provider is configured for this API key", 0)
		return
	}

	// A client may pin the provider via the X-AGL-Provider request header; it must name one
	// of the key's bound providers, and no cross-provider failover is attempted. Otherwise
	// the key's provider-selection policy builds the ordered sequence of providers to try.
	var sequence []*config.Provider
	if requested := strings.TrimSpace(r.Header.Get(headerProvider)); requested != "" {
		for _, cp := range valid {
			if cp.Name == requested {
				sequence = []*config.Provider{cp}
				break
			}
		}
		if sequence == nil {
			gatewayError(w, http.StatusBadRequest, sourceGateway, "",
				fmt.Sprintf("agl-gateway: provider %q is not bound to this API key", requested), 0)
			return
		}
	} else {
		sequence = providerSequence(valid, key.ProviderStart, key.ProviderOrder)
	}

	p.forward(w, r, body, key, sequence, model, start)
}

// validProviders returns the providers bound to the key that still exist in the
// configuration, preserving the key's binding order.
func (p *Proxy) validProviders(key *store.APIKey) []*config.Provider {
	valid := make([]*config.Provider, 0, len(key.Providers))
	for _, name := range key.Providers {
		if cp := p.cfg.Provider(name); cp != nil {
			valid = append(valid, cp)
		}
	}
	return valid
}

// providerSequence orders a key's valid providers into the sequence of attempts the retry
// loop walks when no provider is pinned. "start" fixes the first provider (the first-bound
// one, or a random one); "order" determines how the rest follow — binding order for
// round-robin (optionally rotated to a random start), shuffled for random.
func providerSequence(valid []*config.Provider, start, order string) []*config.Provider {
	seq := make([]*config.Provider, len(valid))
	copy(seq, valid)
	n := len(seq)
	if n <= 1 {
		return seq
	}
	switch order {
	case config.OrderRandom:
		if start == config.StartRandom {
			rand.Shuffle(n, func(i, j int) { seq[i], seq[j] = seq[j], seq[i] })
		} else {
			// Keep the first-bound provider in front; shuffle the rest.
			rand.Shuffle(n-1, func(i, j int) { seq[i+1], seq[j+1] = seq[j+1], seq[i+1] })
		}
	default: // round-robin: binding order, rotated to a random start when start=random.
		if start == config.StartRandom {
			k := rand.IntN(n)
			rot := make([]*config.Provider, 0, n)
			rot = append(rot, seq[k:]...)
			rot = append(rot, seq[:k]...)
			seq = rot
		}
	}
	return seq
}

// forward applies model mapping, runs the retry loop (failing over across the provider
// sequence), and streams the final response.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte, key *store.APIKey, sequence []*config.Provider, model string, start time.Time) {
	// The retry budget (attempt count + backoff) travels with the starting provider; each
	// retry advances to the next provider in the sequence, wrapping if the budget exceeds
	// the provider count.
	retry := sequence[0].ResolvedRetry(p.cfg.Defaults.Retry)

	logRow := &store.RequestLog{APIKeyID: key.ID, KeyName: key.Name, Model: model}

	var (
		resp           *http.Response
		lastErr        error
		prov           *config.Provider
		sendBody       []byte
		effectiveModel string
		mappedModel    string
		attempts       int
	)
	for attempt := 0; ; attempt++ {
		prov = sequence[attempt%len(sequence)]

		// Target and model mapping are provider-scoped, so recompute them each attempt.
		target := strings.TrimRight(prov.BaseURL, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		sendBody = body
		effectiveModel = model
		mappedModel = ""
		if to, ok := prov.ModelMap[model]; ok && to != "" && to != model {
			if rewritten, done := usage.SetModel(body, to); done {
				sendBody = rewritten
				mappedModel = to
				effectiveModel = to
			}
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(sendBody))
		if err != nil {
			lastErr = err
			break
		}
		copyRequestHeaders(req, r, prov)

		attempts++
		resp, err = p.client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < retry.MaxRetries && r.Context().Err() == nil {
				if !sleepBackoff(r.Context(), retry, attempt) {
					lastErr = r.Context().Err()
					break
				}
				continue
			}
			break
		}
		if retryable(resp) && attempt < retry.MaxRetries {
			drain(resp)
			if !sleepBackoff(r.Context(), retry, attempt) {
				lastErr = r.Context().Err()
				resp = nil
				break
			}
			continue
		}
		lastErr = nil
		break
	}

	// prov, mappedModel, and effectiveModel reflect the provider of the last attempt — the
	// one that served the response (or the last one tried before giving up).
	logRow.Provider = prov.Name
	logRow.MappedModel = mappedModel
	logRow.Attempts = attempts

	// No HTTP response at all: a network/transport failure (or a request we couldn't build).
	if resp == nil {
		source := sourceProvider
		msg := fmt.Sprintf("agl-gateway: upstream provider %q is unreachable after %d attempt(s)", prov.Name, attempts)
		if attempts == 0 {
			source = sourceGateway
			msg = "agl-gateway: failed to build upstream request"
		}
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		logRow.StatusCode = http.StatusBadGateway
		logRow.Error = msg
		logRow.DurationMillis = time.Since(start).Milliseconds()
		p.save(logRow)
		gatewayError(w, http.StatusBadGateway, source, prov.Name, msg, attempts)
		return
	}
	defer resp.Body.Close()

	streaming := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	copyResponseHeaders(w, resp)
	w.Header().Set(headerProvider, prov.Name)
	w.Header().Set(headerAttempts, strconv.Itoa(attempts))
	// An upstream error status that survived retries is reported as a provider failure so
	// the client can distinguish it from a gateway problem.
	if retryableStatus(resp.StatusCode) {
		w.Header().Set(headerErrorSource, sourceProvider)
	}
	isErr := resp.StatusCode >= 400
	w.WriteHeader(resp.StatusCode)

	acc := usage.NewAccumulator(streaming)
	captureLimit := 0
	if isErr {
		captureLimit = maxErrorBodyCapture // keep a snippet of the error body for the log
	}
	ttft, gotByte, head := streamBody(w, resp.Body, acc, start, captureLimit)

	u, _ := acc.Finalize()
	// Price by the requested model (the alias the client asked for); only fall back to the
	// mapped upstream model when the requested one has no configured price.
	u.Model = model
	if !p.prices.Has(model) && p.prices.Has(effectiveModel) {
		u.Model = effectiveModel
	}

	logRow.StatusCode = resp.StatusCode
	logRow.Streaming = streaming
	if isErr {
		msg := fmt.Sprintf("upstream provider %q returned HTTP %d after %d attempt(s)",
			prov.Name, resp.StatusCode, attempts)
		if snippet := strings.Join(strings.Fields(string(head)), " "); snippet != "" {
			msg += ": " + snippet
		}
		logRow.Error = msg
	}
	if gotByte {
		logRow.TTFTMillis = ttft.Milliseconds()
	}
	logRow.DurationMillis = time.Since(start).Milliseconds()
	logRow.InputTokens = u.InputTokens
	logRow.OutputTokens = u.OutputTokens
	logRow.CacheReadTokens = u.CacheReadTokens
	logRow.CacheWriteTokens = u.CacheWriteTokens
	logRow.Cost = p.prices.Cost(u)
	p.save(logRow)
}

func (p *Proxy) save(row *store.RequestLog) {
	if err := p.store.InsertLog(row); err != nil {
		p.logger.Error("failed to write request log", "err", err)
	}
}

// streamBody copies upstream->client, flushing each chunk (for SSE liveness), tees bytes
// into the usage accumulator, and reports time-to-first-byte measured from reqStart. When
// captureLimit > 0 it also returns up to that many leading bytes of the body (used to log
// the upstream's error payload).
func streamBody(w http.ResponseWriter, body io.Reader, acc *usage.Accumulator, reqStart time.Time, captureLimit int) (ttft time.Duration, gotByte bool, head []byte) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if !gotByte {
				ttft = time.Since(reqStart)
				gotByte = true
			}
			_, _ = acc.Write(buf[:n])
			if captureLimit > 0 && len(head) < captureLimit {
				room := captureLimit - len(head)
				if room > n {
					room = n
				}
				head = append(head, buf[:room]...)
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return ttft, gotByte, head
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return ttft, gotByte, head
		}
	}
}

func presentedKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if h := r.Header.Get("X-Api-Key"); h != "" {
		return strings.TrimSpace(h)
	}
	return ""
}

func copyRequestHeaders(req *http.Request, src *http.Request, prov *config.Provider) {
	for k, vv := range src.Header {
		ck := http.CanonicalHeaderKey(k)
		// Strip hop-by-hop headers, the client's Authorization (replaced below), and the
		// gateway's own X-AGL-* control headers so they never reach the upstream provider.
		if hopByHop[ck] || ck == "Authorization" || strings.HasPrefix(ck, controlHeaderPrefix) {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	} else {
		req.Header.Del("Authorization")
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}

// litellmTagBugMarker is a substring of a known LiteLLM bug: it intermittently rejects a
// valid request with a spurious 401 about the model's tag configuration (e.g. "Not allowed
// to access model due to tags configuration. Passed model=… and tags=[…]"). The request
// succeeds on retry, so we treat that specific 401 as transient.
const litellmTagBugMarker = "due to tags configuration"

// azureUnsupportedMarkers are substrings of a known LiteLLM/Azure flake: a valid request is
// intermittently rejected with a 400 "litellm.BadRequestError: AzureException - The requested
// operation is unsupported.. Received Model Group=… Available Model Group Fallbacks=None". It
// succeeds on retry, so we treat that specific 400 as transient. Both markers must be present
// so we only retry the LiteLLM-originated variant, not an unrelated provider 400.
var azureUnsupportedMarkers = [][]byte{
	[]byte("litellm"),
	[]byte("The requested operation is unsupported"),
}

// retryableStatus reports whether a status code is transient and worth retrying on its own:
// request timeout (408), rate limit (429), and any 5xx. These are also the statuses reported
// to the client as a provider-side failure when they survive the retry loop.
func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500
}

// retryable reports whether an upstream response should be retried. 408, 429 and 5xx always
// are. A 401 is retried only when its body matches the known LiteLLM tag-config bug, and a
// 400 only when it matches the known LiteLLM/Azure "operation is unsupported" flake.
// Detecting either requires reading the body, so the consumed bytes are restored to resp.Body
// (via a MultiReader) for the pass-through path when we decide not to retry.
func retryable(resp *http.Response) bool {
	if retryableStatus(resp.StatusCode) {
		return true
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusBadRequest {
		return false
	}
	orig := resp.Body
	head, _ := io.ReadAll(io.LimitReader(orig, maxErrorBodyCapture))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), orig), orig}

	if resp.StatusCode == http.StatusUnauthorized {
		return bytes.Contains(head, []byte(litellmTagBugMarker))
	}
	for _, m := range azureUnsupportedMarkers {
		if !bytes.Contains(head, m) {
			return false
		}
	}
	return true
}

// sleepBackoff waits for an exponential backoff with full jitter. It returns false if the
// context is cancelled during the wait.
func sleepBackoff(ctx context.Context, r config.Retry, attempt int) bool {
	d := r.BaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= r.MaxDelay {
			d = r.MaxDelay
			break
		}
	}
	if d <= 0 {
		d = r.BaseDelay
	}
	// Full jitter: sleep a random duration in [0, d].
	wait := time.Duration(rand.Int64N(int64(d) + 1))
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}
