// Package capture records proxied payload bytes and best-effort assembled streaming
// responses for inspection.
package capture

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// StreamFormat is the provider stream shape inferred from the request path.
type StreamFormat int

const (
	FormatUnknown StreamFormat = iota
	FormatOpenAIChat
	FormatOpenAIResponses
	FormatAnthropicMessages
)

// FormatForPath returns the streaming format understood for a request path. Unknown
// paths still have their raw response stream recorded by the caller.
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

// StreamAssembler consumes SSE bytes and produces a non-streaming JSON response in the
// matching API family. It is deliberately best-effort: malformed or unknown events are
// ignored, and Finalize returns ok=false when nothing useful could be assembled.
type StreamAssembler struct {
	format StreamFormat
	parser sseParser

	chat      chatState
	responses responsesState
	anthropic anthropicState
}

// NewStreamAssembler returns an assembler for format, or nil for FormatUnknown.
func NewStreamAssembler(format StreamFormat) *StreamAssembler {
	if format == FormatUnknown {
		return nil
	}
	return &StreamAssembler{format: format}
}

// Write feeds raw SSE bytes to the assembler.
func (a *StreamAssembler) Write(p []byte) (int, error) {
	a.parser.write(p, a.handleEvent)
	return len(p), nil
}

// Finalize flushes any pending SSE event and returns the assembled JSON response.
func (a *StreamAssembler) Finalize() ([]byte, bool) {
	a.parser.flush(a.handleEvent)
	switch a.format {
	case FormatOpenAIChat:
		return a.chat.finalize()
	case FormatOpenAIResponses:
		return a.responses.finalize()
	case FormatAnthropicMessages:
		return a.anthropic.finalize()
	default:
		return nil, false
	}
}

