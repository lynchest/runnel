package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
	"github.com/lynchest/runnel/internal/config"
)

func TestProxyCacheMissThenHitUsesOneUpstreamRequest(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "cached-value")
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
	first := serveAppRequest(t, app, http.MethodGet, path, "")
	if first.Code != http.StatusOK || first.Body.String() != "cached-value" {
		t.Fatalf("cache miss response = %d %q", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("cache miss X-Cache = %q, want MISS", got)
	}
	second := serveAppRequest(t, app, http.MethodGet, path, "")
	if second.Code != http.StatusOK || second.Body.String() != "cached-value" {
		t.Fatalf("cache hit response = %d %q", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("cache hit X-Cache = %q, want HIT", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	snapshot := app.Metrics().Snapshot()
	if snapshot.CacheMissesTotal != 1 || snapshot.CacheHitsTotal != 1 {
		t.Fatalf("cache metrics = %+v, want one miss and one hit", snapshot)
	}
}

func TestProxyCacheBypassWithNoCacheHeader(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "version-"+strconv.Itoa(int(count)))
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

	first := serveAppRequest(t, app, http.MethodGet, path, "")
	if first.Code != http.StatusOK || first.Body.String() != "version-1" {
		t.Fatalf("first response = %d %q, want version-1", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", got)
	}

	second := serveAppRequest(t, app, http.MethodGet, path, "")
	if second.Code != http.StatusOK || second.Body.String() != "version-1" {
		t.Fatalf("second response = %d %q, want version-1", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second X-Cache = %q, want HIT", got)
	}

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Cache-Control", "no-cache")
	third := httptest.NewRecorder()
	app.Handler().ServeHTTP(third, req)

	if third.Code != http.StatusOK || third.Body.String() != "version-2" {
		t.Fatalf("third response = %d %q, want version-2", third.Code, third.Body.String())
	}
	if got := third.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("third X-Cache = %q, want MISS", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestProxyRateLimitOpensHalfOpensAndClosesCircuit(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "probe-success")
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
	first := serveAppRequest(t, app, http.MethodGet, path, "")
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limit response = %d, want 429", first.Code)
	}
	domain := upstreamDomain(upstreamURL)
	breaker := app.Registry().Get(domain)
	if got := breaker.State(); got != circuit.StateOpen {
		t.Fatalf("state after 429 = %s, want %s", got, circuit.StateOpen)
	}
	second := serveAppRequest(t, app, http.MethodGet, path, "")
	if second.Code != http.StatusOK || second.Body.String() != "probe-success" {
		t.Fatalf("half-open response = %d %q", second.Code, second.Body.String())
	}
	if got := breaker.State(); got != circuit.StateClosed {
		t.Fatalf("state after successful probe = %s, want %s", got, circuit.StateClosed)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestProxyServesDurableStaleEntryWhileCircuitOpen(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "stale-value")
	})
	defer closeUpstream()

	cfg := testConfig(t)
	cfg.Defaults.DefaultCacheTTLSec = 1
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()
	path := "/proxy?url=" + url.QueryEscape(upstreamURL)
	first := serveAppRequest(t, app, http.MethodGet, path, "")
	if first.Code != http.StatusOK || first.Body.String() != "stale-value" {
		t.Fatalf("initial response = %d %q", first.Code, first.Body.String())
	}
	if err := app.cache.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	breaker := app.Registry().Get(upstreamDomain(upstreamURL))
	breaker.OnRateLimit(60)
	second := serveAppRequest(t, app, http.MethodGet, path, "")
	if second.Code != http.StatusOK || second.Body.String() != "stale-value" {
		t.Fatalf("stale response = %d %q", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-Cache"); got != "STALE" {
		t.Fatalf("stale X-Cache = %q, want STALE", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls while stale = %d, want 1", got)
	}
	if got := app.Metrics().Snapshot().CacheStaleHitsTotal; got != 1 {
		t.Fatalf("stale cache metric = %d, want 1", got)
	}
}

func TestProxyQueuedGETRecoversWithSingleProbe(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "recovered-value")
	})
	defer closeUpstream()

	cfg := testConfig(t)
	cfg.Defaults.QueueTimeoutSec = 3
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()
	first := serveAppRequest(t, app, http.MethodGet, "/proxy?url="+url.QueryEscape(upstreamURL+"/first"), "")
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("initial response = %d, want 429", first.Code)
	}

	result := make(chan *httptest.ResponseRecorder, 2)
	for _, target := range []string{"/second", "/third"} {
		path := "/proxy?url=" + url.QueryEscape(upstreamURL+target)
		go func(path string) {
			result <- serveAppRequest(t, app, http.MethodGet, path, "")
		}(path)
	}
	for request := 0; request < 2; request++ {
		select {
		case response := <-result:
			if response.Code != http.StatusOK || response.Body.String() != "recovered-value" {
				t.Fatalf("queued response = %d %q", response.Code, response.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued request was not released by recovery probe")
		}
	}

	breaker := app.Registry().Get(upstreamDomain(upstreamURL))
	if got := breaker.State(); got != circuit.StateClosed {
		t.Fatalf("state after queued probe = %s, want %s", got, circuit.StateClosed)
	}
	if got := app.queues[upstreamDomain(upstreamURL)].Len(); got != 0 {
		t.Fatalf("queue length after recovery = %d, want 0", got)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want one failure and two recovered requests", got)
	}
}

func TestProxyConfiguredAuthCookiesIsolateCacheEntries(t *testing.T) {
	var calls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "auth-value")
	})
	defer closeUpstream()

	cfg := testConfig(t)
	cfg.Domains = []config.DomainConfig{{
		Match:       upstreamDomain(upstreamURL),
		AuthCookies: []string{"tenant_session"},
	}}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown application: %v", err)
		}
	}()
	path := "/proxy?url=" + url.QueryEscape(upstreamURL+"/account")
	request := func(cookieValue string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "tenant_session", Value: cookieValue})
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, req)
		return recorder
	}

	first := request("one")
	second := request("two")
	third := request("one")
	for name, response := range map[string]*httptest.ResponseRecorder{
		"first":  first,
		"second": second,
		"third":  third,
	} {
		if response.Code != http.StatusOK || response.Body.String() != "auth-value" {
			t.Fatalf("%s response = %d %q", name, response.Code, response.Body.String())
		}
	}
	if got := first.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", got)
	}
	if got := second.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("second X-Cache = %q, want MISS", got)
	}
	if got := third.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("third X-Cache = %q, want HIT", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want one call per configured cookie value", got)
	}
}

func startIPv4Upstream(t *testing.T, handler http.HandlerFunc) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("host does not permit IPv4 loopback listeners: %v", err)
		}
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	closeServer := func() {
		if err := server.Close(); err != nil {
			t.Errorf("close upstream server: %v", err)
		}
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("upstream server did not stop")
		}
	}
	return "http://" + listener.Addr().String(), closeServer
}

func upstreamDomain(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
