package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/config"
	"github.com/lynchest/runnel/internal/limiter"
)

func TestBurstDefaultIsOne(t *testing.T) {
	cfg := config.NewDefaultConfig()
	if cfg.Defaults.Burst != 1 {
		t.Fatalf("defaults burst = %d, want backward-compatible 1", cfg.Defaults.Burst)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestBurstValidation(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Defaults.Burst = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "burst") {
		t.Fatalf("defaults burst 0 error = %v, want burst complaint", err)
	}

	cfg = config.NewDefaultConfig()
	cfg.Domains = []config.DomainConfig{{Match: "example.com", Burst: -1}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "burst") {
		t.Fatalf("override burst -1 error = %v, want burst complaint", err)
	}
}

func TestBurstOverrideMerge(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Storage.DBPath = t.TempDir() + "/runnel.db"
	cfg.Security.BlockPrivateIPs = false
	cfg.Domains = []config.DomainConfig{{Match: "*.example.com", Burst: 5}}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()
	if got := app.domainPolicy("api.example.com").Burst; got != 5 {
		t.Fatalf("override burst = %d, want 5", got)
	}
	if got := app.domainPolicy("other.test").Burst; got != 1 {
		t.Fatalf("inherited burst = %d, want default 1", got)
	}
}

func TestDomainPolicyMergeKeepsCooldownMax(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Storage.DBPath = t.TempDir() + "/runnel.db"
	cfg.Security.BlockPrivateIPs = false
	cfg.Domains = []config.DomainConfig{{Match: "api.example.com", CooldownMaxSec: 60, Burst: 2}}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()
	policy := app.domainPolicy("api.example.com")
	if policy.CooldownMaxSec != 60 {
		t.Fatalf("cooldown max = %d, want override 60", policy.CooldownMaxSec)
	}
	if policy.Burst != 2 {
		t.Fatalf("burst = %d, want override 2", policy.Burst)
	}
}

func TestBurstCapacityBehavior(t *testing.T) {
	clock := limiter.NewMockClock(time.Now())

	// burst=1 (the default): one immediate success, then the bucket is
	// empty. Acquire would wait rather than fail; Allow shows the capacity.
	one := limiter.New(1.0, 1, limiter.WithClock(clock))
	if !one.Allow() {
		t.Fatal("burst=1 first Allow must succeed")
	}
	if one.Allow() {
		t.Fatal("burst=1 second Allow must fail: requests serialize")
	}

	three := limiter.New(1.0, 3, limiter.WithClock(clock))
	for i := range 3 {
		if !three.Allow() {
			t.Fatalf("burst=3 Allow %d must succeed", i+1)
		}
	}
	if three.Allow() {
		t.Fatal("burst=3 fourth Allow must fail")
	}
}

func TestBurstLoadsFromYAML(t *testing.T) {
	cfg, err := config.Load(strings.NewReader(`
server: {port: 8090}
defaults: {burst: 4}
domains: [{match: "api.example.com", burst: 2}]
`))
	if err != nil {
		t.Fatalf("load with burst: %v", err)
	}
	if cfg.Defaults.Burst != 4 {
		t.Fatalf("defaults burst = %d, want 4", cfg.Defaults.Burst)
	}
	if len(cfg.Domains) != 1 || cfg.Domains[0].Burst != 2 {
		t.Fatalf("override burst = %+v", cfg.Domains)
	}
}
