package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
)

func TestDomainTrackingBound(t *testing.T) {
	metrics := &Metrics{}
	for i := range maxTrackedDomains {
		domain := "bound" + strconv.Itoa(i) + ".test"
		if metrics.Domain(domain) == nil {
			t.Fatalf("domain %d unexpectedly untracked", i)
		}
	}
	snapshot := metrics.Snapshot()
	if len(snapshot.Domains) != maxTrackedDomains {
		t.Fatalf("tracked domains = %d, want %d", len(snapshot.Domains), maxTrackedDomains)
	}
}

func TestDomainTrackingOverflowCountsDropped(t *testing.T) {
	metrics := &Metrics{}
	for i := range maxTrackedDomains {
		metrics.Domain("overflow" + strconv.Itoa(i) + ".test")
	}
	before := metrics.Snapshot().DroppedDomains
	if metrics.Domain("overflow.example.com") != nil {
		t.Fatal("overflow domain tracked, want nil")
	}
	if got := metrics.Snapshot().DroppedDomains; got != before+1 {
		t.Fatalf("dropped = %d, want %d", got, before+1)
	}
	// Untracked domains still feed globals.
	metrics.UpstreamRequestsTotal.Add(1)
	if got := metrics.Snapshot().UpstreamRequestsTotal; got != 1 {
		t.Fatalf("global upstream = %d, want 1", got)
	}
}

func TestDomainNormalization(t *testing.T) {
	metrics := &Metrics{}
	metrics.Domain("Example.COM.").UpstreamRequests.Add(1)
	dm := metrics.Domain("example.com")
	if dm == nil || dm.UpstreamRequests.Load() != 1 {
		t.Fatal("case/whitespace variants must share one domain entry")
	}
	if metrics.Domain("") != nil {
		t.Fatal("empty domain must not be tracked")
	}
}

func TestSnapshotQueueDepthSums(t *testing.T) {
	metrics := &Metrics{}
	metrics.SetQueueDepth("a.test", 3)
	metrics.SetQueueDepth("b.test", 4)
	metrics.SetQueueDepth("c.test", -2)
	snapshot := metrics.Snapshot()
	if snapshot.QueueDepth != 7 {
		t.Fatalf("total queue depth = %d, want 7", snapshot.QueueDepth)
	}
	if snapshot.Domains["c.test"].QueueDepth != 0 {
		t.Fatalf("negative depth = %d, want clamped 0", snapshot.Domains["c.test"].QueueDepth)
	}
}

func TestMetricsEndpointRendersDomains(t *testing.T) {
	metrics := &Metrics{}
	metrics.Domain("example.com").UpstreamRequests.Add(5)
	metrics.Domain("example.com").UpstreamRateLimited.Add(2)
	metrics.SetQueueDepth("example.com", 2)
	registry := circuit.NewRegistry(circuit.Config{}, nil)
	registry.Get("example.com").OnRateLimit(60)
	handler := NewAdminHandler(AdminConfig{Registry: registry, Metrics: metrics})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		"runnel_requests_total",
		`runnel_upstream_requests_total{domain="example.com"} 5`,
		`runnel_upstream_rate_limited_total{domain="example.com"} 2`,
		`runnel_queue_depth{domain="example.com"} 2`,
		`runnel_circuit_state{domain="example.com",state="OPEN"} 1`,
		"runnel_metrics_dropped_domains_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestEscapeLabelValue(t *testing.T) {
	if got := escapeLabelValue(`a"b\c` + "\n" + `d`); got != `a\"b\\c\nd` {
		t.Fatalf("escaped = %q", got)
	}
}

func TestDomainMetricsConcurrentSafe(t *testing.T) {
	metrics := &Metrics{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			domain := strings.Repeat("c", 2) + string(rune('a'+i)) + ".test"
			for j := 0; j < 100; j++ {
				if dm := metrics.Domain(domain); dm != nil {
					dm.UpstreamRequests.Add(1)
				}
				metrics.SetQueueDepth(domain, j)
				_ = metrics.Snapshot()
			}
		}(i)
	}
	wg.Wait()
}

func TestGatewayRecordsPerDomainMetrics(t *testing.T) {
	cache := &memoryResponseCache{}
	metrics := &Metrics{}
	breaker := circuit.NewBreaker(circuit.Config{InitialCooldown: time.Minute, MaxCooldown: time.Minute}, nil)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"60"}},
			Body:       io.NopCloser(strings.NewReader("limited")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{
		Client:   &http.Client{Transport: transport},
		Breaker:  breaker,
		Cache:    cache,
		CacheTTL: time.Hour,
		Metrics:  metrics,
	})
	target := "/proxy?url=" + url.QueryEscape("https://api.test/items")
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	dm := metrics.Domain("api.test")
	if dm == nil {
		t.Fatal("api.test untracked")
	}
	if got := dm.UpstreamRequests.Load(); got != 1 {
		t.Fatalf("domain upstream requests = %d, want 1", got)
	}
	if got := dm.UpstreamRateLimited.Load(); got != 1 {
		t.Fatalf("domain rate limited = %d, want 1", got)
	}
	// Non-idempotent method while OPEN counts a circuit rejection.
	post := httptest.NewRequest(http.MethodPost, target, nil)
	recorder = httptest.NewRecorder()
	gateway.ServeHTTP(recorder, post)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("post status = %d, want 503", recorder.Code)
	}
	if got := dm.CircuitRejected.Load(); got != 1 {
		t.Fatalf("domain circuit rejected = %d, want 1", got)
	}
	if got := metrics.Snapshot().CircuitRejectedTotal; got != 1 {
		t.Fatalf("global circuit rejected = %d, want 1", got)
	}
}

func TestQueueDepthTotalTracksDeltas(t *testing.T) {
	metrics := &Metrics{}
	metrics.SetQueueDepth("a.test", 3)
	metrics.SetQueueDepth("b.test", 4)
	if got := metrics.Snapshot().QueueDepth; got != 7 {
		t.Fatalf("total = %d, want 7", got)
	}
	metrics.SetQueueDepth("a.test", 1)
	if got := metrics.Snapshot().QueueDepth; got != 5 {
		t.Fatalf("total after drain = %d, want 5", got)
	}
}

func TestQueueDepthTotalIgnoresUntracked(t *testing.T) {
	metrics := &Metrics{}
	for i := range maxTrackedDomains {
		metrics.SetQueueDepth("fill"+strconv.Itoa(i)+".test", 1)
	}
	before := metrics.Snapshot().QueueDepth
	if before != maxTrackedDomains {
		t.Fatalf("total = %d, want %d", before, maxTrackedDomains)
	}
	// Beyond the bound the depth cannot be tracked exactly; the global
	// total must not move rather than drift.
	metrics.SetQueueDepth("overflow.test", 5)
	if got := metrics.Snapshot().QueueDepth; got != before {
		t.Fatalf("total after overflow = %d, want unchanged %d", got, before)
	}
}
