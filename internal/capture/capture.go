// Package capture records proxied payload bytes and, for recognized API formats, derives a
// best-effort non-streaming assembly of a streamed response plus its normalized token usage.
//
// Accumulation is delegated to the official provider SDKs (github.com/openai/openai-go/v3 and
// github.com/anthropics/anthropic-sdk-go): the SDK accumulators own the fiddly, drift-prone work
// of merging deltas (partial tool-call JSON, cumulative usage, content-block indexing, citations,
// thinking/signature, the full Responses event set). We then read the accumulated typed objects
// to (a) emit a clean assembled JSON body for inspection and (b) report usage for metering.
//
// It is deliberately best-effort: malformed events are skipped, and Finalize returns a non-empty
// reason when nothing useful could be assembled. Usage never gates on that reason — partial counts
// (e.g. Anthropic input tokens from message_start) are still reported.
package capture

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/agent-lightning/agl-gateway/internal/pricing"
)

// maxBodyBuffer caps how much of a non-streaming response body is retained for parsing.
const maxBodyBuffer = 1 << 20 // 1 MiB

// StreamFormat is the provider API shape inferred from the request path.
type StreamFormat int

const (
	FormatUnknown StreamFormat = iota
	FormatOpenAIChat
	FormatOpenAIResponses
	FormatAnthropicMessages
)

// FormatForPath returns the API format understood for a request path. Unknown paths still have
// their raw response stream recorded by the caller, but are not assembled or SDK-metered.
func FormatForPath(path string) StreamFormat {
	path = strings.TrimRight(path, "/")
	switch path {
	case "/v1/chat/completions", "/v1/chat/completion":
		return FormatOpenAIChat
	case "/v1/responses":
		return FormatOpenAIResponses
	case "/v1/messages":
		return FormatAnthropicMessages
	default:
		return FormatUnknown
	}
}

// String returns the stable identifier logged in the api_type column.
func (f StreamFormat) String() string {
	switch f {
	case FormatOpenAIChat:
		return "openai_chat"
	case FormatOpenAIResponses:
		return "openai_responses"
	case FormatAnthropicMessages:
		return "anthropic_messages"
	default:
		return ""
	}
}

// Accumulator consumes a response body (SSE bytes when streaming, or a full JSON document when
// not) for one recognized format and exposes the assembled JSON (Finalize) and normalized token
// usage (Usage). The zero value is not usable; construct with NewAccumulator.
type Accumulator struct {
	format    StreamFormat
	streaming bool
	parser    sseParser

	body    []byte // non-streaming: capped raw body, parsed once on flush
	seen    bool   // at least one event/document was parsed
	reason  string // first hard-failure reason; once set, assembly gives up
	flushed bool

	chat   openai.ChatCompletionAccumulator
	chatID string // first seen chunk id, backfilled into id-less chunks
	// The SDK chat accumulator discards every unmodeled field, so these re-capture provider
	// extras from the raw chunk bytes for finalizeChat to re-attach (top-level / per-choice /
	// per-message, keyed by choice index).
	chatExtra       map[string]json.RawMessage
	chatChoiceExtra map[int64]map[string]json.RawMessage
	chatMsgExtra    map[int64]map[string]json.RawMessage

	message  anthropic.Message
	response responses.Response // typed terminal Response (for usage)
	// The Anthropic ContentBlockUnion flattens variant fields and exposes no ExtraFields, so —
	// as with chat — provider extras are re-captured from the raw events: top-level keys from
	// message_start, per-block keys from content_block_start (keyed by block index).
	anthropicExtra      map[string]json.RawMessage
	anthropicBlockExtra map[int64]map[string]json.RawMessage

	// assembled holds verbatim clean bytes to emit as-is from Finalize, when available: the
	// untouched non-streaming body (any format), or a Responses terminal event's RawJSON. When
	// empty, Finalize reconstructs from the SDK accumulator instead.
	assembled []byte
	respText  strings.Builder // fallback text when a Responses stream is truncated before a terminal event
}

