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
const (
	headerProvider    = "X-AGL-Provider"
	headerAttempts    = "X-AGL-Attempts"
	headerErrorSource = "X-AGL-Error-Source"
)

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
	body := map[string]any{"message": msg, "source": source, "attempts": attempts}
	if provider != "" {
		body["provider"] = provider
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
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

	prov := p.pickProvider(key)
	if prov == nil {
		p.logger.Error("key bound to no valid provider", "key_id", key.ID, "providers", key.Providers)
		gatewayError(w, http.StatusBadGateway, sourceGateway, "",
			"agl-gateway: no upstream provider is configured for this API key", 0)
		return
	}

	p.forward(w, r, body, key, prov, model, start)
}

// pickProvider chooses a random provider, among those bound to the key, that still exists
// in the configuration.
func (p *Proxy) pickProvider(key *store.APIKey) *config.Provider {
	valid := make([]*config.Provider, 0, len(key.Providers))
	for _, name := range key.Providers {
		if cp := p.cfg.Provider(name); cp != nil {
			valid = append(valid, cp)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	return valid[rand.IntN(len(valid))]
}

// forward applies model mapping, runs the retry loop, and streams the final response.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte, key *store.APIKey, prov *config.Provider, model string, start time.Time) {
	retry := prov.ResolvedRetry(p.cfg.Defaults.Retry)
	target := strings.TrimRight(prov.BaseURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	// Model mapping: rewrite the request's model to the provider's upstream name.
	sendBody := body
	effectiveModel := model
	mappedModel := ""
	if to, ok := prov.ModelMap[model]; ok && to != "" && to != model {
		if rewritten, done := usage.SetModel(body, to); done {
			sendBody = rewritten
			mappedModel = to
			effectiveModel = to
		}
	}

	logRow := &store.RequestLog{
		APIKeyID: key.ID, KeyName: key.Name, Provider: prov.Name, Model: model, MappedModel: mappedModel,
	}

	var resp *http.Response
	var lastErr error
	attempts := 0
	for attempt := 0; ; attempt++ {
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
		if isRetryable(resp.StatusCode) && attempt < retry.MaxRetries {
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
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
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
		if hopByHop[http.CanonicalHeaderKey(k)] || http.CanonicalHeaderKey(k) == "Authorization" {
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

func isRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
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
