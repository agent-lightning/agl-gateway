package capture

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// assemble feeds streamed SSE chunks and returns the assembled body plus an ok flag (true when no
// failure reason was reported), matching how the proxy treats an empty reason as success.
func assemble(t *testing.T, format StreamFormat, chunks ...string) ([]byte, bool) {
	t.Helper()
	a := NewAccumulator(format, true)
	for _, c := range chunks {
		if _, err := a.Write([]byte(c)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	data, reason := a.Finalize()
	return data, reason == ""
}

func TestFormatForPath(t *testing.T) {
	cases := map[string]StreamFormat{
		"/v1/chat/completions": FormatOpenAIChat,
		"/v1/chat/completion":  FormatOpenAIChat,
		"/v1/responses":        FormatOpenAIResponses,
		"/v1/messages":         FormatAnthropicMessages,
		"/v1/unknown":          FormatUnknown,
		"/v1/responses/extra":  FormatUnknown,
		"/v1/messages/":        FormatAnthropicMessages,
	}
	for path, want := range cases {
		if got := FormatForPath(path); got != want {
			t.Errorf("FormatForPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestStreamFormatString(t *testing.T) {
	cases := map[StreamFormat]string{
		FormatOpenAIChat:        "openai_chat",
		FormatOpenAIResponses:   "openai_responses",
		FormatAnthropicMessages: "anthropic_messages",
		FormatUnknown:           "",
	}
	for f, want := range cases {
		if got := f.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", f, got, want)
		}
	}
}

func TestNewAccumulatorUnknownIsNil(t *testing.T) {
	if a := NewAccumulator(FormatUnknown, true); a != nil {
		t.Errorf("NewAccumulator(FormatUnknown) = %v, want nil", a)
	}
}

func TestAssembleOpenAIChatStream(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":123,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`+"\n\n",
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.ID != "chatcmpl_1" || got.Object != "chat.completion" {
		t.Errorf("identity = %q/%q", got.ID, got.Object)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Role != "assistant" || got.Choices[0].Message.Content != "hello" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", got.Choices)
	}
	if got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// TestAssembleOpenAIChatBackfillsMissingID guards the dirty-data case where a trailing usage-only
// chunk omits the id: the SDK accumulator would otherwise drop it and lose the usage.
func TestAssembleOpenAIChatBackfillsMissingID(t *testing.T) {
	a := NewAccumulator(FormatOpenAIChat, true)
	a.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n"))
	a.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\n"))
	u, ok := a.Usage()
	if !ok || u.InputTokens != 3 || u.OutputTokens != 1 {
		t.Errorf("usage from id-less trailing chunk = %+v ok=%v", u, ok)
	}
}

func TestAssembleOpenAIResponsesPrefersCompletedResponse(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIResponses,
		"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"ignored because completed wins"}`+"\n\n",
		"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","output_text":"final","usage":{"input_tokens":4,"output_tokens":1}}}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		ID         string `json:"id"`
		Object     string `json:"object"`
		OutputText string `json:"output_text"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.ID != "resp_1" || got.Object != "response" || got.OutputText != "final" {
		t.Errorf("response = %+v", got)
	}
	if got.Usage.InputTokens != 4 || got.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func TestAssembleOpenAIResponsesDeltaFallback(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIResponses,
		"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"hel"}`+"\n\n",
		"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"lo"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if len(got.Output) != 1 || len(got.Output[0].Content) != 1 || got.Output[0].Content[0].Text != "hello" {
		t.Errorf("fallback output = %+v", got.Output)
	}
}

func TestAssembleOpenAIResponsesIncompleteIsTerminal(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIResponses,
		"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"partial"}`+"\n\n",
		"event: response.incomplete\n"+
			`data: {"type":"response.incomplete","response":{"id":"resp_1","object":"response","status":"incomplete","output_text":"partial","usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.ID != "resp_1" || got.Status != "incomplete" {
		t.Errorf("incomplete terminal not used: %+v\n%s", got, out)
	}
}

func TestAssembleAnthropicMessagesStream(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":7,"output_tokens":1}}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`+"\n\n",
		"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`+"\n\n",
		"event: message_stop\n"+
			`data: {"type":"message_stop"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.ID != "msg_1" || got.Type != "message" || got.Role != "assistant" || got.Model != "claude" {
		t.Errorf("message identity = %+v", got)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "hello" {
		t.Errorf("content = %+v", got.Content)
	}
	if got.StopReason != "end_turn" || got.Usage.InputTokens != 7 || got.Usage.OutputTokens != 3 {
		t.Errorf("stop/usage = %q/%+v", got.StopReason, got.Usage)
	}
}

// TestAssembleAnthropicModernUsageNotDropped guards that the modern usage shape — nested objects
// (cache_creation, output_tokens_details) and strings (inference_geo) alongside the integer token
// counts — survives accumulation and is faithfully re-emitted by the SDK usage type.
func TestAssembleAnthropicModernUsageNotDropped(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"inference_geo":"global","input_tokens":9,"output_tokens":6}}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n",
		"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":9,"output_tokens":28,"output_tokens_details":{"thinking_tokens":0}}}`+"\n\n",
		"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens   int            `json:"input_tokens"`
			OutputTokens  int            `json:"output_tokens"`
			InferenceGeo  string         `json:"inference_geo"`
			CacheCreation map[string]int `json:"cache_creation"`
		} `json:"usage"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.ID != "msg_1" || got.Model != "claude-opus-4-8" || got.StopReason != "end_turn" {
		t.Errorf("identity/stop lost: id=%q model=%q stop=%q\n%s", got.ID, got.Model, got.StopReason, out)
	}
	// input_tokens from message_start, output_tokens from message_delta (latest wins).
	if got.Usage.InputTokens != 9 || got.Usage.OutputTokens != 28 {
		t.Errorf("usage tokens = in:%d out:%d, want 9/28", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.Usage.InferenceGeo != "global" || got.Usage.CacheCreation == nil {
		t.Errorf("non-int usage fields lost: geo=%q cache_creation=%v", got.Usage.InferenceGeo, got.Usage.CacheCreation)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "hi" {
		t.Errorf("content = %+v", got.Content)
	}
}

func TestAssembleAnthropicThinkingSignatureAndCitations(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ponder"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"see source"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"x","document_index":0,"document_title":"d","start_char_index":0,"end_char_index":1}}}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		Content []struct {
			Type      string            `json:"type"`
			Thinking  string            `json:"thinking"`
			Signature string            `json:"signature"`
			Text      string            `json:"text"`
			Citations []json.RawMessage `json:"citations"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2\n%s", len(got.Content), out)
	}
	if got.Content[0].Type != "thinking" || got.Content[0].Thinking != "ponder" || got.Content[0].Signature != "sig123" {
		t.Errorf("thinking block = %+v", got.Content[0])
	}
	if got.Content[1].Type != "text" || got.Content[1].Text != "see source" || len(got.Content[1].Citations) != 1 {
		t.Errorf("text block = %+v", got.Content[1])
	}
}

