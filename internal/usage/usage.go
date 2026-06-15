// Package usage performs best-effort extraction of model name and token usage from
// provider request/response payloads. It understands the common shapes of the OpenAI
// Chat Completions, OpenAI Responses, and Anthropic Messages APIs (streaming and not)
// without being coupled to any specific endpoint.
package usage

import (
	"bytes"
	"encoding/json"

	"github.com/kiki/agl-gateway/internal/pricing"
)

// MaxBufferBytes caps how much of a non-streaming response body is retained for parsing.
const MaxBufferBytes = 1 << 20 // 1 MiB

// RequestModel returns the "model" field from a JSON request body, or "" if absent.
func RequestModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// rawUsage mirrors the union of usage fields across providers. Pointers distinguish
// "absent" from "zero".
type rawUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`

	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`

	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

// usageEnvelope locates a usage object at any of the well-known nesting points:
// top-level (chat completions, anthropic non-stream, openai responses non-stream),
// response.usage (openai responses streaming), or message.usage (anthropic message_start).
type usageEnvelope struct {
	Usage    *rawUsage `json:"usage"`
	Response *struct {
		Usage *rawUsage `json:"usage"`
	} `json:"response"`
	Message *struct {
		Usage *rawUsage `json:"usage"`
	} `json:"message"`
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// normalize converts a raw usage object into the gateway's non-overlapping Usage buckets.
func normalize(r *rawUsage) (u pricing.Usage, any bool) {
	cacheWrite := deref(r.CacheCreationInputTokens)

	var cacheRead int
	subtractCache := false // OpenAI counts cached tokens *within* the input total
	switch {
	case r.PromptTokensDetails != nil && r.PromptTokensDetails.CachedTokens != nil:
		cacheRead = *r.PromptTokensDetails.CachedTokens
		subtractCache = true
	case r.InputTokensDetails != nil && r.InputTokensDetails.CachedTokens != nil:
		cacheRead = *r.InputTokensDetails.CachedTokens
		subtractCache = true
	case r.CacheReadInputTokens != nil: // Anthropic counts cache reads separately
		cacheRead = *r.CacheReadInputTokens
	}

	output := deref(r.CompletionTokens)
	if output == 0 {
		output = deref(r.OutputTokens)
	}

	input := deref(r.PromptTokens)
	if input == 0 {
		input = deref(r.InputTokens)
	}
	if subtractCache {
		input -= cacheRead
	}
	if input < 0 {
		input = 0
	}

	u = pricing.Usage{
		InputTokens:      input,
		OutputTokens:     output,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
	}
	any = u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0
	return u, any
}

// findUsage parses one JSON document and returns the normalized usage it contains, if any.
func findUsage(doc []byte) (pricing.Usage, bool) {
	var env usageEnvelope
	if err := json.Unmarshal(doc, &env); err != nil {
		return pricing.Usage{}, false
	}
	for _, ru := range []*rawUsage{env.Usage, nested(env.Response), nested(env.Message)} {
		if ru == nil {
			continue
		}
		if u, ok := normalize(ru); ok {
			return u, true
		}
	}
	return pricing.Usage{}, false
}

func nested(s *struct {
	Usage *rawUsage `json:"usage"`
}) *rawUsage {
	if s == nil {
		return nil
	}
	return s.Usage
}

// Accumulator extracts usage from a response body fed to it in arbitrary chunks via Write.
// For streaming bodies it parses SSE `data:` lines incrementally; for non-streaming bodies
// it retains up to MaxBufferBytes and parses the whole document on Finalize. Across events,
// usage fields are combined field-wise by maximum, which yields correct totals for
// Anthropic (input/cache in message_start, output in message_delta) and OpenAI alike.
type Accumulator struct {
	streaming bool
	usage     pricing.Usage
	found     bool
	lineBuf   []byte // streaming: bytes of an in-progress line
	bodyBuf   []byte // non-streaming: capped full body
}

// NewAccumulator returns an Accumulator. Pass streaming=true for text/event-stream bodies.
func NewAccumulator(streaming bool) *Accumulator {
	return &Accumulator{streaming: streaming}
}

// Write feeds response bytes. It never errors and always reports len(p) consumed so it can
// be used as the sink of an io.TeeReader.
func (a *Accumulator) Write(p []byte) (int, error) {
	if a.streaming {
		a.lineBuf = append(a.lineBuf, p...)
		for {
			i := bytes.IndexByte(a.lineBuf, '\n')
			if i < 0 {
				break
			}
			line := a.lineBuf[:i]
			a.lineBuf = a.lineBuf[i+1:]
			a.handleLine(line)
		}
	} else if len(a.bodyBuf) < MaxBufferBytes {
		room := MaxBufferBytes - len(a.bodyBuf)
		if len(p) > room {
			a.bodyBuf = append(a.bodyBuf, p[:room]...)
		} else {
			a.bodyBuf = append(a.bodyBuf, p...)
		}
	}
	return len(p), nil
}

func (a *Accumulator) handleLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if u, ok := findUsage(payload); ok {
		a.merge(u)
	}
}

func (a *Accumulator) merge(u pricing.Usage) {
	a.usage.InputTokens = max(a.usage.InputTokens, u.InputTokens)
	a.usage.OutputTokens = max(a.usage.OutputTokens, u.OutputTokens)
	a.usage.CacheReadTokens = max(a.usage.CacheReadTokens, u.CacheReadTokens)
	a.usage.CacheWriteTokens = max(a.usage.CacheWriteTokens, u.CacheWriteTokens)
	a.found = true
}

// Finalize flushes any buffered data and returns the accumulated usage. The boolean is
// false when no usage could be extracted.
func (a *Accumulator) Finalize() (pricing.Usage, bool) {
	if a.streaming {
		if len(a.lineBuf) > 0 {
			a.handleLine(a.lineBuf)
			a.lineBuf = nil
		}
	} else if len(a.bodyBuf) > 0 {
		if u, ok := findUsage(a.bodyBuf); ok {
			a.merge(u)
		}
	}
	return a.usage, a.found
}
