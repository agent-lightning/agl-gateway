// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	Server    Server         `yaml:"server"`
	MasterKey string         `yaml:"master_key"`
	Database  string         `yaml:"database"`
	Defaults  Defaults       `yaml:"defaults"`
	Providers []Provider     `yaml:"providers"`
	Pricing   []ModelPricing `yaml:"pricing"`
}

// Server holds HTTP listener settings.
type Server struct {
	Addr string `yaml:"addr"`
}

// Defaults provides fallback settings applied to every provider.
type Defaults struct {
	Retry Retry `yaml:"retry"`
}

// Retry configures exponential backoff with jitter for an upstream provider.
type Retry struct {
	// MaxRetries is the number of retries *after* the first attempt. 0 disables retrying.
	MaxRetries int `yaml:"max_retries"`
	// BaseDelay is the backoff base; delay grows as BaseDelay * 2^attempt.
	BaseDelay time.Duration `yaml:"base_delay"`
	// MaxDelay caps the backoff before jitter is applied.
	MaxDelay time.Duration `yaml:"max_delay"`
}

// Provider is a single upstream LLM endpoint that requests can be routed to.
type Provider struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	// APIKey is the upstream credential injected as the Authorization bearer token
	// (unless overridden via Headers).
	APIKey string `yaml:"api_key"`
	// Headers are extra headers added to every upstream request.
	Headers map[string]string `yaml:"headers"`
	// ModelMap rewrites the request's "model" field before forwarding: a request for a
	// key in this map is sent upstream with the mapped value instead. Both the original
	// and mapped model are logged.
	ModelMap map[string]string `yaml:"model_map"`
	// Retry overrides Defaults.Retry for this provider. Nil fields fall back to defaults.
	Retry *Retry `yaml:"retry"`
}

// ModelPricing is the per-token cost for a model, mirroring the new-api schema.
type ModelPricing struct {
	Model                       string  `yaml:"model"`
	InputCostPerToken           float64 `yaml:"input_cost_per_token"`
	OutputCostPerToken          float64 `yaml:"output_cost_per_token"`
	CacheReadInputTokenCost     float64 `yaml:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost float64 `yaml:"cache_creation_input_token_cost"`
}

// ResolvedRetry returns the effective retry policy for the provider, layering any
// provider-level overrides on top of the supplied defaults.
func (p Provider) ResolvedRetry(def Retry) Retry {
	r := def
	if p.Retry != nil {
		if p.Retry.MaxRetries != 0 {
			r.MaxRetries = p.Retry.MaxRetries
		}
		if p.Retry.BaseDelay != 0 {
			r.BaseDelay = p.Retry.BaseDelay
		}
		if p.Retry.MaxDelay != 0 {
			r.MaxDelay = p.Retry.MaxDelay
		}
	}
	return r
}

// Load reads, parses, and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse parses configuration from YAML bytes, applying defaults and validating.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Database == "" {
		c.Database = "./gateway.db"
	}
	if c.Defaults.Retry.MaxRetries == 0 {
		c.Defaults.Retry.MaxRetries = 3
	}
	if c.Defaults.Retry.BaseDelay == 0 {
		c.Defaults.Retry.BaseDelay = 200 * time.Millisecond
	}
	if c.Defaults.Retry.MaxDelay == 0 {
		c.Defaults.Retry.MaxDelay = 10 * time.Second
	}
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	if c.MasterKey == "" {
		return fmt.Errorf("config: master_key is required")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("config: provider[%d] is missing a name", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
		if p.BaseURL == "" {
			return fmt.Errorf("config: provider %q is missing base_url", p.Name)
		}
		if p.Retry != nil && p.Retry.MaxRetries < 0 {
			return fmt.Errorf("config: provider %q has negative max_retries", p.Name)
		}
	}
	pricingSeen := make(map[string]bool, len(c.Pricing))
	for _, m := range c.Pricing {
		if m.Model == "" {
			return fmt.Errorf("config: pricing entry is missing a model name")
		}
		if pricingSeen[m.Model] {
			return fmt.Errorf("config: duplicate pricing entry for model %q", m.Model)
		}
		pricingSeen[m.Model] = true
	}
	return nil
}

// Provider returns the named provider, or nil if it does not exist.
func (c *Config) Provider(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

// ProviderNames returns the set of configured provider names.
func (c *Config) ProviderNames() map[string]bool {
	names := make(map[string]bool, len(c.Providers))
	for _, p := range c.Providers {
		names[p.Name] = true
	}
	return names
}