func (a *StreamAssembler) handleEvent(event string, data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	switch a.format {
	case FormatOpenAIChat:
		a.chat.handle(data)
	case FormatOpenAIResponses:
		a.responses.handle(event, data)
	case FormatAnthropicMessages:
		a.anthropic.handle(data)
	}
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

type chatState struct {
	seen              bool
	id                string
	created           int64
	model             string
	systemFingerprint string
	usage             json.RawMessage
	choices           map[int]*chatChoice
}

type chatChoice struct {
	index        int
	role         string
	content      strings.Builder
	refusal      strings.Builder
	toolCalls    map[int]*chatToolCall
	toolOrder    []int
	finishReason json.RawMessage
}

type chatToolCall struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

type chatChunk struct {
	ID                string `json:"id"`
	Created           int64  `json:"created"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint"`
	Choices           []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Refusal   json.RawMessage `json:"refusal"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason json.RawMessage `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func (s *chatState) handle(data []byte) {
	var c chatChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return
	}
	s.seen = true
	if c.ID != "" {
		s.id = c.ID
	}
	if c.Created != 0 {
		s.created = c.Created
	}
	if c.Model != "" {
		s.model = c.Model
	}
	if c.SystemFingerprint != "" {
		s.systemFingerprint = c.SystemFingerprint
	}
	if len(c.Usage) > 0 && string(c.Usage) != "null" {
		s.usage = append(s.usage[:0], c.Usage...)
	}
	if s.choices == nil {
		s.choices = map[int]*chatChoice{}
	}
	for _, ch := range c.Choices {
		dst := s.choices[ch.Index]
		if dst == nil {
			dst = &chatChoice{index: ch.Index}
			s.choices[ch.Index] = dst
		}
		if ch.Delta.Role != "" {
			dst.role = ch.Delta.Role
		}
		if text, ok := rawJSONString(ch.Delta.Content); ok {
			dst.content.WriteString(text)
		}
		if text, ok := rawJSONString(ch.Delta.Refusal); ok {
			dst.refusal.WriteString(text)
		}
		for _, tc := range ch.Delta.ToolCalls {
			if dst.toolCalls == nil {
				dst.toolCalls = map[int]*chatToolCall{}
			}
			call := dst.toolCalls[tc.Index]
			if call == nil {
				call = &chatToolCall{}
				dst.toolCalls[tc.Index] = call
				dst.toolOrder = append(dst.toolOrder, tc.Index)
			}
			if tc.ID != "" {
				call.id = tc.ID
			}
			if tc.Type != "" {
				call.typ = tc.Type
			}
			if tc.Function.Name != "" {
				call.name = tc.Function.Name
			}
			call.args.WriteString(tc.Function.Arguments)
		}
		if len(ch.FinishReason) > 0 && string(ch.FinishReason) != "null" {
			dst.finishReason = append(dst.finishReason[:0], ch.FinishReason...)
		}
	}
}

func (s *chatState) finalize() ([]byte, bool) {
	if !s.seen {
		return nil, false
	}
	out := map[string]any{
		"object":  "chat.completion",
		"choices": s.finalChoices(),
	}
	if s.id != "" {
		out["id"] = s.id
	}
	if s.created != 0 {
		out["created"] = s.created
	}
	if s.model != "" {
		out["model"] = s.model
	}
	if s.systemFingerprint != "" {
		out["system_fingerprint"] = s.systemFingerprint
	}
	if len(s.usage) > 0 {
		out["usage"] = s.usage
	}
	b, err := json.Marshal(out)
	return b, err == nil
}

func (s *chatState) finalChoices() []map[string]any {
	indexes := make([]int, 0, len(s.choices))
	for idx := range s.choices {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	out := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		ch := s.choices[idx]
		role := ch.role
		if role == "" {
			role = "assistant"
		}
		message := map[string]any{"role": role}
		hasExtras := len(ch.toolCalls) > 0 || ch.refusal.Len() > 0
		if ch.content.Len() > 0 || !hasExtras {
			// Faithful to OpenAI: content is null when only tool_calls/refusal are present.
			message["content"] = ch.content.String()
		} else {
			message["content"] = nil
		}
		if ch.refusal.Len() > 0 {
			message["refusal"] = ch.refusal.String()
		}
		if len(ch.toolCalls) > 0 {
			message["tool_calls"] = ch.finalToolCalls()
		}
		item := map[string]any{
			"index":         idx,
			"message":       message,
			"finish_reason": nil,
		}
		if len(ch.finishReason) > 0 {
			item["finish_reason"] = ch.finishReason
		}
		out = append(out, item)
	}
	return out
}

func (ch *chatChoice) finalToolCalls() []map[string]any {
	order := append([]int(nil), ch.toolOrder...)
	sort.Ints(order)
	calls := make([]map[string]any, 0, len(order))
	for _, idx := range order {
		tc := ch.toolCalls[idx]
		typ := tc.typ
		if typ == "" {
			typ = "function"
		}
		call := map[string]any{
			"type": typ,
			"function": map[string]any{
				"name":      tc.name,
				"arguments": tc.args.String(),
			},
		}
		if tc.id != "" {
			call["id"] = tc.id
		}
		calls = append(calls, call)
	}
	return calls
}

type responsesState struct {
	seen          bool
	finalResponse json.RawMessage
	text          strings.Builder
	usage         json.RawMessage
}

func (s *responsesState) handle(event string, data []byte) {
	var e struct {
		Type     string          `json:"type"`
		Delta    string          `json:"delta"`
		Text     string          `json:"text"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return
	}
	s.seen = true
	if e.Type == "" {
		e.Type = event
	}
	if len(e.Response) > 0 && string(e.Response) != "null" {
		var probe struct {
			Usage json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(e.Response, &probe)
		if len(probe.Usage) > 0 && string(probe.Usage) != "null" {
			s.usage = append(s.usage[:0], probe.Usage...)
		}
		// All three terminal events carry the complete final response object; keep the
		// last one seen.
		switch e.Type {
		case "response.completed", "response.incomplete", "response.failed":
			s.finalResponse = append(s.finalResponse[:0], e.Response...)
		}
	}
	switch e.Type {
	case "response.output_text.delta", "response.refusal.delta":
		s.text.WriteString(e.Delta)
	case "response.output_text.done":
		if s.text.Len() == 0 && e.Text != "" {
			s.text.WriteString(e.Text)
		}
	}
}

func (s *responsesState) finalize() ([]byte, bool) {
	if len(s.finalResponse) > 0 {
		return append([]byte(nil), s.finalResponse...), true
	}
	if !s.seen {
		return nil, false
	}
	out := map[string]any{
		"object": "response",
		"output": []map[string]any{{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": s.text.String(),
			}},
		}},
	}
	if len(s.usage) > 0 {
		out["usage"] = s.usage
	}
	b, err := json.Marshal(out)
	return b, err == nil
}

type anthropicState struct {
	seen         bool
	id           string
	role         string
	model        string
	stopReason   string
	stopSequence *string
	usage        map[string]json.RawMessage
	blocks       map[int]*anthropicBlock
}

type anthropicBlock struct {
	index       int
	typ         string
	id          string
	name        string
	text        strings.Builder
	thinking    strings.Builder
	signature   strings.Builder
	partialJSON strings.Builder
	data        string            // redacted_thinking payload
	citations   []json.RawMessage // citations_delta accumulations
}

func (s *anthropicState) handle(data []byte) {
	var e struct {
		Type    string `json:"type"`
		Message *struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Model   string          `json:"model"`
			Usage   json.RawMessage `json:"usage"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		Index        int `json:"index"`
		ContentBlock *struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Text  string          `json:"text"`
			Data  string          `json:"data"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
		Delta *struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			Thinking     string          `json:"thinking"`
			Signature    string          `json:"signature"`
			PartialJSON  string          `json:"partial_json"`
			Citation     json.RawMessage `json:"citation"`
			StopReason   string          `json:"stop_reason"`
			StopSequence *string         `json:"stop_sequence"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return
	}
	s.seen = true
	if s.blocks == nil {
		s.blocks = map[int]*anthropicBlock{}
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.id = e.Message.ID
			s.role = e.Message.Role
			s.model = e.Message.Model
			s.mergeUsage(e.Message.Usage)
			for i, block := range e.Message.Content {
				dst := s.block(i)
				dst.typ = block.Type
				dst.text.WriteString(block.Text)
			}
		}
	case "content_block_start":
		if e.ContentBlock != nil {
			dst := s.block(e.Index)
			dst.typ = e.ContentBlock.Type
			dst.id = e.ContentBlock.ID
			dst.name = e.ContentBlock.Name
			dst.text.WriteString(e.ContentBlock.Text)
			if e.ContentBlock.Data != "" {
				dst.data = e.ContentBlock.Data
			}
			if len(e.ContentBlock.Input) > 0 && string(e.ContentBlock.Input) != "null" {
				dst.partialJSON.Write(e.ContentBlock.Input)
			}
		}
	case "content_block_delta":
		if e.Delta != nil {
			dst := s.block(e.Index)
			switch e.Delta.Type {
			case "text_delta":
				if dst.typ == "" {
					dst.typ = "text"
				}
				dst.text.WriteString(e.Delta.Text)
			case "thinking_delta":
				if dst.typ == "" {
					dst.typ = "thinking"
				}
				dst.thinking.WriteString(e.Delta.Thinking)
			case "signature_delta":
				dst.signature.WriteString(e.Delta.Signature)
			case "input_json_delta":
				if dst.typ == "" {
					dst.typ = "tool_use"
				}
				dst.partialJSON.WriteString(e.Delta.PartialJSON)
			case "citations_delta":
				if len(e.Delta.Citation) > 0 && string(e.Delta.Citation) != "null" {
					dst.citations = append(dst.citations, append([]byte(nil), e.Delta.Citation...))
				}
			}
		}
	case "message_delta":
		if e.Delta != nil {
			s.stopReason = e.Delta.StopReason
			s.stopSequence = e.Delta.StopSequence
		}
		s.mergeUsage(e.Usage)
	}
}