func TestAssembleOpenAIChatToolCallsAndRefusal(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl_1","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`+"\n\n",
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SF\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d\n%s", len(got.Choices), out)
	}
	c := got.Choices[0]
	if c.Message.Content != nil {
		t.Errorf("content should be null when only tool_calls present, got %q", *c.Message.Content)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1\n%s", len(c.Message.ToolCalls), out)
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"SF"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", c.FinishReason)
	}
}

func TestAssembleOpenAIChatRefusal(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl_2","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can"}}]}`+"\n\n",
		`data: {"id":"chatcmpl_2","choices":[{"index":0,"delta":{"refusal":"not help"},"finish_reason":"stop"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		Choices []struct {
			Message struct {
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Refusal != "I cannot help" {
		t.Errorf("refusal = %+v\n%s", got.Choices, out)
	}
}

func TestUsageOpenAIChatSubtractsCache(t *testing.T) {
	a := NewAccumulator(FormatOpenAIChat, true)
	a.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
	a.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":6}}}` + "\n\n"))
	u, ok := a.Usage()
	if !ok || u.InputTokens != 4 || u.OutputTokens != 4 || u.CacheReadTokens != 6 || u.CacheWriteTokens != 0 {
		t.Errorf("usage = %+v ok=%v, want input 4 (10-6) output 4 cacheRead 6", u, ok)
	}
}