// NewAccumulator returns an Accumulator for format, or nil for FormatUnknown. Pass streaming=true
// for text/event-stream bodies.
func NewAccumulator(format StreamFormat, streaming bool) *Accumulator {
	if format == FormatUnknown {
		return nil
	}
	return &Accumulator{format: format, streaming: streaming}
}

// Write feeds response bytes. It never errors and always reports len(p) consumed so it can be used
// as the sink of an io.TeeReader.
func (a *Accumulator) Write(p []byte) (int, error) {
	if a.streaming {
		a.parser.write(p, a.handleEvent)
	} else if len(a.body) < maxBodyBuffer {
		room := maxBodyBuffer - len(a.body)
		if len(p) > room {
			a.body = append(a.body, p[:room]...)
		} else {
			a.body = append(a.body, p...)
		}
	}
	return len(p), nil
}

// Usage returns the normalized token usage accumulated so far. The boolean is false when no usage
// could be extracted. It deliberately ignores any assembly failure reason.
func (a *Accumulator) Usage() (pricing.Usage, bool) {
	a.flush()
	switch a.format {
	case FormatOpenAIChat:
		u := a.chat.ChatCompletion.Usage
		cacheRead := int(u.PromptTokensDetails.CachedTokens) // OpenAI counts cached within the input total
		return buckets(int(u.PromptTokens)-cacheRead, int(u.CompletionTokens), cacheRead, 0)
	case FormatOpenAIResponses:
		u := a.response.Usage
		cacheRead := int(u.InputTokensDetails.CachedTokens)
		return buckets(int(u.InputTokens)-cacheRead, int(u.OutputTokens), cacheRead, 0)
	case FormatAnthropicMessages:
		u := a.message.Usage // Anthropic counts cache reads/writes separately from input
		return buckets(int(u.InputTokens), int(u.OutputTokens), int(u.CacheReadInputTokens), int(u.CacheCreationInputTokens))
	}
	return pricing.Usage{}, false
}

// buckets assembles the non-overlapping Usage buckets, clamping a negative input (which can occur
// when cached tokens are subtracted) to zero.
func buckets(input, output, cacheRead, cacheWrite int) (pricing.Usage, bool) {
	if input < 0 {
		input = 0
	}
	u := pricing.Usage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite}
	return u, input > 0 || output > 0 || cacheRead > 0 || cacheWrite > 0
}

// Finalize flushes any pending input and returns the assembled JSON response. The string is empty
// on success, or a short failure reason when the stream could not be assembled.
func (a *Accumulator) Finalize() (data []byte, reason string) {
	a.flush()
	if a.reason != "" {
		return nil, a.reason
	}
	// When clean bytes are available verbatim (a non-streaming body, or a Responses terminal
	// event), emit them as-is — no lossy round-trip through the typed struct, and provider extra
	// fields survive untouched. Only streamed Chat/Anthropic (and truncated Responses) reconstruct.
	if len(a.assembled) > 0 {
		return append([]byte(nil), a.assembled...), ""
	}
	if !a.seen {
		return nil, "no assemblable events"
	}
	switch a.format {
	case FormatOpenAIChat:
		return a.finalizeChat()
	case FormatOpenAIResponses:
		return a.finalizeResponses()
	case FormatAnthropicMessages:
		return a.finalizeAnthropic()
	}
	return nil, "unknown format"
}

// flush is idempotent: it drains the SSE parser (streaming) or parses the buffered body once
// (non-streaming) into the SDK typed object.
func (a *Accumulator) flush() {
	if a.flushed {
		return
	}
	a.flushed = true
	if a.streaming {
		a.parser.flush(a.handleEvent)
		return
	}
	if len(a.body) == 0 {
		return
	}
	// Non-streaming: the body is already a clean, complete document. Parse it into the typed
	// struct for usage, but emit the original bytes verbatim (assembled) rather than rebuilding.
	switch a.format {
	case FormatOpenAIChat:
		if json.Unmarshal(a.body, &a.chat.ChatCompletion) == nil {
			a.assembled = a.body
			a.seen = true
		}
	case FormatOpenAIResponses:
		if json.Unmarshal(a.body, &a.response) == nil {
			a.assembled = a.body
			a.seen = true
		}
	case FormatAnthropicMessages:
		if json.Unmarshal(a.body, &a.message) == nil {
			a.assembled = a.body
			a.seen = true
		}
	}
}

