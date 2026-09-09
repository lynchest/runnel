package config_test

import (
	"strings"
	"testing"

	"github.com/lynchest/runnel/internal/config"
)

func TestNewDefaultConfig(t *testing.T) {
	cfg := config.NewDefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid default config, got: %v", err)
	}

	if cfg.Server.Host != config.DefaultServerHost {
		t.Errorf("expected host %s, got %s", config.DefaultServerHost, cfg.Server.Host)
	}
	if cfg.Server.Port != config.DefaultServerPort {
		t.Errorf("expected port %d, got %d", config.DefaultServerPort, cfg.Server.Port)
	}
	if cfg.Server.ReadTimeoutSec != config.DefaultServerReadTimeoutSec {
		t.Errorf("expected read timeout %d, got %d", config.DefaultServerReadTimeoutSec, cfg.Server.ReadTimeoutSec)
	}
	if cfg.Server.WriteTimeoutSec != config.DefaultServerWriteTimeoutSec {
		t.Errorf("expected write timeout %d, got %d", config.DefaultServerWriteTimeoutSec, cfg.Server.WriteTimeoutSec)
	}
	if cfg.Server.MaxBodyBytes != config.DefaultServerMaxBodyBytes {
		t.Errorf("expected max body bytes %d, got %d", config.DefaultServerMaxBodyBytes, cfg.Server.MaxBodyBytes)
	}
	if !cfg.Security.BlockPrivateIPs {
		t.Errorf("expected block_private_ips to be true")
	}
	if !cfg.Security.EnableCORS {
		t.Errorf("expected enable_cors to be true")
	}
	if cfg.Storage.DBPath != config.DefaultStorageDBPath {
		t.Errorf("expected db_path %s, got %s", config.DefaultStorageDBPath, cfg.Storage.DBPath)
	}
	if cfg.Storage.CacheCleanupIntervalSec != config.DefaultStorageCacheCleanupIntervalSec {
		t.Errorf("expected cleanup interval %d, got %d", config.DefaultStorageCacheCleanupIntervalSec, cfg.Storage.CacheCleanupIntervalSec)
	}
	if cfg.Storage.MaxCacheSizeMB != config.DefaultStorageMaxCacheSizeMB {
		t.Errorf("expected max cache size %d, got %d", config.DefaultStorageMaxCacheSizeMB, cfg.Storage.MaxCacheSizeMB)
	}
	if cfg.Storage.WriteBatchSize != config.DefaultStorageWriteBatchSize {
		t.Errorf("expected write batch size %d, got %d", config.DefaultStorageWriteBatchSize, cfg.Storage.WriteBatchSize)
	}
	if cfg.Storage.WriteFlushIntervalMS != config.DefaultStorageWriteFlushIntervalMS {
		t.Errorf("expected write flush interval %d, got %d", config.DefaultStorageWriteFlushIntervalMS, cfg.Storage.WriteFlushIntervalMS)
	}
	if cfg.Defaults.CooldownInitialSec != config.DefaultCooldownInitialSec {
		t.Errorf("expected cooldown initial %d, got %d", config.DefaultCooldownInitialSec, cfg.Defaults.CooldownInitialSec)
	}
	if cfg.Defaults.CooldownMultiplier != config.DefaultCooldownMultiplier {
		t.Errorf("expected cooldown multiplier %f, got %f", config.DefaultCooldownMultiplier, cfg.Defaults.CooldownMultiplier)
	}
	if cfg.Defaults.CooldownMaxSec != config.DefaultCooldownMaxSec {
		t.Errorf("expected cooldown max %d, got %d", config.DefaultCooldownMaxSec, cfg.Defaults.CooldownMaxSec)
	}
	if cfg.Defaults.RequestsPerSec != config.DefaultRequestsPerSec {
		t.Errorf("expected requests per sec %f, got %f", config.DefaultRequestsPerSec, cfg.Defaults.RequestsPerSec)
	}
	if cfg.Defaults.JitterMinMS != config.DefaultJitterMinMS {
		t.Errorf("expected jitter min %d, got %d", config.DefaultJitterMinMS, cfg.Defaults.JitterMinMS)
	}
	if cfg.Defaults.JitterMaxMS != config.DefaultJitterMaxMS {
		t.Errorf("expected jitter max %d, got %d", config.DefaultJitterMaxMS, cfg.Defaults.JitterMaxMS)
	}
	if cfg.Defaults.QueueMaxSize != config.DefaultQueueMaxSize {
		t.Errorf("expected queue max size %d, got %d", config.DefaultQueueMaxSize, cfg.Defaults.QueueMaxSize)
	}
	if cfg.Defaults.QueueTimeoutSec != config.DefaultQueueTimeoutSec {
		t.Errorf("expected queue timeout %d, got %d", config.DefaultQueueTimeoutSec, cfg.Defaults.QueueTimeoutSec)
	}
	if !cfg.Defaults.ServeStaleOnOpen {
		t.Errorf("expected serve_stale_on_open to be true")
	}
	if cfg.Defaults.DefaultCacheTTLSec != config.DefaultCacheTTLSec {
		t.Errorf("expected cache ttl %d, got %d", config.DefaultCacheTTLSec, cfg.Defaults.DefaultCacheTTLSec)
	}
	if cfg.Defaults.ProbeStrategy != config.DefaultProbeStrategy {
		t.Errorf("expected probe strategy %s, got %s", config.DefaultProbeStrategy, cfg.Defaults.ProbeStrategy)
	}
}

