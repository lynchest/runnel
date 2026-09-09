package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

func TestRequestIDGeneratedAndReturned(t *testing.T) {
	logger, _ := testLogger()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("X-Request-ID"); got == "" {
			t.Error("upstream request missing X-Request-ID")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, CacheTTL: time.Hour, Logger: logger})
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape("https://api.test/items"), nil))
	id := recorder.Header().Get("X-Request-ID")
	if len(id) != 32 {
		t.Fatalf("generated request id = %q, want 32 hex chars", id)
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("generated request id = %q, want lowercase hex", id)
		}
	}
}

func TestValidInboundRequestIDPreserved(t *testing.T) {
	logger, _ := testLogger()
	var upstreamID string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamID = r.Header.Get("X-Request-ID")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, CacheTTL: time.Hour, Logger: logger})
	request := httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape("https://api.test/items"), nil)
	request.Header.Set("X-Request-ID", "client-trace_01.2")
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("X-Request-ID"); got != "client-trace_01.2" {
		t.Fatalf("response request id = %q, want preserved inbound", got)
	}
	if upstreamID != "client-trace_01.2" {
		t.Fatalf("upstream request id = %q, want preserved inbound", upstreamID)
	}
}

func TestInvalidInboundRequestIDReplaced(t *testing.T) {
	for _, inbound := range []string{
		"has space",
		"line\nbreak",
		"tab\there",
		strings.Repeat("a", maxRequestIDLen+1),
		"evil\"quote",
		"back\\slash",
	} {
		logger, _ := testLogger()
		gateway := NewGateway(GatewayConfig{CacheTTL: time.Hour, Logger: logger})
		request := httptest.NewRequest(http.MethodGet, "/proxy", nil)
		request.Header.Set("X-Request-ID", inbound)
		recorder := httptest.NewRecorder()
		// Missing target fails fast; the response must still carry a fresh ID.
		gateway.ServeHTTP(recorder, request)
		got := recorder.Header().Get("X-Request-ID")
		if got == inbound || validatedRequestID(got) == "" {
			t.Fatalf("inbound %q produced unsafe response id %q", inbound, got)
		}
	}
}

func TestAccessLogRedactsSecrets(t *testing.T) {
	logger, buf := testLogger()
	secret := "supersecret-token-value"
	target := "https://api.test/search?q=" + url.QueryEscape(secret)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: &memoryResponseCache{}, CacheTTL: time.Hour, Logger: logger})
	request := httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape(target), nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Cookie", "session="+secret)
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, request)
	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, secret) {
		t.Fatalf("access log leaks secret: %q", line)
	}
	if strings.Contains(line, "q=") || strings.Contains(line, "/search") {
		t.Fatalf("access log leaks URL path/query: %q", line)
	}
	for _, want := range []string{"method=GET", "domain=api.test", "status=200", "cache=miss", "request_id="} {
		if !strings.Contains(line, want) {
			t.Errorf("access log missing %q: %q", want, line)
		}
	}
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("access log must be one line: %q", line)
	}
}

func TestAccessLogRecordsError(t *testing.T) {
	logger, buf := testLogger()
	cache := &memoryResponseCache{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("bad")),
			Request:    r,
		}, nil
	})
	gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, Cache: cache, CacheTTL: time.Hour, Logger: logger})
	target := "/proxy?url=" + url.QueryEscape("https://api.test/flaky")
	gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	line := strings.TrimSpace(buf.String())
	for _, want := range []string{"status=502", "err=bad_gateway"} {
		if !strings.Contains(line, want) {
			t.Errorf("error access log missing %q: %q", want, line)
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		0:   "client_canceled",
		200: "",
		201: "",
		400: "bad_request",
		403: "forbidden",
		404: "not_found",
		413: "body_too_large",
		429: "rate_limited",
		500: "server_error",
		502: "bad_gateway",
		503: "unavailable",
		504: "gateway_timeout",
		418: "client_error",
	}
	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestSanitizeLogToken(t *testing.T) {
	if got := sanitizeLogToken(""); got != "-" {
		t.Fatalf("empty = %q, want -", got)
	}
	if got := sanitizeLogToken("api.test-1_x"); got != "api.test-1_x" {
		t.Fatalf("safe = %q", got)
	}
	if got := sanitizeLogToken("a b\nc\x00d"); got != "a?b?c?d" {
		t.Fatalf("unsafe = %q", got)
	}
}

func TestClassifyTransportError(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "gone.test", IsNotFound: true}
	if got := classifyTransportError(dns); got != "dns_error" {
		t.Fatalf("dns = %q", got)
	}
	if got := classifyTransportError(&url.Error{Op: "Get", URL: "https://x/", Err: dns}); got != "dns_error" {
		t.Fatalf("wrapped dns = %q", got)
	}
	if got := classifyTransportError(os.ErrDeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline = %q", got)
	}
	if got := classifyTransportError(x509.UnknownAuthorityError{}); got != "tls_error" {
		t.Fatalf("unknown authority = %q", got)
	}
	if got := classifyTransportError(x509.HostnameError{}); got != "tls_error" {
		t.Fatalf("hostname = %q", got)
	}
	if got := classifyTransportError(errors.New("tls: bad certificate")); got != "tls_error" {
		t.Fatalf("tls message = %q", got)
	}
	if got := classifyTransportError(errors.New("connection refused")); got != "upstream_error" {
		t.Fatalf("generic = %q", got)
	}
}

func TestUpstreamClassWrapPreservesMatching(t *testing.T) {
	wrapped := withUpstreamClass(context.DeadlineExceeded, "timeout")
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatal("wrapped error must still match DeadlineExceeded")
	}
	class, ok := upstreamClassOf(wrapped)
	if !ok || class != "timeout" {
		t.Fatalf("class = %q %v", class, ok)
	}
	if withUpstreamClass(nil, "timeout") != nil {
		t.Fatal("wrapping nil must stay nil")
	}
	if _, ok := upstreamClassOf(errors.New("plain")); ok {
		t.Fatal("plain error must have no class")
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestAccessLogUpstreamErrorClasses(t *testing.T) {
	serve := func(transport http.RoundTripper) string {
		logger, buf := testLogger()
		gateway := NewGateway(GatewayConfig{Client: &http.Client{Transport: transport}, CacheTTL: time.Hour, Logger: logger})
		recorder := httptest.NewRecorder()
		gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape("https://api.test/down"), nil))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", recorder.Code)
		}
		return strings.TrimSpace(buf.String())
	}
	dnsLine := serve(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "api.test", IsNotFound: true}
	}))
	if !strings.Contains(dnsLine, "err=dns_error") {
		t.Errorf("dns log = %q, want err=dns_error", dnsLine)
	}
	readLine := serve(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(failingReader{err: errors.New("boom")}),
			Request:    r,
		}, nil
	}))
	if !strings.Contains(readLine, "err=response_read_error") {
		t.Errorf("read log = %q, want err=response_read_error", readLine)
	}
	if strings.Contains(dnsLine, "api.test.") || strings.Contains(readLine, "no such host") {
		t.Errorf("error detail leaked into logs: %q / %q", dnsLine, readLine)
	}
}