func (a *Accumulator) handleEvent(event string, data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	if a.reason != "" {
		return
	}
	switch a.format {
	case FormatOpenAIChat:
		a.handleChat(data)
	case FormatOpenAIResponses:
		a.handleResponses(data)
	case FormatAnthropicMessages:
		a.handleAnthropic(data)
	}
}

// Assembly strategy differs by format because the provider stream shapes differ:
//
//   - Chat Completions and Anthropic Messages stream *fragment deltas* — partial content,
//     tool-call argument JSON split across chunks, content-block indices, cumulative usage.
//     Merging those correctly is fiddly and drift-prone, so we delegate to the official SDK
//     accumulators (openai.ChatCompletionAccumulator.AddChunk, anthropic.Message.Accumulate)
//     in handleChat / handleAnthropic.
//   - The Responses SDK ships no accumulator and needs none: every terminal event
//     (response.completed|incomplete|failed) carries the entire final Response object, so
//     assembly collapses to keeping the last terminal Response and its RawJSON() (handleResponses).
//     respText is only a fallback for a stream truncated before any terminal event arrives.
//
// Provider-custom ("extra") fields follow the same asymmetry: the OpenAI Chat accumulator
// discards every unmodeled field, so handleChat re-captures unknown top-level / per-choice /
// per-delta keys from the raw chunk bytes (chatExtra et al.); the Anthropic accumulator keeps
// them in JSON.ExtraFields, which finalizeAnthropic/anthropicBlock read directly.
func (a *Accumulator) handleChat(data []byte) {
	var chunk openai.ChatCompletionChunk
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	a.seen = true
	// The SDK accumulator drops chunks whose id disagrees with the first; some providers omit the
	// id on the trailing usage-only chunk, so backfill the established id to keep that chunk.
	if chunk.ID == "" {
		chunk.ID = a.chatID
	} else {
		a.chatID = chunk.ID
	}
	a.chat.AddChunk(chunk) // false is a soft skip (e.g. mismatched id); never fatal
	a.captureChatExtras(data)
}

