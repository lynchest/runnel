package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
	"github.com/lynchest/runnel/internal/queue"
	"github.com/lynchest/runnel/internal/storage"
)

type memoryResponseCache struct {
	entry storage.CacheEntry
	hit   bool
	sets  int
	// staleOnly makes Get miss while GetStale still hits, modelling an
	// expired durable row for stale-fallback tests.
	staleOnly bool
}

func (c *memoryResponseCache) Get(context.Context, string) (storage.CacheEntry, bool, error) {
	if c.staleOnly {
		return storage.CacheEntry{}, false, nil
	}
	return c.entry.Clone(), c.hit, nil
}

func (c *memoryResponseCache) GetStale(context.Context, string) (storage.CacheEntry, bool, error) {
	return c.entry.Clone(), c.hit, nil
}

func (c *memoryResponseCache) Set(_ context.Context, entry storage.CacheEntry) error {
	c.entry = entry.Clone()
	c.hit = true
	c.sets++
	return nil
}

type breakerOpeningLimiter struct{ breaker *circuit.Breaker }

func (l breakerOpeningLimiter) Acquire(context.Context) error {
	l.breaker.OnFailure()
	return nil
}

func TestGatewaySingleflightFirstClientCancelOthersSurvive(t *testing.T) {
	gateway := NewGateway(GatewayConfig{UpstreamTimeout: time.Second})
	firstContext, cancelFirst := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	operation := func(context.Context) (GatewayResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return GatewayResponse{StatusCode: http.StatusOK, Body: []byte("shared")}, nil
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := gateway.Do(firstContext, "same-request", operation)
		firstResult <- err
	}()
	<-started

	const waiters = 19
	results := make(chan error, waiters)
	var wg sync.WaitGroup
	wg.Add(waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			defer wg.Done()
			_, err := gateway.Do(context.Background(), "same-request", operation)
			results <- err
		}()
	}
	// Give all callers a chance to enter DoChan before the leader is canceled.
	time.Sleep(20 * time.Millisecond)
	cancelFirst()
	close(release)

	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller error = %v, want context canceled", err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("waiting caller error = %v, want success", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("shared operation calls = %d, want 1", got)
	}
}

func TestGatewayNonIdempotentBoundedPathPreservesCancellation(t *testing.T) {
	gateway := NewGateway(GatewayConfig{UpstreamTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	observed := make(chan error, 1)
	done := make(chan error, 1)

	go func() {
		_, err := gateway.doBounded(ctx, func(workContext context.Context) (GatewayResponse, error) {
			close(started)
			<-workContext.Done()
			observed <- workContext.Err()
			return GatewayResponse{}, workContext.Err()
		})
		done <- err
	}()

	<-started
	cancel()
	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("bounded operation context error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded operation did not observe caller cancellation")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("bounded request error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded request did not return after caller cancellation")
	}
}

func TestGatewayRedirectRelativeLocationRewritten(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/start" {
			return nil, errors.New("unexpected upstream path: " + r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"/v2/games"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape("https://api.test.com/start"), nil)
	gateway.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("gateway status = %d, want 302", recorder.Code)
	}
	want := "/proxy?url=" + url.QueryEscape("https://api.test.com/v2/games")
	if got := recorder.Header().Get("Location"); got != want {
		t.Fatalf("rewritten Location = %q, want %q", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayOpenCircuitRejectsNonIdempotentImmediately(t *testing.T) {
	breaker := circuit.NewBreaker(circuit.Config{
		InitialCooldown: time.Minute,
		MaxCooldown:     time.Minute,
	}, nil)
	breaker.OnRateLimit(60)
	q := queue.New(4, time.Second)
	gateway := NewGateway(GatewayConfig{Breaker: breaker, Queue: q})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/proxy?url=https%3A%2F%2Fapi.test%2Fwrite", nil)
	gateway.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("POST Retry-After = %q, want 60", got)
	}
	if q.Len() != 0 {
		t.Fatalf("POST was retained in queue, length = %d", q.Len())
	}
}

func TestGatewayCanceledOpenCircuitWaitersAreRemoved(t *testing.T) {
	breaker := circuit.NewBreaker(circuit.Config{
		InitialCooldown: time.Minute,
		MaxCooldown:     time.Minute,
	}, nil)
	breaker.OnRateLimit(60)
	q := queue.New(200, time.Second)
	gateway := NewGateway(GatewayConfig{Breaker: breaker, Queue: q})
	baseline := runtime.NumGoroutine()
	const callers = 100
	var wg sync.WaitGroup
	wg.Add(callers)
	cancels := make([]context.CancelFunc, 0, callers)
	var cancelMu sync.Mutex
	for i := 0; i < callers; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancelMu.Lock()
		cancels = append(cancels, cancel)
		cancelMu.Unlock()
		go func(ctx context.Context) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/proxy?url=https%3A%2F%2Fapi.test%2Fitems", nil).WithContext(ctx)
			gateway.ServeHTTP(recorder, request)
		}(ctx)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for q.Len() < callers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancelMu.Lock()
	for _, cancel := range cancels {
		cancel()
	}
	cancelMu.Unlock()
	wg.Wait()
	waitForProxyQueueLen(t, q, 0)

	deadline = time.Now().Add(500 * time.Millisecond)
	for runtime.NumGoroutine() > baseline+20 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+20 {
		t.Fatalf("goroutines after canceled gateway waiters = %d, baseline %d", got, baseline)
	}
}

func TestGatewayQueueTimeoutIncludesRemainingRetryAfter(t *testing.T) {
	breaker := circuit.NewBreaker(circuit.Config{
		InitialCooldown: time.Minute,
		MaxCooldown:     time.Minute,
	}, nil)
	breaker.OnRateLimit(60)
	q := queue.New(1, 30*time.Millisecond)
	gateway := NewGateway(GatewayConfig{Breaker: breaker, Queue: q})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/proxy?url=https%3A%2F%2Fapi.test%2Fitems", nil)
	gateway.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("queue timeout status = %d, want 503", recorder.Code)
	}
	if got, err := strconv.ParseInt(recorder.Header().Get("Retry-After"), 10, 64); err != nil || got < 1 {
		t.Fatalf("queue timeout Retry-After = %q, want positive seconds", recorder.Header().Get("Retry-After"))
	}
}

