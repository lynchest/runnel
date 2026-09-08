package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestApplicationSoakBoundedHeap sends 10,000 cache-hit requests with 32
// concurrent workers. HeapAlloc is measured after forced GC; the generous
// 128 MiB ceiling catches unbounded retention while avoiding a machine-specific
// absolute RSS/15 MiB assertion. Production RSS remains an external measure.
func TestApplicationSoakBoundedHeap(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstreamURL, closeUpstream := startIPv4Upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.WriteString(w, "soak-value")
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
	warm := serveAppRequest(t, app, http.MethodGet, path, "")
	if warm.Code != http.StatusOK || warm.Body.String() != "soak-value" {
		t.Fatalf("warm request = %d %q", warm.Code, warm.Body.String())
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const (
		totalRequests = 10000
		concurrency   = 32
	)
	var failures atomic.Int64
	jobs := make(chan struct{}, concurrency)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for worker := 0; worker < concurrency; worker++ {
		go func() {
			defer workers.Done()
			for range jobs {
				response := serveAppRequest(t, app, http.MethodGet, path, "")
				if response.Code != http.StatusOK || response.Body.String() != "soak-value" {
					failures.Add(1)
				}
			}
		}()
	}
	for request := 0; request < totalRequests; request++ {
		jobs <- struct{}{}
	}
	close(jobs)
	workers.Wait()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growth := uint64(0)
	if after.HeapAlloc > before.HeapAlloc {
		growth = after.HeapAlloc - before.HeapAlloc
	}
	t.Logf("soak requests=%d concurrency=%d upstream_calls=%d HeapAlloc_before=%d HeapAlloc_after=%d growth=%d", totalRequests, concurrency, upstreamCalls.Load(), before.HeapAlloc, after.HeapAlloc, growth)
	if failures.Load() != 0 {
		t.Fatalf("soak response failures = %d", failures.Load())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want one warm-cache call", got)
	}
	if growth > 128*1024*1024 {
		t.Fatalf("heap growth = %d bytes, exceeds 128 MiB ceiling", growth)
	}
}
