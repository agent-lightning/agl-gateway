package usage

import (
	"testing"

	"github.com/kiki/agl-gateway/internal/pricing"
)

func TestRequestModel(t *testing.T) {
	if got := RequestModel([]byte(`{"model":"gpt-5.4","messages":[]}`)); got != "gpt-5.4" {
		t.Errorf("RequestModel = %q, want gpt-5.4", got)
	}
	if got := RequestModel([]byte(`not json`)); got != "" {
		t.Errorf("RequestModel(garbage) = %q, want empty", got)
	}
}

func feed(a *Accumulator, chunks ...string) (pricing.Usage, bool) {
	for _, c := range chunks {
		a.Write([]byte(c))
	}
	return a.Finalize()
}

func TestOpenAIChatNonStreaming(t *testing.T) {
	body := `{"id":"x","usage":{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200,"prompt_tokens_details":{"cached_tokens":400}}}`
	u, ok := feed(NewAccumulator(false), body)
	if !ok {
		t.Fatal("expected usage found")
	}
	// prompt_tokens includes cached -> input = 1000 - 400.
	want := pricing.Usage{InputTokens: 600, OutputTokens: 200, CacheReadTokens: 400}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestOpenAIResponsesNonStreaming(t *testing.T) {
	body := `{"usage":{"input_tokens":500,"output_tokens":100,"input_tokens_details":{"cached_tokens":50}}}`
	u, ok := feed(NewAccumulator(false), body)
	if !ok {
		t.Fatal("expected usage")
	}
	want := pricing.Usage{InputTokens: 450, OutputTokens: 100, CacheReadTokens: 50}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestAnthropicNonStreaming(t *testing.T) {
	body := `{"usage":{"input_tokens":300,"output_tokens":80,"cache_read_input_tokens":120,"cache_creation_input_tokens":40}}`
	u, ok := feed(NewAccumulator(false), body)
	if !ok {
		t.Fatal("expected usage")
	}
	// Anthropic input_tokens excludes cache -> no subtraction.
	want := pricing.Usage{InputTokens: 300, OutputTokens: 80, CacheReadTokens: 120, CacheWriteTokens: 40}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestOpenAIChatStreaming(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	u, ok := feed(NewAccumulator(true), stream)
	if !ok {
		t.Fatal("expected usage from stream")
	}
	want := pricing.Usage{InputTokens: 10, OutputTokens: 5}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestAnthropicStreamingMergesEvents(t *testing.T) {
	// input/cache arrive in message_start; final output in message_delta.
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":1,\"cache_read_input_tokens\":50}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":150}}\n\n"
	u, ok := feed(NewAccumulator(true), stream)
	if !ok {
		t.Fatal("expected usage")
	}
	want := pricing.Usage{InputTokens: 200, OutputTokens: 150, CacheReadTokens: 50}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestStreamingAcrossChunkBoundaries(t *testing.T) {
	// Split the usage line across several Write calls, mid-token.
	u, ok := feed(NewAccumulator(true),
		"data: {\"usage\":{\"prompt_to",
		"kens\":42,\"completion_tok",
		"ens\":7}}\n",
	)
	if !ok {
		t.Fatal("expected usage despite split writes")
	}
	if u.InputTokens != 42 || u.OutputTokens != 7 {
		t.Errorf("usage = %+v, want input 42 output 7", u)
	}
}

func TestNoUsage(t *testing.T) {
	if _, ok := feed(NewAccumulator(false), `{"id":"x","choices":[]}`); ok {
		t.Error("did not expect usage")
	}
	if _, ok := feed(NewAccumulator(true), "data: [DONE]\n\n"); ok {
		t.Error("did not expect usage from DONE-only stream")
	}
}
