package main

import (
	"context"
	"strings"
	"testing"
)

func TestIsLoopbackBind(t *testing.T) {
	loopback := []string{"", "127.0.0.1", "127.0.0.2", "::1", "[::1]", "localhost", "LOCALHOST", "localhost."}
	for _, host := range loopback {
		if !isLoopbackBind(host) {
			t.Errorf("isLoopbackBind(%q) = false, want true", host)
		}
	}
	public := []string{"0.0.0.0", "::", "[::]", "192.168.1.10", "10.0.0.5", "8.8.8.8", "example.com", "runnel.internal"}
	for _, host := range public {
		if isLoopbackBind(host) {
			t.Errorf("isLoopbackBind(%q) = true, want false", host)
		}
	}
}

func TestNonLoopbackBindWithoutAllowlistRefused(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.10", "example.com"} {
		cfg := testConfig(t)
		cfg.Server.Host = host
		if _, err := NewApplication(cfg); err == nil {
			t.Errorf("NewApplication with host %q and empty allowlist succeeded, want refusal", host)
		} else if !strings.Contains(err.Error(), "allowed_domains") {
			t.Errorf("host %q error = %v, want allowed_domains refusal", host, err)
		}
	}
}

func TestLoopbackBindWithoutAllowlistAllowed(t *testing.T) {
	for _, host := range []string{"", "127.0.0.1", "::1", "localhost"} {
		cfg := testConfig(t)
		cfg.Server.Host = host
		app, err := NewApplication(cfg)
		if err != nil {
			t.Errorf("NewApplication with loopback host %q failed: %v", host, err)
			continue
		}
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown with host %q: %v", host, err)
		}
	}
}

func TestNonLoopbackBindWithAllowlistAllowed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.Host = "0.0.0.0"
	cfg.Security.AllowedDomains = []string{"example.com"}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication with allowlist failed: %v", err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestRunnelHostEnvOverrideGuarded(t *testing.T) {
	t.Setenv("RUNNEL_HOST", "0.0.0.0")
	cfg := testConfig(t)
	if _, err := NewApplication(cfg); err == nil {
		t.Fatal("NewApplication with RUNNEL_HOST=0.0.0.0 and empty allowlist succeeded, want refusal")
	}

	cfg = testConfig(t)
	cfg.Security.AllowedDomains = []string{"example.com"}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication with RUNNEL_HOST=0.0.0.0 and allowlist failed: %v", err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