// captureChatExtras records provider fields the SDK accumulator does not model, from one raw
// chunk. Top-level, per-choice, and per-delta unknown keys are kept (last non-null value wins,
// keyed by choice index) so finalizeChat can re-attach them to the assembled body.
func (a *Accumulator) captureChatExtras(data []byte) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	for k, v := range raw {
		if chatTopKeys[k] || isNullRaw(v) {
			continue
		}
		if a.chatExtra == nil {
			a.chatExtra = map[string]json.RawMessage{}
		}
		a.chatExtra[k] = v
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(raw["choices"], &choices) != nil {
		return
	}
	for _, ch := range choices {
		idx := rawIndex(ch["index"])
		for k, v := range ch {
			if chatChoiceKeys[k] || isNullRaw(v) {
				continue
			}
			a.chatChoiceExtra = putExtra(a.chatChoiceExtra, idx, k, v)
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(ch["delta"], &delta) != nil {
			continue
		}
		for k, v := range delta {
			if chatDeltaKeys[k] || isNullRaw(v) {
				continue
			}
			a.chatMsgExtra = putExtra(a.chatMsgExtra, idx, k, v)
		}
	}
}

func (a *Accumulator) handleAnthropic(data []byte) {
	var ev anthropic.MessageStreamEventUnion
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	a.seen = true
	if err := a.message.Accumulate(ev); err != nil {
		a.reason = "anthropic accumulate: " + err.Error()
		return
	}
	a.captureAnthropicExtras(data)
}

// captureAnthropicExtras records provider fields the SDK does not model: top-level message keys
// from message_start, and per-block keys from content_block_start (keyed by block index). Both
// carry the original object verbatim, unlike the flattened ContentBlockUnion.
func (a *Accumulator) captureAnthropicExtras(data []byte) {
	var ev map[string]json.RawMessage
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	var typ string
	json.Unmarshal(ev["type"], &typ)
	switch typ {
	case "message_start":
		var msg map[string]json.RawMessage
		if json.Unmarshal(ev["message"], &msg) != nil {
			return
		}
		for k, v := range msg {
			if anthropicMsgKeys[k] || isNullRaw(v) {
				continue
			}
			if a.anthropicExtra == nil {
				a.anthropicExtra = map[string]json.RawMessage{}
			}
			a.anthropicExtra[k] = v
		}
	case "content_block_start":
		var cb map[string]json.RawMessage
		if json.Unmarshal(ev["content_block"], &cb) != nil {
			return
		}
		idx := rawIndex(ev["index"])
		for k, v := range cb {
			if anthropicBlockKeys[k] || isNullRaw(v) {
				continue
			}
			a.anthropicBlockExtra = putExtra(a.anthropicBlockExtra, idx, k, v)
		}
	}
}

func (a *Accumulator) handleResponses(data []byte) {
	var ev responses.ResponseStreamEventUnion
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	a.seen = true
	switch ev.Type {
	case "response.completed", "response.incomplete", "response.failed":
		// Every terminal event carries the complete Response object; keep the last one and its
		// clean original bytes for the assembled output (emitted verbatim by Finalize).
		a.response = ev.Response
		if raw := ev.Response.RawJSON(); raw != "" {
			a.assembled = append(a.assembled[:0], raw...)
		}
	case "response.output_text.delta", "response.refusal.delta":
		a.respText.WriteString(ev.Delta)
	}
}

func (a *Accumulator) finalizeChat() ([]byte, string) {
	cc := a.chat.ChatCompletion
	choices := make([]map[string]any, 0, len(cc.Choices))
	for _, ch := range cc.Choices {
		msg := ch.Message
		role := string(msg.Role)
		if role == "" {
			role = "assistant"
		}
		m := map[string]any{"role": role}
		hasExtras := len(msg.ToolCalls) > 0 || msg.Refusal != ""
		if msg.Content != "" || !hasExtras {
			// Faithful to OpenAI: content is null when only tool_calls/refusal are present.
			m["content"] = msg.Content
		} else {
			m["content"] = nil
		}
		if msg.Refusal != "" {
			m["refusal"] = msg.Refusal
		}
		if len(msg.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				typ := tc.Type
				if typ == "" {
					typ = "function"
				}
				call := map[string]any{
					"type":     typ,
					"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
				}
				if tc.ID != "" {
					call["id"] = tc.ID
				}
				calls = append(calls, call)
			}
			m["tool_calls"] = calls
		}
		mergeExtras(m, a.chatMsgExtra[ch.Index])
		item := map[string]any{"index": ch.Index, "message": m, "finish_reason": nil}
		if ch.FinishReason != "" {
			item["finish_reason"] = ch.FinishReason
		}
		mergeExtras(item, a.chatChoiceExtra[ch.Index])
		choices = append(choices, item)
	}
	out := map[string]any{"object": "chat.completion", "choices": choices}
	if cc.ID != "" {
		out["id"] = cc.ID
	}
	if cc.Created != 0 {
		out["created"] = cc.Created
	}
	if cc.Model != "" {
		out["model"] = cc.Model
	}
	if u := faithfulUsage(cc.Usage, cc.Usage.PromptTokens != 0 || cc.Usage.CompletionTokens != 0 || cc.Usage.TotalTokens != 0); u != nil {
		out["usage"] = u
	}
	mergeExtras(out, a.chatExtra)
	return marshal(out, "chat")
}

