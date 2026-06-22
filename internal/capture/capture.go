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

	chat      openai.ChatCompletionAccumulator
	chatID    string // first seen chunk id, backfilled into id-less chunks
	message   anthropic.Message
	response  responses.Response // typed terminal Response (for usage)
	respRaw   []byte             // clean raw bytes of the terminal Response (for output)
	respText  strings.Builder    // fallback text when no terminal Response arrives
	respFinal bool
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
	switch a.format {
	case FormatOpenAIChat:
		if json.Unmarshal(a.body, &a.chat.ChatCompletion) == nil {
			a.seen = true
		}
	case FormatOpenAIResponses:
		if json.Unmarshal(a.body, &a.response) == nil {
			a.respRaw = append(a.respRaw[:0], a.body...)
			a.respFinal = true
			a.seen = true
		}
	case FormatAnthropicMessages:
		if json.Unmarshal(a.body, &a.message) == nil {
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
}

func (a *Accumulator) handleAnthropic(data []byte) {
	var ev anthropic.MessageStreamEventUnion
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	a.seen = true
	if err := a.message.Accumulate(ev); err != nil {
		a.reason = "anthropic accumulate: " + err.Error()
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
		// clean original bytes for the assembled output.
		a.response = ev.Response
		if raw := ev.Response.RawJSON(); raw != "" {
			a.respRaw = append(a.respRaw[:0], raw...)
			a.respFinal = true
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
		item := map[string]any{"index": ch.Index, "message": m, "finish_reason": nil}
		if ch.FinishReason != "" {
			item["finish_reason"] = ch.FinishReason
		}
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
		content = append(content, anthropicBlock(msg.Content[i]))
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
	return marshal(out, "message")
}

func (a *Accumulator) finalizeResponses() ([]byte, string) {
	if a.respFinal && len(a.respRaw) > 0 {
		// The terminal event already carried the complete, clean Response object.
		return append([]byte(nil), a.respRaw...), ""
	}
	// Fallback: a minimal Response built from accumulated output_text deltas (truncated stream).
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
// minimal shape; other block types still record whatever populated fields they carry.
func anthropicBlock(block anthropic.ContentBlockUnion) json.RawMessage {
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
