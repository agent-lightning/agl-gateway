package config

import (
	"testing"
	"time"
)

const minimalYAML = `
master_key: mk-test
providers:
  - name: openai
    base_url: http://localhost:4141
    api_key: dummy
`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Server.Addr != ":8080" {
		t.Errorf("default addr = %q, want :8080", c.Server.Addr)
	}
	if c.Database != "./gateway.db" {
		t.Errorf("default database = %q", c.Database)
	}
	if c.Defaults.Retry.MaxRetries != 3 {
		t.Errorf("default max_retries = %d, want 3", c.Defaults.Retry.MaxRetries)
	}
	if c.Defaults.Retry.BaseDelay != 200*time.Millisecond {
		t.Errorf("default base_delay = %v", c.Defaults.Retry.BaseDelay)
	}
	if c.Defaults.Retry.MaxDelay != 10*time.Second {
		t.Errorf("default max_delay = %v", c.Defaults.Retry.MaxDelay)
	}
}

func TestParseDurations(t *testing.T) {
	y := minimalYAML + `
defaults:
  retry:
    max_retries: 5
    base_delay: 500ms
    max_delay: 30s
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Defaults.Retry.BaseDelay != 500*time.Millisecond {
		t.Errorf("base_delay = %v, want 500ms", c.Defaults.Retry.BaseDelay)
	}
	if c.Defaults.Retry.MaxDelay != 30*time.Second {
		t.Errorf("max_delay = %v, want 30s", c.Defaults.Retry.MaxDelay)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"missing master key": `
providers:
  - name: openai
    base_url: http://x
`,
		"no providers": `
master_key: mk
`,
		"provider missing base_url": `
master_key: mk
providers:
  - name: openai
`,
		"duplicate provider": `
master_key: mk
providers:
  - name: openai
    base_url: http://x
  - name: openai
    base_url: http://y
`,
		"duplicate pricing": `
master_key: mk
providers:
  - name: openai
    base_url: http://x
pricing:
  - model: gpt
    input_cost_per_token: 1
  - model: gpt
    input_cost_per_token: 2
`,
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestResolvedRetryOverride(t *testing.T) {
	def := Retry{MaxRetries: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 10 * time.Second}
	p := Provider{Retry: &Retry{MaxRetries: 7}}
	got := p.ResolvedRetry(def)
	if got.MaxRetries != 7 {
		t.Errorf("MaxRetries = %d, want 7", got.MaxRetries)
	}
	if got.BaseDelay != def.BaseDelay {
		t.Errorf("BaseDelay = %v, want fallback %v", got.BaseDelay, def.BaseDelay)
	}
	// No override -> full defaults.
	none := Provider{}
	if none.ResolvedRetry(def) != def {
		t.Errorf("ResolvedRetry without override = %+v, want %+v", none.ResolvedRetry(def), def)
	}
}

func TestProviderLookup(t *testing.T) {
	c, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Provider("openai") == nil {
		t.Error("Provider(openai) = nil, want found")
	}
	if c.Provider("missing") != nil {
		t.Error("Provider(missing) != nil")
	}
	if !c.ProviderNames()["openai"] {
		t.Error("ProviderNames missing openai")
	}
}

func TestExampleConfigParses(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("example config failed to load: %v", err)
	}
	// Model mapping is parsed.
	if c.Provider("openai").ModelMap["gpt-fast"] != "gpt-5-mini" {
		t.Errorf("model_map not parsed: %+v", c.Provider("openai").ModelMap)
	}
	// Base models are priced; context/effort variants are intentionally excluded.
	priced := make(map[string]bool)
	for _, p := range c.Pricing {
		priced[p.Model] = true
	}
	for _, base := range []string{"gpt-5.4", "claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5"} {
		if !priced[base] {
			t.Errorf("expected base model %q to be priced", base)
		}
	}
	for _, variant := range []string{"claude-opus-4-7-200k", "claude-opus-4-8-232k", "claude-haiku-4-5-20251001", "codex-auto-review"} {
		if priced[variant] {
			t.Errorf("variant %q should not be in the example pricing", variant)
		}
	}
}
