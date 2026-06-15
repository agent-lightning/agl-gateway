// Package pricing computes the monetary cost of a request from normalized token usage.
package pricing

import "github.com/kiki/agl-gateway/internal/config"

// Usage is provider-normalized token usage. All fields are non-overlapping counts so
// that cost is a simple linear combination:
//
//	cost = Input*input + Output*output + CacheRead*cacheRead + CacheWrite*cacheCreate
//
// In particular InputTokens MUST NOT include cached tokens; extraction is responsible
// for moving cached counts out of the input bucket.
type Usage struct {
	Model            string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Table maps model name -> pricing.
type Table struct {
	models map[string]config.ModelPricing
}

// New builds a pricing table from the configured per-model costs.
func New(entries []config.ModelPricing) *Table {
	m := make(map[string]config.ModelPricing, len(entries))
	for _, e := range entries {
		m[e.Model] = e
	}
	return &Table{models: m}
}

// Has reports whether the table has pricing for the model.
func (t *Table) Has(model string) bool {
	_, ok := t.models[model]
	return ok
}

// Cost returns the dollar cost of the usage. Unknown models cost 0 (best-effort: we still
// log the request, just without a price). Cached tokens fall back to the input rate when a
// dedicated cache rate is not configured for the model.
func (t *Table) Cost(u Usage) float64 {
	p, ok := t.models[u.Model]
	if !ok {
		return 0
	}
	cacheRead := p.CacheReadInputTokenCost
	if cacheRead == 0 {
		cacheRead = p.InputCostPerToken
	}
	cacheWrite := p.CacheCreationInputTokenCost
	if cacheWrite == 0 {
		cacheWrite = p.InputCostPerToken
	}
	return float64(u.InputTokens)*p.InputCostPerToken +
		float64(u.OutputTokens)*p.OutputCostPerToken +
		float64(u.CacheReadTokens)*cacheRead +
		float64(u.CacheWriteTokens)*cacheWrite
}
