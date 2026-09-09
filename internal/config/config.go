package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default configuration constants matching the runnel spec.
const (
	DefaultServerHost            = "127.0.0.1"
	DefaultServerPort            = 8090
	DefaultServerReadTimeoutSec  = 15
	DefaultServerWriteTimeoutSec = 30
	DefaultServerMaxBodyBytes    = 10 * 1024 * 1024 // 10 MB

	DefaultSecurityBlockPrivateIPs = true
	DefaultSecurityEnableCORS      = true

	DefaultStorageDBPath                  = "./data/runnel.db"
	DefaultStorageCacheCleanupIntervalSec = 3600
	DefaultStorageMaxCacheSizeMB          = 500
	DefaultStorageWriteBatchSize          = 50
	DefaultStorageWriteFlushIntervalMS    = 100

	DefaultCooldownInitialSec = 300
	DefaultCooldownMultiplier = 2.0
	DefaultCooldownMaxSec     = 7200
	DefaultRequestsPerSec     = 0.5
	DefaultJitterMinMS        = 200
	DefaultJitterMaxMS        = 800
	DefaultQueueMaxSize       = 200
	DefaultQueueTimeoutSec    = 15
	DefaultServeStaleOnOpen   = true
	DefaultCacheTTLSec        = 3600
	DefaultProbeStrategy      = StrategyHeadersOnly
)

// Probe strategy identifiers.
const (
	StrategyHeadersOnly = "headers_only"
	StrategyQueryParam  = "query_param"
)

// Config represents the top-level runnel configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Security SecurityConfig `yaml:"security"`
	Storage  StorageConfig  `yaml:"storage"`
	Defaults DomainDefaults `yaml:"defaults"`
	Domains  []DomainConfig `yaml:"domains"`
}

// ServerConfig configures the inbound HTTP server settings.
type ServerConfig struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	ReadTimeoutSec  int    `yaml:"read_timeout_sec"`
	WriteTimeoutSec int    `yaml:"write_timeout_sec"`
	MaxBodyBytes    int64  `yaml:"max_body_bytes"`
}

// SecurityConfig configures ingress filtering, SSRF safeguards, and CORS headers.
type SecurityConfig struct {
	BlockPrivateIPs bool     `yaml:"block_private_ips"`
	EnableCORS      bool     `yaml:"enable_cors"`
	AllowedDomains  []string `yaml:"allowed_domains"`
	AdminToken      string   `yaml:"admin_token,omitempty"`
}

// StorageConfig configures SQLite database storage and persistence batching.
type StorageConfig struct {
	DBPath                  string `yaml:"db_path"`
	CacheCleanupIntervalSec int    `yaml:"cache_cleanup_interval_sec"`
	MaxCacheSizeMB          int    `yaml:"max_cache_size_mb"`
	WriteBatchSize          int    `yaml:"write_batch_size"`
	WriteFlushIntervalMS    int    `yaml:"write_flush_interval_ms"`
}

// DomainDefaults defines fallback circuit breaker, limiter, and caching policies.
type DomainDefaults struct {
	CooldownInitialSec int     `yaml:"cooldown_initial_sec"`
	CooldownMultiplier float64 `yaml:"cooldown_multiplier"`
	CooldownMaxSec     int     `yaml:"cooldown_max_sec"`
	RequestsPerSec     float64 `yaml:"requests_per_sec"`
	JitterMinMS        int     `yaml:"jitter_min_ms"`
	JitterMaxMS        int     `yaml:"jitter_max_ms"`
	QueueMaxSize       int     `yaml:"queue_max_size"`
	QueueTimeoutSec    int     `yaml:"queue_timeout_sec"`
	ServeStaleOnOpen   bool    `yaml:"serve_stale_on_open"`
	DefaultCacheTTLSec int     `yaml:"default_cache_ttl_sec"`
	ProbeStrategy      string  `yaml:"probe_strategy"`
}