func TestUsageOpenAIResponsesSubtractsCache(t *testing.T) {
	a := NewAccumulator(FormatOpenAIResponses, true)
	a.Write([]byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","object":"response","usage":{"input_tokens":12,"output_tokens":5,"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"))
	u, ok := a.Usage()
	if !ok || u.InputTokens != 9 || u.OutputTokens != 5 || u.CacheReadTokens != 3 {
		t.Errorf("usage = %+v ok=%v, want input 9 (12-3) output 5 cacheRead 3", u, ok)
	}
}

func TestUsageAnthropicSeparatesCache(t *testing.T) {
	a := NewAccumulator(FormatAnthropicMessages, true)
	a.Write([]byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":9,"cache_read_input_tokens":2,"cache_creation_input_tokens":5,"output_tokens":1}}}` + "\n\n"))
	a.Write([]byte("event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":28}}` + "\n\n"))
	u, ok := a.Usage()
	if !ok || u.InputTokens != 9 || u.OutputTokens != 28 || u.CacheReadTokens != 2 || u.CacheWriteTokens != 5 {
		t.Errorf("usage = %+v ok=%v, want input 9 output 28 cacheRead 2 cacheWrite 5", u, ok)
	}
}

func TestUsageNonStreaming(t *testing.T) {
	a := NewAccumulator(FormatAnthropicMessages, false)
	a.Write([]byte(`{"type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":7}}`))
	u, ok := a.Usage()
	if !ok || u.InputTokens != 5 || u.OutputTokens != 7 {
		t.Errorf("non-streaming usage = %+v ok=%v, want input 5 output 7", u, ok)
	}
}

func TestFinalizeNoAssemblableEvents(t *testing.T) {
	// Nothing written.
	a := NewAccumulator(FormatOpenAIChat, true)
	if _, reason := a.Finalize(); reason != "no assemblable events" {
		t.Errorf("empty stream reason = %q, want %q", reason, "no assemblable events")
	}
	// Only unparseable data.
	b := NewAccumulator(FormatOpenAIChat, true)
	b.Write([]byte("data: not json at all\n\n"))
	if _, reason := b.Finalize(); reason != "no assemblable events" {
		t.Errorf("garbage stream reason = %q, want %q", reason, "no assemblable events")
	}
}

// TestAssembleAnthropicWebSearchToolUse exercises a server_tool_use block (input_json deltas) and
// the web_search_tool_result block from the streaming-docs sample. The result block is an
// otherwise-unmodeled type, so it flows through the comprehensive fallback (type + tool_use_id).
func TestAssembleAnthropicWebSearchToolUse(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":2679,"output_tokens":3}}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{}}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" \"weather NYC\"}"}}`+"\n\n",
		"event: content_block_stop\n"+
			`data: {"type":"content_block_stop","index":0}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_01","content":[{"type":"web_search_result","title":"Weather","url":"https://example.com","encrypted_content":"abc","page_age":null}]}}`+"\n\n",
		"event: content_block_stop\n"+
			`data: {"type":"content_block_stop","index":1}`+"\n\n",
		"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":10682,"output_tokens":510,"server_tool_use":{"web_search_requests":1}}}`+"\n\n",
		"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		Content []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Query string `json:"query"`
			} `json:"input"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2\n%s", len(got.Content), out)
	}
	if got.Content[0].Type != "server_tool_use" || got.Content[0].Name != "web_search" || got.Content[0].Input.Query != "weather NYC" {
		t.Errorf("server_tool_use block = %+v", got.Content[0])
	}
	if got.Content[1].Type != "web_search_tool_result" || got.Content[1].ToolUseID != "srvtoolu_01" {
		t.Errorf("web_search_tool_result fallback = %+v", got.Content[1])
	}
}

func TestFinalizeAnthropicIndexGapGivesUp(t *testing.T) {
	a := NewAccumulator(FormatAnthropicMessages, true)
	// A content_block_start at a non-zero index with no preceding block is an out-of-order gap the
	// SDK rejects; we surface that as a failure reason rather than a bogus assembly.
	a.Write([]byte("event: content_block_start\n" +
		`data: {"type":"content_block_start","index":5,"content_block":{"type":"text","text":""}}` + "\n\n"))
	data, reason := a.Finalize()
	if data != nil || !strings.HasPrefix(reason, "anthropic accumulate:") {
		t.Errorf("index gap: data=%s reason=%q, want nil data + accumulate reason", data, reason)
	}
	// Usage must not panic and reports nothing.
	if u, ok := a.Usage(); ok || u.InputTokens != 0 {
		t.Errorf("usage after failure = %+v ok=%v, want empty", u, ok)
	}
}

