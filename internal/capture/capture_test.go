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
