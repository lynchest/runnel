package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynchest/runnel/internal/circuit"
	"github.com/lynchest/runnel/internal/config"
)

func TestAdminEndpointsExposeHealthCircuitMetricsAndGuardReset(t *testing.T) {
	cfg := testConfig(t)
	cfg.Security.AdminToken = "test-admin-token"
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()

	breaker := app.Registry().Get("api.example.test")
	breaker.OnRateLimit(60)

	response := serveAppRequest(t, app, http.MethodGet, "/_healthz", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ok"`) {
		t.Fatalf("health response = %d %q", response.Code, response.Body.String())
	}

	response = serveAppRequest(t, app, http.MethodGet, "/_circuit", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "api.example.test") || !strings.Contains(response.Body.String(), "OPEN") {
		t.Fatalf("circuit response = %d %q", response.Code, response.Body.String())
	}

	response = serveAppRequest(t, app, http.MethodGet, "/_metrics", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "runnel_requests_total") {
		t.Fatalf("metrics response = %d %q", response.Code, response.Body.String())
	}

	response = serveAppRequest(t, app, http.MethodPost, "/_circuit/reset", "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthorized reset status = %d, want 403", response.Code)
	}
	response = serveAppRequest(t, app, http.MethodPost, "/_circuit/reset", "wrong-token")
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong-token reset status = %d, want 403", response.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/_circuit/reset?domain=api.example.test", nil)
	request.Header.Set("X-Admin-Token", cfg.Security.AdminToken)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized reset status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := breaker.State(); got != circuit.StateClosed {
		t.Fatalf("state after reset = %s, want %s", got, circuit.StateClosed)
	}
}

func TestAdminResetAcceptsBearerTokenAndRejectsMissingRegistry(t *testing.T) {
	cfg := testConfig(t)
	cfg.Security.AdminToken = "bearer-token"
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()

	breaker := app.Registry().Get("api.example.test")
	breaker.OnFailure()
	request := httptest.NewRequest(http.MethodPost, "/_circuit/reset", nil)
	request.Header.Set("Authorization", "Bearer "+cfg.Security.AdminToken)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("bearer reset status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if breaker.State() != circuit.StateClosed {
		t.Fatal("bearer reset did not close circuit")
	}
}

func serveAppRequest(t *testing.T, app *Application, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("X-Admin-Token", token)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Storage.DBPath = filepath.Join(t.TempDir(), "runnel.db")
	cfg.Security.BlockPrivateIPs = false
	cfg.Defaults.RequestsPerSec = 1000
	cfg.Defaults.JitterMinMS = 0
	cfg.Defaults.JitterMaxMS = 0
	cfg.Defaults.CooldownInitialSec = 1
	cfg.Defaults.CooldownMaxSec = 2
	cfg.Defaults.QueueTimeoutSec = 1
	cfg.Defaults.DefaultCacheTTLSec = 3600
	return cfg
}

func TestApplicationHealthChecksClosedStore(t *testing.T) {
	app, err := NewApplication(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.health(context.Background()); err == nil {
		t.Fatal("health check succeeded after store shutdown")
	}
}

func TestApplicationCORSConfigControlsPreflightAndOrdinaryResponses(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		status  int
	}{
		{name: "enabled", enabled: true, status: http.StatusNoContent},
		{name: "disabled", enabled: false, status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Security.EnableCORS = test.enabled
			app, err := NewApplication(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := app.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown application: %v", err)
				}
			}()

			preflight := serveAppRequest(t, app, http.MethodOptions, "/proxy", "")
			if preflight.Code != test.status {
				t.Fatalf("preflight status = %d, want %d", preflight.Code, test.status)
			}
			if test.enabled {
				if got := preflight.Header().Get("Access-Control-Allow-Origin"); got != "*" {
					t.Fatalf("preflight origin = %q, want *", got)
				}
			} else if got := preflight.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("disabled preflight origin = %q, want empty", got)
			}

			ordinary := serveAppRequest(t, app, http.MethodGet, "/proxy", "")
			if ordinary.Code != http.StatusBadRequest {
				t.Fatalf("ordinary status = %d, want 400", ordinary.Code)
			}
			wantOrigin := ""
			if test.enabled {
				wantOrigin = "*"
			}
			if got := ordinary.Header().Get("Access-Control-Allow-Origin"); got != wantOrigin {
				t.Fatalf("ordinary origin = %q, want %q", got, wantOrigin)
			}
		})
	}
}