func TestGatewayCacheStripsSensitiveHeaders(t *testing.T) {
	cache := &memoryResponseCache{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":       {"text/plain"},
				"Set-Cookie":         {"session=secret"},
				"WWW-Authenticate":   {"Bearer private"},
				"Proxy-Authenticate": {"Basic private"},
			},
			Body:    io.NopCloser(strings.NewReader("ok")),
			Request: r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{
		Client:   &http.Client{Transport: transport},
		Cache:    cache,
		CacheTTL: time.Minute,
	})
	target := "/proxy?url=" + url.QueryEscape("https://api.test.com/cookie")

	first := httptest.NewRecorder()
	gateway.ServeHTTP(first, httptest.NewRequest(http.MethodGet, target, nil))
	if first.Header().Get("Set-Cookie") != "session=secret" {
		t.Fatal("original upstream response lost Set-Cookie")
	}
	second := httptest.NewRecorder()
	gateway.ServeHTTP(second, httptest.NewRequest(http.MethodGet, target, nil))
	if second.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second response X-Cache = %q, want HIT", second.Header().Get("X-Cache"))
	}
	for _, name := range []string{"Set-Cookie", "WWW-Authenticate", "Proxy-Authenticate"} {
		if got := second.Header().Get(name); got != "" {
			t.Errorf("cached response %s = %q, want empty", name, got)
		}
	}
	if got := second.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("cached Content-Type = %q, want text/plain", got)
	}
}

func TestGatewayDoesNotCachePartialContent(t *testing.T) {
	cache := &memoryResponseCache{}
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": {"bytes 0-99/1000"}},
			Body:       io.NopCloser(strings.NewReader("partial")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Minute})
	target := "/proxy?url=" + url.QueryEscape("https://api.test.com/file")
	for i := 0; i < 2; i++ {
		recorder := httptest.NewRecorder()
		gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusPartialContent {
			t.Fatalf("response %d status = %d, want 206", i+1, recorder.Code)
		}
	}
	if cache.sets != 0 {
		t.Fatalf("partial response cache writes = %d, want 0", cache.sets)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestGatewayIgnoresLegacyPartialContentCacheEntry(t *testing.T) {
	cache := &memoryResponseCache{
		hit: true,
		entry: storage.CacheEntry{
			StatusCode: http.StatusPartialContent,
			Headers:    http.Header{"Content-Range": {"bytes 0-99/1000"}},
			Body:       []byte("partial"),
		},
	}
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("complete")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Minute})
	recorder := httptest.NewRecorder()
	target := "/proxy?url=" + url.QueryEscape("https://api.test.com/legacy-file")
	gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "complete" {
		t.Fatalf("response = %d %q, want 200 complete", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", recorder.Header().Get("X-Cache"))
	}
	if calls.Load() != 1 || cache.sets != 1 {
		t.Fatalf("upstream calls = %d, cache writes = %d; want 1 and 1", calls.Load(), cache.sets)
	}
}

func TestGatewayNonPositiveCacheTTLDisablesCache(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			cache := &memoryResponseCache{}
			var calls atomic.Int32
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
			})
			gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: ttl})
			target := "/proxy?url=" + url.QueryEscape("https://api.test.com/no-cache")
			for i := 0; i < 2; i++ {
				gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
			}
			if cache.sets != 0 || calls.Load() != 2 {
				t.Fatalf("ttl %s: cache writes %d, upstream calls %d; want 0 and 2", ttl, cache.sets, calls.Load())
			}
		})
	}
}

func TestGatewayRechecksCircuitAfterLimiterDelay(t *testing.T) {
	breaker := circuit.NewBreaker(circuit.Config{InitialCooldown: time.Minute, MaxCooldown: time.Minute}, nil)
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})
	gateway := NewGateway(GatewayConfig{
		Client:  &http.Client{Transport: transport},
		Breaker: breaker,
		Limiter: breakerOpeningLimiter{breaker: breaker},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/proxy?url=https%3A%2F%2Fapi.test%2Fitems", nil)
	gateway.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func waitForProxyQueueLen(t *testing.T, q *queue.Queue, want int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for q.Len() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := q.Len(); got != want {
		t.Fatalf("queue length = %d, want %d", got, want)
	}
}