func (a *Accumulator) finalizeAnthropic() ([]byte, string) {
	msg := a.message
	role := string(msg.Role)
	if role == "" {
		role = "assistant"
	}
	content := make([]json.RawMessage, 0, len(msg.Content))
	for i := range msg.Content {
		content = append(content, anthropicBlock(msg.Content[i], a.anthropicBlockExtra[int64(i)]))
	}
	out := map[string]any{"type": "message", "role": role, "content": content}
	if msg.ID != "" {
		out["id"] = msg.ID
	}
	if msg.Model != "" {
		out["model"] = string(msg.Model)
	}
	if msg.StopReason != "" {
		out["stop_reason"] = string(msg.StopReason)
	}
	if msg.StopSequence != "" {
		out["stop_sequence"] = msg.StopSequence
	}
	any := msg.Usage.InputTokens != 0 || msg.Usage.OutputTokens != 0 ||
		msg.Usage.CacheReadInputTokens != 0 || msg.Usage.CacheCreationInputTokens != 0
	if u := faithfulUsage(msg.Usage, any); u != nil {
		out["usage"] = u
	}
	mergeExtras(out, a.anthropicExtra)
	return marshal(out, "message")
}

func (a *Accumulator) finalizeResponses() ([]byte, string) {
	// Reached only when no terminal event carried a complete Response (a truncated stream); the
	// happy path returns that Response's RawJSON verbatim via Finalize's assembled fast path.
	// Build a minimal Response from the accumulated output_text deltas.
	out := map[string]any{
		"object": "response",
		"status": "incomplete",
		"output": []map[string]any{{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": a.respText.String(),
			}},
		}},
	}
	return marshal(out, "response")
}

