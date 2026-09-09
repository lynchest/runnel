package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestProxyHonorsUpstreamNoStore(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("live"))
	})
	defer closeUpstream()

	app, err := NewApplication(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()
	path := "/proxy?url=" + url.QueryEscape(upstreamURL)
	for i := 0; i < 2; i++ {
		recorder := serveAppRequest(t, app, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK || recorder.Body.String() != "live" {
			t.Fatalf("response %d = %d %q, want 200 live", i+1, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("X-Cache"); got != "MISS" {
			t.Fatalf("response %d X-Cache = %q, want MISS", i+1, got)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 for no-store", got)
	}
}

func TestProxyHonorsUpstreamVary(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Vary", "X-Tier")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("tiered"))
	})
	defer closeUpstream()

	app, err := NewApplication(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()
	path := "/proxy?url=" + url.QueryEscape(upstreamURL)
	serve := func(tier string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Tier", tier)
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	if got := serve("free").Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", got)
	}
	if got := serve("free").Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("same tier X-Cache = %q, want HIT", got)
	}
	if got := serve("pro").Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("changed tier X-Cache = %q, want MISS", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestProxySkipsVaryStarResponses(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Vary", "*")
		_, _ = w.Write([]byte("varied"))
	})
	defer closeUpstream()

	app, err := NewApplication(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()
	path := "/proxy?url=" + url.QueryEscape(upstreamURL)
	for i := 0; i < 2; i++ {
		serveAppRequest(t, app, http.MethodGet, path, "")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 for Vary: *", got)
	}
}
