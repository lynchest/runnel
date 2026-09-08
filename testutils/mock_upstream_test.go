package testutils_test

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/lynchest/runnel/testutils"
)

func TestMockUpstream_DeterministicStatusCodesAndHeaders(t *testing.T) {
	probeListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("host does not permit IPv4 loopback listeners: %v", err)
		}
		t.Fatal(err)
	}
	if err := probeListener.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	mock := testutils.NewMockUpstream(testutils.ResponseRule{
		StatusCode: http.StatusOK,
		Body:       "default-ok",
	})
	defer mock.Close()

	// Enqueue 200, 429, 503 with custom headers
	mock.EnqueueRule(testutils.ResponseRule{
		StatusCode: http.StatusOK,
		Headers: map[string]string{
			"X-Custom-Header": "probe-test-200",
		},
		Body: `{"status":"ok"}`,
	})
	mock.EnqueueRule(testutils.ResponseRule{
		StatusCode: http.StatusTooManyRequests,
		Headers: map[string]string{
			"Retry-After": "60",
			"X-RateLimit": "exceeded",
		},
		Body: `{"error":"rate limited"}`,
	})
	mock.EnqueueRule(testutils.ResponseRule{
		StatusCode: http.StatusServiceUnavailable,
		Headers: map[string]string{
			"Retry-After": "120",
		},
		Body: `{"error":"upstream down"}`,
	})

	client := mock.Server.Client()

	// Step 1: 200 OK
	resp1, err := client.Get(mock.URL + "/items")
	if err != nil {
		t.Fatalf("request 1 failed: %v", err)
	}
	defer func() {
		if err := resp1.Body.Close(); err != nil {
			t.Errorf("close response 1: %v", err)
		}
	}()

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("request 1 expected status 200, got %d", resp1.StatusCode)
	}
	if val := resp1.Header.Get("X-Custom-Header"); val != "probe-test-200" {
		t.Errorf("request 1 expected X-Custom-Header probe-test-200, got %q", val)
	}
	body1, _ := io.ReadAll(resp1.Body)
	if string(body1) != `{"status":"ok"}` {
		t.Errorf("request 1 expected body {\"status\":\"ok\"}, got %q", string(body1))
	}

	// Step 2: 429 Too Many Requests
	resp2, err := client.Get(mock.URL + "/items")
	if err != nil {
		t.Fatalf("request 2 failed: %v", err)
	}
	defer func() {
		if err := resp2.Body.Close(); err != nil {
			t.Errorf("close response 2: %v", err)
		}
	}()

	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("request 2 expected status 429, got %d", resp2.StatusCode)
	}
	if val := resp2.Header.Get("Retry-After"); val != "60" {
		t.Errorf("request 2 expected Retry-After 60, got %q", val)
	}
	if val := resp2.Header.Get("X-RateLimit"); val != "exceeded" {
		t.Errorf("request 2 expected X-RateLimit exceeded, got %q", val)
	}
	body2, _ := io.ReadAll(resp2.Body)
	if string(body2) != `{"error":"rate limited"}` {
		t.Errorf("request 2 expected body {\"error\":\"rate limited\"}, got %q", string(body2))
	}

	// Step 3: 503 Service Unavailable
	resp3, err := client.Get(mock.URL + "/items")
	if err != nil {
		t.Fatalf("request 3 failed: %v", err)
	}
	defer func() {
		if err := resp3.Body.Close(); err != nil {
			t.Errorf("close response 3: %v", err)
		}
	}()

	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("request 3 expected status 503, got %d", resp3.StatusCode)
	}
	if val := resp3.Header.Get("Retry-After"); val != "120" {
		t.Errorf("request 3 expected Retry-After 120, got %q", val)
	}

	// Step 4: Default Fallback (Rule queue exhausted)
	resp4, err := client.Get(mock.URL + "/fallback")
	if err != nil {
		t.Fatalf("request 4 failed: %v", err)
	}
	defer func() {
		if err := resp4.Body.Close(); err != nil {
			t.Errorf("close response 4: %v", err)
		}
	}()

	if resp4.StatusCode != http.StatusOK {
		t.Errorf("request 4 expected default status 200, got %d", resp4.StatusCode)
	}
	body4, _ := io.ReadAll(resp4.Body)
	if string(body4) != "default-ok" {
		t.Errorf("request 4 expected default body default-ok, got %q", string(body4))
	}

	if count := mock.RequestCount(); count != 4 {
		t.Errorf("expected 4 total requests handled, got %d", count)
	}
}