// TestAssembleOpenAIChatStreamPreservesExtras verifies provider-custom fields survive streaming
// assembly at every level — the SDK accumulator discards them, so handleChat re-captures top-level,
// per-choice, and per-delta keys from the raw chunk bytes.
func TestAssembleOpenAIChatStreamPreservesExtras(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"m","x_top":{"k":1},"choices":[{"index":0,"delta":{"role":"assistant","content":"hi","x_delta":"dval"},"finish_reason":null,"x_choice":"cval"}]}`+"\n\n",
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		XTop    map[string]int `json:"x_top"`
		Choices []struct {
			XChoice string `json:"x_choice"`
			Message struct {
				Content string `json:"content"`
				XDelta  string `json:"x_delta"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.XTop["k"] != 1 {
		t.Errorf("top-level extra lost: %v\n%s", got.XTop, out)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d\n%s", len(got.Choices), out)
	}
	c := got.Choices[0]
	if c.XChoice != "cval" || c.Message.XDelta != "dval" || c.Message.Content != "hi" {
		t.Errorf("nested extras lost: choice=%q delta=%q content=%q\n%s", c.XChoice, c.Message.XDelta, c.Message.Content, out)
	}
}

// TestAssembleAnthropicStreamPreservesExtras verifies top-level (message_start) and per-block
// (content_block_start) provider-custom fields survive streaming assembly.
func TestAssembleAnthropicStreamPreservesExtras(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"x_msg":"mval","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","x_block":"bval"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n",
		"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		XMsg    string `json:"x_msg"`
		Content []struct {
			Text   string `json:"text"`
			XBlock string `json:"x_block"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.XMsg != "mval" {
		t.Errorf("top-level extra lost: %q\n%s", got.XMsg, out)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "hi" || got.Content[0].XBlock != "bval" {
		t.Errorf("block extra lost: %+v\n%s", got.Content, out)
	}
}

// TestFinalizeNonStreamingReturnsBodyVerbatim confirms a non-streaming response is emitted byte-for-
// byte (the typed struct is internal-only for usage): provider extras and a real content:null
// survive without round-tripping through the manual reconstruction.
func TestFinalizeNonStreamingReturnsBodyVerbatim(t *testing.T) {
	cases := map[StreamFormat]string{
		FormatOpenAIChat:        `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"x_custom":"keep"}`,
		FormatAnthropicMessages: `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"x_custom":"keep"}`,
	}
	for format, body := range cases {
		a := NewAccumulator(format, false)
		a.Write([]byte(body))
		out, reason := a.Finalize()
		if reason != "" {
			t.Errorf("%v: reason = %q, want empty", format, reason)
			continue
		}
		if string(out) != body {
			t.Errorf("%v: not byte-for-byte\n got: %s\nwant: %s", format, out, body)
		}
	}
}

// --- Doc-grounded comprehensive streaming tests -----------------------------------------------
// The event sequences below are taken from the provider streaming reference docs (Anthropic
// "Streaming messages", OpenAI Chat Completions and Responses API references) so the assembler is
// exercised against the real wire format, including ping/error noise and full lifecycle events.

// TestAssembleAnthropicIgnoresPingAndError feeds the documented basic stream interleaved with ping
// events and a mid-stream overloaded error event (anthropic-streaming-messages.md). Unknown events
// must be skipped without failing assembly, and text + cumulative usage must still come through.
func TestAssembleAnthropicIgnoresPingAndError(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4-8","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1}}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n",
		"event: ping\n"+`data: {"type": "ping"}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`+"\n\n",
		"event: error\n"+
			`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}`+"\n\n",
		"event: content_block_stop\n"+`data: {"type":"content_block_stop","index":0}`+"\n\n",
		"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`+"\n\n",
		"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false (ping/error event broke assembly)")
	}
	var got struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "Hello!" || got.StopReason != "end_turn" {
		t.Errorf("text/stop = %+v / %q\n%s", got.Content, got.StopReason, out)
	}
}

