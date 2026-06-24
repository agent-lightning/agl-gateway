// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	Server Server `yaml:"server"`
	// MasterKey authenticates the control plane (/admin/*). The AGL_MASTER_KEY environment
	// variable, when set, overrides this field — convenient for containers and for keeping the
	// secret out of the YAML file.
	MasterKey string `yaml:"master_key"`
	// Database selects the persistence backend: a postgres:// or postgresql:// URL uses
	// PostgreSQL; anything else is a SQLite file path (default "./gateway.db"). The
	// AGL_DATABASE environment variable, when set, overrides this field — convenient for
	// containers and for keeping a PostgreSQL DSN (with its password) out of the YAML file.
	Database string `yaml:"database"`
	// LogsDatabase, when set, sends request_logs to a separate backend while api_keys stay in
	// Database. A clickhouse:// (or clickhouses://) URL selects ClickHouse — an append-only
	// OLAP store suited to high-volume log analytics; a postgres:// URL or a file path are
	// also accepted. Empty (the default) keeps logs co-located with keys in Database. The
	// AGL_LOGS_DATABASE environment variable, when set, overrides this field.
	LogsDatabase   string         `yaml:"logs_database"`
	Defaults       Defaults       `yaml:"defaults"`
	PayloadCapture PayloadCapture `yaml:"payload_capture"`
	Providers      []Provider     `yaml:"providers"`
	Pricing        []ModelPricing `yaml:"pricing"`
}

// Server holds HTTP listener settings.
type Server struct {
	Addr string `yaml:"addr"`
	// MaxRequestBytes caps the size of a proxied request body the gateway will buffer (the
	// body is held in memory so it can be replayed across provider failover). 0 applies
	// DefaultMaxRequestBytes; a negative value disables the limit (unbounded). Over-limit
	// requests are rejected with 413 before any upstream call, so a single large or malicious
	// request cannot exhaust gateway memory.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// DefaultMaxRequestBytes bounds the in-memory request body when no explicit limit is set. It
// is generous enough for multimodal payloads (base64 images/audio) while still preventing an
// unbounded body from OOMing the gateway.
const DefaultMaxRequestBytes = 100 << 20 // 100 MiB

// Defaults provides fallback settings applied to every provider.
type Defaults struct {
	Retry Retry `yaml:"retry"`
	// KeepLogsOnKeyDelete is the fallback log-retention policy applied to a new key when the
	// create request does not specify one: when false (the default) deleting the key also
	// cascade-deletes its request logs; when true the logs are retained (orphaned) so usage
	// history survives the key. A key stores its own resolved value, so changing this default
	// only affects keys created afterward.
	KeepLogsOnKeyDelete bool `yaml:"keep_logs_on_key_delete"`
}

// DefaultPayloadCaptureBytes caps each stored payload field when capture is enabled and no
// explicit limit is configured.
const DefaultPayloadCaptureBytes = 1 << 20 // 1 MiB

// PayloadCapture controls optional storage of request/response bodies in request_logs.
type PayloadCapture struct {
	Enabled           bool `yaml:"enabled"`
	MaxRequestBytes   int  `yaml:"max_request_bytes"`
	MaxResponseBytes  int  `yaml:"max_response_bytes"`
	AssembleStreams   bool `yaml:"assemble_streams"`
	MaxAssembledBytes int  `yaml:"max_assembled_bytes"`
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

// Provider-selection policy values for a key. When a request does not pin a provider via
// the X-AGL-Provider header and the key is bound to several providers, "start" decides
// which provider the first attempt uses and "order" decides how retries walk the rest.
const (
	StartFirst      = "first"       // first attempt uses the first-bound provider
	StartRandom     = "random"      // first attempt uses a random bound provider
	OrderRoundRobin = "round_robin" // retries walk the remaining providers in order
	OrderRandom     = "random"      // retries pick the remaining providers at random

	DefaultStart = StartFirst
	DefaultOrder = OrderRoundRobin
)

// ValidStart reports whether s is a recognized provider-selection start policy.
func ValidStart(s string) bool { return s == StartFirst || s == StartRandom }

// ValidOrder reports whether s is a recognized provider-selection retry order.
func ValidOrder(s string) bool { return s == OrderRoundRobin || s == OrderRandom }

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

// DatabaseEnv is the environment variable that, when set, overrides the configured
// database backend (see Config.Database).
const DatabaseEnv = "AGL_DATABASE"

// LogsDatabaseEnv is the environment variable that, when set, overrides the configured
// request_logs backend (see Config.LogsDatabase).
const LogsDatabaseEnv = "AGL_LOGS_DATABASE"

// MasterKeyEnv is the environment variable that, when set, overrides the configured
// master key (see Config.MasterKey).
const MasterKeyEnv = "AGL_MASTER_KEY"

func (c *Config) applyDefaults() {
	if v := os.Getenv(MasterKeyEnv); v != "" {
		c.MasterKey = v
	}
	if v := os.Getenv(DatabaseEnv); v != "" {
		c.Database = v
	}
	if v := os.Getenv(LogsDatabaseEnv); v != "" {
		c.LogsDatabase = v
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.MaxRequestBytes == 0 {
		c.Server.MaxRequestBytes = DefaultMaxRequestBytes
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
	if c.PayloadCapture.MaxRequestBytes == 0 {
		c.PayloadCapture.MaxRequestBytes = DefaultPayloadCaptureBytes
	}
	if c.PayloadCapture.MaxResponseBytes == 0 {
		c.PayloadCapture.MaxResponseBytes = DefaultPayloadCaptureBytes
	}
	if c.PayloadCapture.MaxAssembledBytes == 0 {
		c.PayloadCapture.MaxAssembledBytes = DefaultPayloadCaptureBytes
	}
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	if c.MasterKey == "" {
		return fmt.Errorf("config: master_key is required")
	}
	if strings.HasPrefix(c.Database, "clickhouse://") || strings.HasPrefix(c.Database, "clickhouses://") {
		return fmt.Errorf("config: database cannot be ClickHouse (it cannot store api_keys); use SQLite/PostgreSQL for database and set logs_database to the ClickHouse URL")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}
	if c.PayloadCapture.MaxRequestBytes < 0 {
		return fmt.Errorf("config: payload_capture max_request_bytes must be non-negative")
	}
	if c.PayloadCapture.MaxResponseBytes < 0 {
		return fmt.Errorf("config: payload_capture max_response_bytes must be non-negative")
	}
	if c.PayloadCapture.MaxAssembledBytes < 0 {
		return fmt.Errorf("config: payload_capture max_assembled_bytes must be non-negative")
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