func TestLoad_ValidYAMLWithOverrides(t *testing.T) {
	yamlContent := `
server:
  host: "0.0.0.0"
  port: 9000
  read_timeout_sec: 20
  write_timeout_sec: 40
  max_body_bytes: 5242880

security:
  block_private_ips: true
  enable_cors: false
  allowed_domains:
    - "api.steampowered.com"

storage:
  db_path: "/tmp/test.db"
  cache_cleanup_interval_sec: 1800
  max_cache_size_mb: 250
  write_batch_size: 25
  write_flush_interval_ms: 50

defaults:
  cooldown_initial_sec: 120
  cooldown_multiplier: 1.5
  cooldown_max_sec: 3600
  requests_per_sec: 2.0
  jitter_min_ms: 50
  jitter_max_ms: 200
  queue_max_size: 100
  queue_timeout_sec: 10
  serve_stale_on_open: false
  default_cache_ttl_sec: 1800
  probe_strategy: "query_param"

domains:
  - match: "*.steampowered.com"
    cooldown_initial_sec: 600
    cooldown_multiplier: 2.0
    cooldown_max_sec: 14400
    requests_per_sec: 0.2
    jitter_min_ms: 500
    jitter_max_ms: 2000
    queue_max_size: 500
    queue_timeout_sec: 20
    probe_strategy: "query_param"
    auth_cookies:
      - "steamLoginSecure"
`
	cfg, err := config.Load(strings.NewReader(yamlContent))
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}

	if cfg.Server.Port != 9000 {
		t.Errorf("expected port 9000, got %d", cfg.Server.Port)
	}
	if cfg.Security.EnableCORS != false {
		t.Errorf("expected enable_cors false, got true")
	}
	if len(cfg.Security.AllowedDomains) != 1 || cfg.Security.AllowedDomains[0] != "api.steampowered.com" {
		t.Errorf("unexpected allowed_domains: %v", cfg.Security.AllowedDomains)
	}
	if len(cfg.Domains) != 1 {
		t.Fatalf("expected 1 domain config, got %d", len(cfg.Domains))
	}
	if cfg.Domains[0].Match != "*.steampowered.com" {
		t.Errorf("expected match *.steampowered.com, got %s", cfg.Domains[0].Match)
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		wantErrSub  string
	}{
		{
			name: "invalid port (out of range)",
			yamlContent: `
server:
  port: 70000
`,
			wantErrSub: "server.port must be between 1 and 65535",
		},
		{
			name: "invalid port (negative)",
			yamlContent: `
server:
  port: -1
`,
			wantErrSub: "server.port must be between 1 and 65535",
		},
		{
			name: "invalid server read_timeout_sec",
			yamlContent: `
server:
  read_timeout_sec: 0
`,
			wantErrSub: "server.read_timeout_sec must be positive",
		},
		{
			name: "invalid server write_timeout_sec",
			yamlContent: `
server:
  write_timeout_sec: -5
`,
			wantErrSub: "server.write_timeout_sec must be positive",
		},
		{
			name: "invalid server max_body_bytes",
			yamlContent: `
server:
  max_body_bytes: 0
`,
			wantErrSub: "server.max_body_bytes must be positive",
		},
		{
			name: "empty db_path",
			yamlContent: `
storage:
  db_path: "   "
`,
			wantErrSub: "storage.db_path cannot be empty",
		},
		{
			name: "invalid storage max_cache_size_mb",
			yamlContent: `
storage:
  max_cache_size_mb: -10
`,
			wantErrSub: "storage.max_cache_size_mb must be positive",
		},
		{
			name: "invalid storage write_batch_size",
			yamlContent: `
storage:
  write_batch_size: 0
`,
			wantErrSub: "storage.write_batch_size must be positive",
		},
		{
			name: "invalid storage write_flush_interval_ms",
			yamlContent: `
storage:
  write_flush_interval_ms: 0
`,
			wantErrSub: "storage.write_flush_interval_ms must be positive",
		},
		{
			name: "invalid defaults cooldown_initial_sec",
			yamlContent: `
defaults:
  cooldown_initial_sec: 0
`,
			wantErrSub: "cooldown_initial_sec must be positive",
		},
		{
			name: "invalid defaults cooldown_max_sec less than initial",
			yamlContent: `
defaults:
  cooldown_initial_sec: 500
  cooldown_max_sec: 300
`,
			wantErrSub: "cooldown_max_sec (300) must be greater than or equal to cooldown_initial_sec (500)",
		},
		{
			name: "invalid defaults jitter min > max",
			yamlContent: `
defaults:
  jitter_min_ms: 500
  jitter_max_ms: 200
`,
			wantErrSub: "jitter_max_ms (200) must be greater than or equal to jitter_min_ms (500)",
		},
		{
			name: "invalid negative default cache ttl",
			yamlContent: `
defaults:
  default_cache_ttl_sec: -1
`,
			wantErrSub: "default_cache_ttl_sec cannot be negative",
		},
		{
			name: "invalid probe_strategy",
			yamlContent: `
defaults:
  probe_strategy: "unknown_strategy"
`,
			wantErrSub: `probe_strategy must be "headers_only" or "query_param"`,
		},
		{
			name: "empty domain match",
			yamlContent: `
domains:
  - match: ""
`,
			wantErrSub: "domains[0].match cannot be empty",
		},
		{
			name: "invalid domain probe_strategy",
			yamlContent: `
domains:
  - match: "api.example.com"
    probe_strategy: "invalid"
`,
			wantErrSub: `domains[0].probe_strategy invalid`,
		},
		{
			name: "domain cooldown initial > max",
			yamlContent: `
domains:
  - match: "api.example.com"
    cooldown_initial_sec: 1000
    cooldown_max_sec: 200
`,
			wantErrSub: "cooldown_initial_sec (1000) cannot exceed cooldown_max_sec (200)",
		},
		{
			name: "domain jitter min > max",
			yamlContent: `
domains:
  - match: "api.example.com"
    jitter_min_ms: 500
    jitter_max_ms: 100
`,
			wantErrSub: "jitter_min_ms (500) cannot exceed jitter_max_ms (100)",
		},
		{
			name: "unknown field error",
			yamlContent: `
unknown_key: "value"
`,
			wantErrSub: "failed to decode config yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(strings.NewReader(tt.yamlContent))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErrSub, err.Error())
			}
		})
	}
}

func TestConfigAllowsZeroCacheTTLToDisableCaching(t *testing.T) {
	cfg, err := config.Load(strings.NewReader("defaults:\n  default_cache_ttl_sec: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.DefaultCacheTTLSec != 0 {
		t.Fatalf("default cache ttl = %d, want 0", cfg.Defaults.DefaultCacheTTLSec)
	}
}