func (s *anthropicState) block(index int) *anthropicBlock {
	b := s.blocks[index]
	if b == nil {
		b = &anthropicBlock{index: index}
		s.blocks[index] = b
	}
	return b
}

// mergeUsage overlays a usage object onto the accumulated usage, keyed by field name with
// the latest non-null value winning. Usage is kept as raw JSON because modern Anthropic
// usage carries nested objects (cache_creation, output_tokens_details) and strings
// (inference_geo) alongside the integer token counts — a map[string]int would fail to
// decode and silently drop the entire event.
func (s *anthropicState) mergeUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var u map[string]json.RawMessage
	if err := json.Unmarshal(raw, &u); err != nil {
		return
	}
	if s.usage == nil {
		s.usage = map[string]json.RawMessage{}
	}
	for k, v := range u {
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		s.usage[k] = append([]byte(nil), v...)
	}
}

func (s *anthropicState) finalize() ([]byte, bool) {
	if !s.seen {
		return nil, false
	}
	role := s.role
	if role == "" {
		role = "assistant"
	}
	out := map[string]any{
		"type":    "message",
		"role":    role,
		"content": s.finalBlocks(),
	}
	if s.id != "" {
		out["id"] = s.id
	}
	if s.model != "" {
		out["model"] = s.model
	}
	if s.stopReason != "" {
		out["stop_reason"] = s.stopReason
	}
	if s.stopSequence != nil {
		out["stop_sequence"] = *s.stopSequence
	}
	if len(s.usage) > 0 {
		out["usage"] = s.usage
	}
	b, err := json.Marshal(out)
	return b, err == nil
}

func (s *anthropicState) finalBlocks() []map[string]any {
	indexes := make([]int, 0, len(s.blocks))
	for idx := range s.blocks {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	out := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		b := s.blocks[idx]
		typ := b.typ
		if typ == "" {
			typ = "text"
		}
		item := map[string]any{"type": typ}
		switch typ {
		case "tool_use", "server_tool_use":
			if b.id != "" {
				item["id"] = b.id
			}
			if b.name != "" {
				item["name"] = b.name
			}
			raw := strings.TrimSpace(b.partialJSON.String())
			var input any = map[string]any{}
			if raw != "" {
				if json.Valid([]byte(raw)) {
					input = json.RawMessage(raw)
				} else {
					input = raw
				}
			}
			item["input"] = input
		case "thinking":
			item["thinking"] = b.thinking.String()
			if b.signature.Len() > 0 {
				item["signature"] = b.signature.String()
			}
		case "redacted_thinking":
			item["data"] = b.data
		default:
			item["text"] = b.text.String()
			if len(b.citations) > 0 {
				item["citations"] = b.citations
			}
		}
		out = append(out, item)
	}
	return out
}

func rawJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}
