package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHeaders_RemoveHopByHopAndConnectionTokens(t *testing.T) {
	headers := http.Header{
		"Connection":          {"keep-alive, X-Connection-Only"},
		"Keep-Alive":          {"timeout=5"},
		"Proxy-Authenticate":  {"Basic"},
		"Proxy-Authorization": {"Basic secret"},
		"TE":                  {"trailers"},
		"Trailer":             {"X-Trailer"},
		"Trailers":            {"X-Trailers"},
		"Transfer-Encoding":   {"chunked"},
		"Upgrade":             {"h2c"},
		"X-Connection-Only":   {"must-remove"},
		"X-End-To-End":        {"keep"},
		"lower-token":         {"keep"},
		"Connection-2":        {"keep"},
	}
	// Verify case-insensitive token removal even for manually constructed map
	// keys that net/http did not canonicalize.
	headers["connection"] = []string{"lower-token"}
	RemoveHopByHopHeaders(headers)

	for _, key := range []string{
		"Connection", "connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Trailers", "Transfer-Encoding",
		"Upgrade", "X-Connection-Only", "lower-token",
	} {
		for actual := range headers {
			if equalFold(actual, key) {
				t.Errorf("hop-by-hop header %q remains as %q", key, actual)
			}
		}
	}
	if headers.Get("X-End-To-End") != "keep" {
		t.Fatal("end-to-end header was removed")
	}
}

func TestHeaders_CORSPreflight(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "http://gateway.test/proxy", nil)
	if !WriteCORSPreflight(recorder, req) {
		t.Fatal("OPTIONS was not handled as preflight")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	for key, want := range map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers": "Authorization, Content-Type, Accept-Language, Cookie",
		"Access-Control-Max-Age":       "86400",
	} {
		if got := recorder.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	nonOptions := httptest.NewRecorder()
	if WriteCORSPreflight(nonOptions, httptest.NewRequest(http.MethodGet, "http://gateway.test/proxy", nil)) {
		t.Fatal("GET was handled as preflight")
	}
}

func TestHeaders_SynchronizeOutboundHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/path", nil)
	target, _ := url.Parse("https://api.example.test:8443/path")
	if err := SynchronizeOutboundHost(req, target); err != nil {
		t.Fatal(err)
	}
	if req.Host != "api.example.test:8443" || req.URL.Host != "api.example.test:8443" || req.URL.Scheme != "https" {
		t.Fatalf("host synchronization failed: Host=%q URL=%s scheme=%q", req.Host, req.URL.Host, req.URL.Scheme)
	}
}

func equalFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		l, r := left[i], right[i]
		if l >= 'A' && l <= 'Z' {
			l += 'a' - 'A'
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if l != r {
			return false
		}
	}
	return true
}

func BenchmarkRemoveHopByHopHeaders(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		headers := http.Header{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer secret"},
			"User-Agent":    {"runnel/1.0"},
			"Accept":        {"*/*"},
			"Connection":    {"keep-alive"},
			"Keep-Alive":    {"timeout=5"},
			"X-Custom-1":    {"val1"},
			"X-Custom-2":    {"val2"},
		}
		RemoveHopByHopHeaders(headers)
	}
}

func BenchmarkRemoveHopByHopHeaders_NoConnection(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		headers := http.Header{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer secret"},
			"User-Agent":    {"runnel/1.0"},
			"Accept":        {"*/*"},
			"X-Custom-1":    {"val1"},
			"X-Custom-2":    {"val2"},
		}
		RemoveHopByHopHeaders(headers)
	}
}