// TestAssembleAnthropicDocumentedToolUse replays the canonical get_weather tool_use stream
// (anthropic-streaming-messages.md): a text block followed by a tool_use block whose input arrives
// as partial_json deltas. The assembled tool_use input must parse back to the complete object.
func TestAssembleAnthropicDocumentedToolUse(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_014p7gG3wDgGV9EUtLvnow3U","type":"message","role":"assistant","model":"claude-opus-4-8","stop_sequence":null,"usage":{"input_tokens":472,"output_tokens":2},"content":[],"stop_reason":null}}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Okay, let's check the weather for San Francisco, CA:"}}`+"\n\n",
		"event: content_block_stop\n"+`data: {"type":"content_block_stop","index":0}`+"\n\n",
		"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01T1x1fJ34qAmk2tNTrN7Up6","name":"get_weather","input":{}}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"San"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" Francisc"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"o,"}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" CA\"}"}}`+"\n\n",
		"event: content_block_stop\n"+`data: {"type":"content_block_stop","index":1}`+"\n\n",
		"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":89}}`+"\n\n",
		"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input struct {
				Location string `json:"location"`
			} `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.StopReason != "tool_use" || len(got.Content) != 2 {
		t.Fatalf("stop=%q blocks=%d\n%s", got.StopReason, len(got.Content), out)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "Okay, let's check the weather for San Francisco, CA:" {
		t.Errorf("text block = %+v", got.Content[0])
	}
	tc := got.Content[1]
	if tc.Type != "tool_use" || tc.ID != "toolu_01T1x1fJ34qAmk2tNTrN7Up6" || tc.Name != "get_weather" || tc.Input.Location != "San Francisco, CA" {
		t.Errorf("tool_use block = %+v\n%s", tc, out)
	}
}

// TestAssembleOpenAIChatDocumentedStream replays the documented Chat Completions stream
// (openai-api-chat-completions.md): role-only first chunk, content deltas, a finish chunk, all
// carrying system_fingerprint and logprobs:null, plus a trailing include_usage chunk with an empty
// choices array. The assembled completion must carry content, system_fingerprint, and usage.
func TestAssembleOpenAIChatDocumentedStream(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o-mini","system_fingerprint":"fp_44709d6fcb","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}`+"\n\n",
		`data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o-mini","system_fingerprint":"fp_44709d6fcb","choices":[{"index":0,"delta":{"content":"Hello"},"logprobs":null,"finish_reason":null}]}`+"\n\n",
		`data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o-mini","system_fingerprint":"fp_44709d6fcb","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}`+"\n\n",
		`data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-4o-mini","system_fingerprint":"fp_44709d6fcb","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		Model             string `json:"model"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.Model != "gpt-4o-mini" || got.SystemFingerprint != "fp_44709d6fcb" {
		t.Errorf("identity lost: model=%q fp=%q\n%s", got.Model, got.SystemFingerprint, out)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v\n%s", got.Choices, out)
	}
	if got.Usage.PromptTokens != 9 || got.Usage.CompletionTokens != 1 {
		t.Errorf("usage from trailing empty-choices chunk lost = %+v\n%s", got.Usage, out)
	}
}

// TestAssembleOpenAIResponsesFullLifecycle feeds the complete documented Responses event sequence
// (openai-api-responses.md): created, in_progress, output_item.added, content_part.added,
// output_text.delta, output_text.done, content_part.done, output_item.done, completed. Only the
// terminal response.completed object should drive the assembled output; the rest are ignored.
func TestAssembleOpenAIResponsesFullLifecycle(t *testing.T) {
	const completed = `{"id":"resp_67c9","object":"response","created_at":1741290958,"status":"completed","error":null,"model":"gpt-5.4","output":[{"id":"msg_67c9","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi there! How can I assist you today?","annotations":[]}]}],"usage":{"input_tokens":37,"output_tokens":11,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":48}}`
	out, ok := assemble(t, FormatOpenAIResponses,
		"event: response.created\n"+`data: {"type":"response.created","response":{"id":"resp_67c9","object":"response","status":"in_progress","model":"gpt-5.4","output":[],"usage":null}}`+"\n\n",
		"event: response.in_progress\n"+`data: {"type":"response.in_progress","response":{"id":"resp_67c9","object":"response","status":"in_progress","output":[]}}`+"\n\n",
		"event: response.output_item.added\n"+`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_67c9","type":"message","status":"in_progress","role":"assistant","content":[]}}`+"\n\n",
		"event: response.content_part.added\n"+`data: {"type":"response.content_part.added","item_id":"msg_67c9","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`+"\n\n",
		"event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","item_id":"msg_67c9","output_index":0,"content_index":0,"delta":"Hi there! How can I assist you today?"}`+"\n\n",
		"event: response.output_text.done\n"+`data: {"type":"response.output_text.done","item_id":"msg_67c9","output_index":0,"content_index":0,"text":"Hi there! How can I assist you today?"}`+"\n\n",
		"event: response.content_part.done\n"+`data: {"type":"response.content_part.done","item_id":"msg_67c9","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hi there! How can I assist you today?","annotations":[]}}`+"\n\n",
		"event: response.output_item.done\n"+`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_67c9","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi there! How can I assist you today?","annotations":[]}]}}`+"\n\n",
		"event: response.completed\n"+`data: {"type":"response.completed","response":`+completed+`}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	if string(out) != completed {
		t.Errorf("assembled is not the terminal Response verbatim\n got: %s\nwant: %s", out, completed)
	}
	// And usage is read from that same terminal Response.
	a := NewAccumulator(FormatOpenAIResponses, true)
	a.Write([]byte("event: response.completed\n" + `data: {"type":"response.completed","response":` + completed + `}` + "\n\n"))
	if u, ok := a.Usage(); !ok || u.InputTokens != 37 || u.OutputTokens != 11 {
		t.Errorf("usage = %+v ok=%v, want input 37 output 11", u, ok)
	}
}

// TestAssembleOpenAIResponsesFailedTerminal verifies response.failed is treated as terminal (like
// completed/incomplete) and its Response object drives the assembled output.
func TestAssembleOpenAIResponsesFailedTerminal(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIResponses,
		"event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","delta":"partial"}`+"\n\n",
		"event: response.failed\n"+`data: {"type":"response.failed","response":{"id":"resp_fail","object":"response","status":"failed","error":{"code":"server_error","message":"boom"},"usage":{"input_tokens":3,"output_tokens":0}}}`+"\n\n",
	)
	if !ok {
		t.Fatal("Finalize returned ok=false")
	}
	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("assembled JSON invalid: %v\n%s", err, out)
	}
	if got.ID != "resp_fail" || got.Status != "failed" || got.Error.Code != "server_error" {
		t.Errorf("failed terminal not used: %+v\n%s", got, out)
	}
}