// anthropicBlock renders one accumulated content block as clean JSON. It reads the union's
// exported fields directly (which the SDK mutates in place as deltas arrive) rather than AsAny(),
// whose typed view is only refreshed on content_block_stop. Common variants get an explicit
// minimal shape; other block types still record whatever populated fields they carry. Any
// provider-custom keys captured from content_block_start (extras) are merged in last.
func anthropicBlock(block anthropic.ContentBlockUnion, extras map[string]json.RawMessage) json.RawMessage {
	m := map[string]any{"type": block.Type}
	switch block.Type {
	case "text":
		m["text"] = block.Text
		if len(block.Citations) > 0 {
			if cb, err := json.Marshal(block.Citations); err == nil {
				m["citations"] = json.RawMessage(cb)
			}
		}
	case "thinking":
		m["thinking"] = block.Thinking
		if block.Signature != "" {
			m["signature"] = block.Signature
		}
	case "redacted_thinking":
		m["data"] = block.Data
	case "tool_use", "server_tool_use":
		if block.ID != "" {
			m["id"] = block.ID
		}
		if block.Name != "" {
			m["name"] = block.Name
		}
		m["input"] = toolInput(block.Input)
	default:
		// Comprehensive fallback: emit every populated scalar field of the block.
		if block.Text != "" {
			m["text"] = block.Text
		}
		if block.ID != "" {
			m["id"] = block.ID
		}
		if block.Name != "" {
			m["name"] = block.Name
		}
		if block.Data != "" {
			m["data"] = block.Data
		}
		if block.ToolUseID != "" {
			m["tool_use_id"] = block.ToolUseID
		}
		if len(block.Input) > 0 && string(block.Input) != "null" {
			m["input"] = toolInput(block.Input)
		}
	}
	mergeExtras(m, extras)
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

// toolInput returns the accumulated tool-call input as a JSON object, defaulting to {} when the
// partial JSON never resolved to anything valid.
func toolInput(raw json.RawMessage) any {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 || !json.Valid(s) {
		return map[string]any{}
	}
	return json.RawMessage(append([]byte(nil), s...))
}

// faithfulUsage re-marshals an SDK usage struct (which carries only real provider usage fields) as
// raw JSON, or returns nil when present is false so an empty usage object is omitted.
func faithfulUsage(v any, present bool) json.RawMessage {
	if !present {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// Known (SDK-modeled) keys at each level of the recognized formats. Any key not listed is treated
// as a provider extra and re-captured for the assembled output.
var (
	// system_fingerprint and service_tier are intentionally absent: the SDK models them but
	// system_fingerprint is deprecated, so rather than special-case the typed accessors we let both
	// ride the extra-field passthrough below — captured from the raw chunks and re-emitted as-is.
	chatTopKeys        = set("id", "object", "created", "model", "choices", "usage")
	chatChoiceKeys     = set("index", "delta", "finish_reason", "logprobs")
	chatDeltaKeys      = set("role", "content", "refusal", "tool_calls", "function_call")
	anthropicMsgKeys   = set("id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage")
	anthropicBlockKeys = set("type", "text", "citations", "signature", "thinking", "data", "id", "input", "name", "content", "tool_use_id")
)

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// mergeExtras copies raw extra JSON fields into m, skipping any key already set by the canonical
// reconstruction (canonical wins) and any empty/invalid raw value.
func mergeExtras(m map[string]any, extras map[string]json.RawMessage) {
	for k, raw := range extras {
		if _, taken := m[k]; taken {
			continue
		}
		if len(raw) == 0 || !json.Valid(raw) {
			continue
		}
		m[k] = raw
	}
}

// putExtra stores v under outer[idx][key], lazily allocating, and returns the (possibly new) outer
// map. Later non-null writes overwrite earlier ones, so the last seen value wins.
func putExtra(outer map[int64]map[string]json.RawMessage, idx int64, key string, v json.RawMessage) map[int64]map[string]json.RawMessage {
	if outer == nil {
		outer = map[int64]map[string]json.RawMessage{}
	}
	inner := outer[idx]
	if inner == nil {
		inner = map[string]json.RawMessage{}
		outer[idx] = inner
	}
	inner[key] = v
	return outer
}

// isNullRaw reports whether a raw JSON value is absent or literal null — such values are skipped so
// a key present only as null in one delta does not clobber a real value from another.
func isNullRaw(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) == 0 || bytes.Equal(s, []byte("null"))
}

// rawIndex parses a numeric JSON value (a choice/content-block index), defaulting to 0.
func rawIndex(raw json.RawMessage) int64 {
	var i int64
	json.Unmarshal(raw, &i)
	return i
}

func marshal(v any, what string) ([]byte, string) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, "marshal " + what + ": " + err.Error()
	}
	return b, ""
}

type sseParser struct {
	lineBuf   []byte
	event     string
	dataLines [][]byte
}

func (p *sseParser) write(b []byte, emit func(string, []byte)) {
	p.lineBuf = append(p.lineBuf, b...)
	for {
		i := bytes.IndexByte(p.lineBuf, '\n')
		if i < 0 {
			break
		}
		line := p.lineBuf[:i]
		p.lineBuf = p.lineBuf[i+1:]
		p.handleLine(line, emit)
	}
}

func (p *sseParser) flush(emit func(string, []byte)) {
	if len(p.lineBuf) > 0 {
		p.handleLine(p.lineBuf, emit)
		p.lineBuf = nil
	}
	p.emit(emit)
}

func (p *sseParser) handleLine(line []byte, emit func(string, []byte)) {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		p.emit(emit)
		return
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		p.event = strings.TrimSpace(string(line[len("event:"):]))
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		data := line[len("data:"):]
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		cp := append([]byte(nil), data...)
		p.dataLines = append(p.dataLines, cp)
	}
}

func (p *sseParser) emit(emit func(string, []byte)) {
	if len(p.dataLines) == 0 {
		p.event = ""
		return
	}
	data := bytes.Join(p.dataLines, []byte("\n"))
	emit(p.event, data)
	p.event = ""
	p.dataLines = nil
}