// DomainConfig defines per-domain circuit breaker and rate limiting overrides.
type DomainConfig struct {
	Match              string   `yaml:"match"`
	CooldownInitialSec int      `yaml:"cooldown_initial_sec,omitempty"`
	CooldownMultiplier float64  `yaml:"cooldown_multiplier,omitempty"`
	CooldownMaxSec     int      `yaml:"cooldown_max_sec,omitempty"`
	RequestsPerSec     float64  `yaml:"requests_per_sec,omitempty"`
	JitterMinMS        int      `yaml:"jitter_min_ms,omitempty"`
	JitterMaxMS        int      `yaml:"jitter_max_ms,omitempty"`
	QueueMaxSize       int      `yaml:"queue_max_size,omitempty"`
	QueueTimeoutSec    int      `yaml:"queue_timeout_sec,omitempty"`
	ProbeStrategy      string   `yaml:"probe_strategy,omitempty"`
	AuthCookies        []string `yaml:"auth_cookies,omitempty"`
}

// NewDefaultConfig returns a Config populated with documented defaults matching the spec.
func NewDefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:            DefaultServerHost,
			Port:            DefaultServerPort,
			ReadTimeoutSec:  DefaultServerReadTimeoutSec,
			WriteTimeoutSec: DefaultServerWriteTimeoutSec,
			MaxBodyBytes:    DefaultServerMaxBodyBytes,
		},
		Security: SecurityConfig{
			BlockPrivateIPs: DefaultSecurityBlockPrivateIPs,
			EnableCORS:      DefaultSecurityEnableCORS,
			AllowedDomains:  []string{},
		},
		Storage: StorageConfig{
			DBPath:                  DefaultStorageDBPath,
			CacheCleanupIntervalSec: DefaultStorageCacheCleanupIntervalSec,
			MaxCacheSizeMB:          DefaultStorageMaxCacheSizeMB,
			WriteBatchSize:          DefaultStorageWriteBatchSize,
			WriteFlushIntervalMS:    DefaultStorageWriteFlushIntervalMS,
		},
		Defaults: DomainDefaults{
			CooldownInitialSec: DefaultCooldownInitialSec,
			CooldownMultiplier: DefaultCooldownMultiplier,
			CooldownMaxSec:     DefaultCooldownMaxSec,
			RequestsPerSec:     DefaultRequestsPerSec,
			JitterMinMS:        DefaultJitterMinMS,
			JitterMaxMS:        DefaultJitterMaxMS,
			QueueMaxSize:       DefaultQueueMaxSize,
			QueueTimeoutSec:    DefaultQueueTimeoutSec,
			ServeStaleOnOpen:   DefaultServeStaleOnOpen,
			DefaultCacheTTLSec: DefaultCacheTTLSec,
			ProbeStrategy:      DefaultProbeStrategy,
		},
		Domains: []DomainConfig{},
	}
}

// Load parses YAML bytes into a Config struct starting with defaults and validates it.
func Load(r io.Reader) (*Config, error) {
	cfg := NewDefaultConfig()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to decode config yaml: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// LoadFile reads and parses a configuration file from the given path.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	return Load(f)
}

