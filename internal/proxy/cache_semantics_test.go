package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
	"github.com/lynchest/runnel/internal/queue"
	"github.com/lynchest/runnel/internal/storage"
)

// serveGatewayProxy issues a GET /proxy request with optional headers.
func serveGatewayProxy(t *testing.T, gateway *Gateway, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape(target), nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	gateway.ServeHTTP(recorder, request)
	return recorder
}

func TestGatewaySkipsStoreOnNoStore(t *testing.T) {
	cache := &memoryResponseCache{}
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Cache-Control": {"no-store"}, "Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("live")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	target := "https://api.test.com/no-store"
	for i := 0; i < 2; i++ {
		recorder := serveGatewayProxy(t, gateway, target, nil)
		if recorder.Code != http.StatusOK || recorder.Body.String() != "live" {
			t.Fatalf("response %d = %d %q, want 200 live", i+1, recorder.Code, recorder.Body.String())
		}
	}
	if cache.sets != 0 {
		t.Fatalf("cache writes = %d, want 0 for no-store", cache.sets)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestGatewaySkipsStoreOnPrivate(t *testing.T) {
	cache := &memoryResponseCache{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Cache-Control": {"private, max-age=60"}},
			Body:       io.NopCloser(strings.NewReader("personal")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	recorder := serveGatewayProxy(t, gateway, "https://api.test.com/private", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if cache.sets != 0 {
		t.Fatalf("cache writes = %d, want 0 for private", cache.sets)
	}
}

func TestGatewaySkipsStoreOnVaryStar(t *testing.T) {
	cache := &memoryResponseCache{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Vary": {"*"}},
			Body:       io.NopCloser(strings.NewReader("varied")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	serveGatewayProxy(t, gateway, "https://api.test.com/star", nil)
	if cache.sets != 0 {
		t.Fatalf("cache writes = %d, want 0 for Vary: *", cache.sets)
	}
}

func TestGatewayVaryMismatchIsMiss(t *testing.T) {
	cache := &memoryResponseCache{}
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Vary": {"X-Tier"}, "Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("tiered")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	target := "https://api.test.com/vary"

	first := serveGatewayProxy(t, gateway, target, map[string]string{"X-Tier": "free"})
	if got := first.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", got)
	}
	second := serveGatewayProxy(t, gateway, target, map[string]string{"X-Tier": "free"})
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("same Vary value X-Cache = %q, want HIT", got)
	}
	third := serveGatewayProxy(t, gateway, target, map[string]string{"X-Tier": "pro"})
	if got := third.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("changed Vary value X-Cache = %q, want MISS", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + mismatch refetch)", calls.Load())
	}
}

func TestGatewayLegacyEntryWithoutVaryIsMiss(t *testing.T) {
	cache := &memoryResponseCache{
		hit: true,
		// Zero-value literal: no Vary metadata, like a row written before
		// Vary tracking existed.
		entry: storage.CacheEntry{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/plain"}},
			Body:       []byte("legacy"),
		},
	}
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("fresh")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	recorder := serveGatewayProxy(t, gateway, "https://api.test.com/legacy", nil)
	if got := recorder.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("legacy entry X-Cache = %q, want MISS", got)
	}
	if recorder.Body.String() != "fresh" {
		t.Fatalf("body = %q, want fresh upstream response", recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestGatewayMaxAgeShortensTTL(t *testing.T) {
	cache := &memoryResponseCache{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Cache-Control": {"max-age=1"}},
			Body:       io.NopCloser(strings.NewReader("short")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	serveGatewayProxy(t, gateway, "https://api.test.com/short", nil)
	if cache.sets != 1 {
		t.Fatalf("cache writes = %d, want 1", cache.sets)
	}
	remaining := time.Until(cache.entry.ExpiresAt)
	if remaining <= 0 || remaining > 90*time.Second {
		t.Fatalf("entry TTL = %s, want about one second", remaining)
	}
}

func TestGatewaySingleflightIsolatesVariants(t *testing.T) {
	cache := &memoryResponseCache{}
	var calls atomic.Int32
	entered := make(chan string, 2)
	release := make(chan struct{})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		tier := r.Header.Get("X-Tier")
		calls.Add(1)
		entered <- tier
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Vary": {"X-Tier"}, "Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("tier-" + tier)),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	target := "https://api.test/concurrent-vary"
	results := make(chan string, 2)
	serve := func(tier string) {
		recorder := serveGatewayProxy(t, gateway, target, map[string]string{"X-Tier": tier})
		results <- recorder.Body.String()
	}
	go serve("free")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first fetch never reached upstream")
	}
	go serve("pro")
	// Give a coalesced second request every chance to attach to the first
	// flight before releasing it; isolation must hold regardless.
	time.Sleep(200 * time.Millisecond)
	close(release)
	got := make(map[string]int)
	for range 2 {
		select {
		case body := <-results:
			got[body]++
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent requests did not complete")
		}
	}
	if got["tier-free"] != 1 || got["tier-pro"] != 1 {
		t.Fatalf("variant bodies = %v, want one tier-free and one tier-pro", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 isolated flights", calls.Load())
	}
}

func TestGatewayMustRevalidateRefusesStale(t *testing.T) {
	serve := func(noStale bool) *httptest.ResponseRecorder {
		expired := storage.CacheEntry{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/plain"}},
			Body:       []byte("old"),
			ExpiresAt:  time.Now().Add(-time.Minute),
		}.WithVary(nil)
		expired.NoStale = noStale
		breaker := circuit.NewBreaker(circuit.Config{InitialCooldown: time.Minute, MaxCooldown: time.Minute}, nil)
		breaker.OnRateLimit(60)
		gateway := NewGateway(GatewayConfig{
			Breaker:          breaker,
			Cache:            &memoryResponseCache{hit: true, staleOnly: true, entry: expired},
			CacheTTL:         time.Hour,
			Queue:            queue.New(0, time.Second),
			ServeStaleOnOpen: true,
		})
		return serveGatewayProxy(t, gateway, "https://api.test/must-revalidate", nil)
	}
	refused := serve(true)
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("must-revalidate stale status = %d, want 503", refused.Code)
	}
	if got := refused.Header().Get("X-Cache"); got == "STALE" {
		t.Fatal("must-revalidate response served stale")
	}
	allowed := serve(false)
	if allowed.Code != http.StatusOK || allowed.Header().Get("X-Cache") != "STALE" {
		t.Fatalf("plain stale = %d %q, want 200 STALE", allowed.Code, allowed.Header().Get("X-Cache"))
	}
}

func TestGatewaySkipsStoreOnVaryRequestID(t *testing.T) {
	cache := &memoryResponseCache{}
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Vary": {"X-Request-ID"}, "Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("echo")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour})
	target := "https://api.test/vary-request-id"
	for range 2 {
		recorder := serveGatewayProxy(t, gateway, target, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if got := recorder.Header().Get("X-Cache"); got != "MISS" {
			t.Fatalf("X-Cache = %q, want MISS", got)
		}
	}
	if cache.sets != 0 {
		t.Fatalf("cache writes = %d, want 0 for Vary: X-Request-ID", cache.sets)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}
