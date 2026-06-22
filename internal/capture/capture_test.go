package capture

import (
	"encoding/json"
	"testing"
)

func assemble(t *testing.T, format StreamFormat, chunks ...string) ([]byte, bool) {
	t.Helper()
	a := NewStreamAssembler(format)
	for _, c := range chunks {
		if _, err := a.Write([]byte(c)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	return a.Finalize()
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

func TestAssembleOpenAIChatStream(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":123,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`+"\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`+"\n\n",
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

// TestAssembleAnthropicModernUsageNotDropped guards the regression where message_start and
// message_delta were silently dropped because their usage objects carry nested objects
// (cache_creation, output_tokens_details) and strings (inference_geo) that a map[string]int
// cannot decode — losing id, model, stop_reason, and usage from the assembled message.
func TestAssembleAnthropicModernUsageNotDropped(t *testing.T) {
	out, ok := assemble(t, FormatAnthropicMessages,
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"inference_geo":"global","input_tokens":9,"output_tokens":6}}}`+"\n\n",
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
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"x"}}}`+"\n\n",
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

func TestAssembleOpenAIChatToolCallsAndRefusal(t *testing.T) {
	out, ok := assemble(t, FormatOpenAIChat,
		`data: {"id":"chatcmpl_1","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`+"\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SF\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n",
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
		`data: {"choices":[{"index":0,"delta":{"refusal":"not help"},"finish_reason":"stop"}]}`+"\n\n",
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
