package pricing

import (
	"math"
	"testing"

	"github.com/kiki/agl-gateway/internal/config"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func table() *Table {
	return New([]config.ModelPricing{
		{
			Model:                       "claude-opus-4-8",
			InputCostPerToken:           5e-6,
			OutputCostPerToken:          2.5e-5,
			CacheReadInputTokenCost:     5e-7,
			CacheCreationInputTokenCost: 6.25e-6,
		},
		{
			Model:              "gpt-5.4",
			InputCostPerToken:  2.5e-6,
			OutputCostPerToken: 1.5e-5,
			// no cache costs configured -> fall back to input rate
		},
	})
}

func TestCostFullClaude(t *testing.T) {
	tb := table()
	got := tb.Cost(Usage{
		Model:            "claude-opus-4-8",
		InputTokens:      1000,
		OutputTokens:     500,
		CacheReadTokens:  2000,
		CacheWriteTokens: 100,
	})
	want := 1000*5e-6 + 500*2.5e-5 + 2000*5e-7 + 100*6.25e-6
	if !approx(got, want) {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestCostCacheFallsBackToInputRate(t *testing.T) {
	tb := table()
	got := tb.Cost(Usage{Model: "gpt-5.4", InputTokens: 100, CacheReadTokens: 50})
	want := 100*2.5e-6 + 50*2.5e-6 // cache read falls back to input rate
	if !approx(got, want) {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestCostUnknownModelIsZero(t *testing.T) {
	tb := table()
	if got := tb.Cost(Usage{Model: "mystery", InputTokens: 100}); got != 0 {
		t.Errorf("unknown model cost = %v, want 0", got)
	}
	if tb.Has("mystery") {
		t.Error("Has(mystery) = true")
	}
	if !tb.Has("gpt-5.4") {
		t.Error("Has(gpt-5.4) = false")
	}
}