// Validate checks configuration values against operational requirements.
func (c *Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	if c.Server.ReadTimeoutSec <= 0 {
		return fmt.Errorf("server.read_timeout_sec must be positive, got %d", c.Server.ReadTimeoutSec)
	}
	if c.Server.WriteTimeoutSec <= 0 {
		return fmt.Errorf("server.write_timeout_sec must be positive, got %d", c.Server.WriteTimeoutSec)
	}
	if c.Server.MaxBodyBytes <= 0 {
		return fmt.Errorf("server.max_body_bytes must be positive, got %d", c.Server.MaxBodyBytes)
	}

	if strings.TrimSpace(c.Storage.DBPath) == "" {
		return errors.New("storage.db_path cannot be empty")
	}
	if c.Storage.CacheCleanupIntervalSec <= 0 {
		return fmt.Errorf("storage.cache_cleanup_interval_sec must be positive, got %d", c.Storage.CacheCleanupIntervalSec)
	}
	if c.Storage.MaxCacheSizeMB <= 0 {
		return fmt.Errorf("storage.max_cache_size_mb must be positive, got %d", c.Storage.MaxCacheSizeMB)
	}
	if c.Storage.WriteBatchSize <= 0 {
		return fmt.Errorf("storage.write_batch_size must be positive, got %d", c.Storage.WriteBatchSize)
	}
	if c.Storage.WriteFlushIntervalMS <= 0 {
		return fmt.Errorf("storage.write_flush_interval_ms must be positive, got %d", c.Storage.WriteFlushIntervalMS)
	}

	if err := validateDefaults(&c.Defaults); err != nil {
		return fmt.Errorf("defaults validation error: %w", err)
	}

	for i, d := range c.Domains {
		if strings.TrimSpace(d.Match) == "" {
			return fmt.Errorf("domains[%d].match cannot be empty", i)
		}
		if d.ProbeStrategy != "" && !isValidProbeStrategy(d.ProbeStrategy) {
			return fmt.Errorf("domains[%d].probe_strategy invalid: %q", i, d.ProbeStrategy)
		}
		if d.CooldownInitialSec < 0 {
			return fmt.Errorf("domains[%d].cooldown_initial_sec cannot be negative", i)
		}
		if d.CooldownMultiplier < 0 {
			return fmt.Errorf("domains[%d].cooldown_multiplier cannot be negative", i)
		}
		if d.CooldownMaxSec < 0 {
			return fmt.Errorf("domains[%d].cooldown_max_sec cannot be negative", i)
		}
		if d.CooldownInitialSec > 0 && d.CooldownMaxSec > 0 && d.CooldownInitialSec > d.CooldownMaxSec {
			return fmt.Errorf("domains[%d].cooldown_initial_sec (%d) cannot exceed cooldown_max_sec (%d)", i, d.CooldownInitialSec, d.CooldownMaxSec)
		}
		if d.RequestsPerSec < 0 {
			return fmt.Errorf("domains[%d].requests_per_sec cannot be negative", i)
		}
		if d.JitterMinMS < 0 || d.JitterMaxMS < 0 {
			return fmt.Errorf("domains[%d] jitter values cannot be negative", i)
		}
		if d.JitterMinMS > 0 && d.JitterMaxMS > 0 && d.JitterMinMS > d.JitterMaxMS {
			return fmt.Errorf("domains[%d].jitter_min_ms (%d) cannot exceed jitter_max_ms (%d)", i, d.JitterMinMS, d.JitterMaxMS)
		}
		if d.QueueMaxSize < 0 {
			return fmt.Errorf("domains[%d].queue_max_size cannot be negative", i)
		}
		if d.QueueTimeoutSec < 0 {
			return fmt.Errorf("domains[%d].queue_timeout_sec cannot be negative", i)
		}
	}

	return nil
}

func validateDefaults(d *DomainDefaults) error {
	if d.CooldownInitialSec <= 0 {
		return fmt.Errorf("cooldown_initial_sec must be positive, got %d", d.CooldownInitialSec)
	}
	if d.CooldownMultiplier <= 0 {
		return fmt.Errorf("cooldown_multiplier must be positive, got %f", d.CooldownMultiplier)
	}
	if d.CooldownMaxSec < d.CooldownInitialSec {
		return fmt.Errorf("cooldown_max_sec (%d) must be greater than or equal to cooldown_initial_sec (%d)", d.CooldownMaxSec, d.CooldownInitialSec)
	}
	if d.RequestsPerSec <= 0 {
		return fmt.Errorf("requests_per_sec must be positive, got %f", d.RequestsPerSec)
	}
	if d.JitterMinMS < 0 {
		return fmt.Errorf("jitter_min_ms cannot be negative, got %d", d.JitterMinMS)
	}
	if d.JitterMaxMS < d.JitterMinMS {
		return fmt.Errorf("jitter_max_ms (%d) must be greater than or equal to jitter_min_ms (%d)", d.JitterMaxMS, d.JitterMinMS)
	}
	if d.QueueMaxSize <= 0 {
		return fmt.Errorf("queue_max_size must be positive, got %d", d.QueueMaxSize)
	}
	if d.QueueTimeoutSec <= 0 {
		return fmt.Errorf("queue_timeout_sec must be positive, got %d", d.QueueTimeoutSec)
	}
	if d.DefaultCacheTTLSec < 0 {
		return fmt.Errorf("default_cache_ttl_sec cannot be negative, got %d", d.DefaultCacheTTLSec)
	}
	if !isValidProbeStrategy(d.ProbeStrategy) {
		return fmt.Errorf("probe_strategy must be %q or %q, got %q", StrategyHeadersOnly, StrategyQueryParam, d.ProbeStrategy)
	}
	return nil
}

func isValidProbeStrategy(strategy string) bool {
	return strategy == StrategyHeadersOnly || strategy == StrategyQueryParam
}