// TestSSEParserBoundsUnterminatedLine proves a malformed upstream that streams a single line
// without a newline cannot grow the parser's line buffer without limit, and that the parser
// resynchronizes on the next newline so well-formed events after the garbage still parse.
func TestSSEParserBoundsUnterminatedLine(t *testing.T) {
	var p sseParser
	var events []string
	emit := func(_ string, data []byte) { events = append(events, string(data)) }

	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for written := 0; written < maxSSELineBytes+(2<<20); written += len(chunk) {
		p.write(chunk, emit)
		if len(p.lineBuf) > maxSSELineBytes {
			t.Fatalf("lineBuf = %d bytes, exceeds cap %d", len(p.lineBuf), maxSSELineBytes)
		}
	}
	if len(events) != 0 {
		t.Fatalf("garbage produced %d events, want 0", len(events))
	}

	// A newline terminates the discarded tail; a following valid event must parse.
	p.write([]byte("\n"), emit)
	p.write([]byte("data: {\"ok\":1}\n\n"), emit)
	if len(events) != 1 || events[0] != `{"ok":1}` {
		t.Fatalf("parser did not resync after over-long line: %v", events)
	}
}

// TestSSEParserBoundsEventData proves a stream that never closes an event cannot grow the
// pending-event buffer without limit; over-cap data is dropped and the event still emits.
func TestSSEParserBoundsEventData(t *testing.T) {
	var p sseParser
	var emitted int
	emit := func(_ string, _ []byte) { emitted++ }

	line := []byte("data: " + strings.Repeat("y", 64<<10) + "\n")
	for total := 0; total < maxSSEEventBytes+(4<<20); total += 64 << 10 {
		p.write(line, emit)
		if p.dataBytes > maxSSEEventBytes {
			t.Fatalf("dataBytes = %d, exceeds cap %d", p.dataBytes, maxSSEEventBytes)
		}
	}
	if emitted != 0 {
		t.Fatalf("emitted %d events before the blank line, want 0", emitted)
	}

	p.write([]byte("\n"), emit) // close the event
	if emitted != 1 {
		t.Fatalf("emitted = %d after closing event, want 1", emitted)
	}
	if p.dataBytes != 0 {
		t.Fatalf("dataBytes = %d after emit, want 0 (reset)", p.dataBytes)
	}
}

// TestSSEParserNormalEventStillWorks is a guard that the bounding logic did not alter parsing
// of ordinary multi-line events.
func TestSSEParserNormalEventStillWorks(t *testing.T) {
	var p sseParser
	var gotEvent string
	var gotData []byte
	emit := func(ev string, data []byte) { gotEvent, gotData = ev, data }

	p.write([]byte("event: message\ndata: {\"a\":1}\ndata: {\"b\":2}\n\n"), emit)
	if gotEvent != "message" || string(gotData) != "{\"a\":1}\n{\"b\":2}" {
		t.Fatalf("event=%q data=%q", gotEvent, gotData)
	}
}
