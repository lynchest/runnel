package testutils

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
)

// ResponseRule defines how the mock server should respond to a request.
type ResponseRule struct {
	StatusCode int
	Headers    map[string]string
	Body       string
}

// MockUpstream is a deterministic mock HTTP upstream server for testing runnel.
type MockUpstream struct {
	Server       *httptest.Server
	URL          string
	mu           sync.RWMutex
	requestCount uint64
	rules        []ResponseRule
	defaultRule  ResponseRule
	requests     []*http.Request
}

// NewMockUpstream creates and starts a new MockUpstream server.
func NewMockUpstream(defaultRule ResponseRule) *MockUpstream {
	mock := &MockUpstream{
		defaultRule: defaultRule,
		rules:       make([]ResponseRule, 0),
		requests:    make([]*http.Request, 0),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", mock.handleRequest)

	mock.Server = httptest.NewUnstartedServer(mux)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("listen for IPv4 mock upstream: %v", err))
	}
	mock.Server.Listener = listener
	mock.Server.Start()
	mock.URL = mock.Server.URL
	return mock
}

// handleRequest dispatches responses based on queued rules or the default rule.
func (m *MockUpstream) handleRequest(w http.ResponseWriter, r *http.Request) {
	atomic.AddUint64(&m.requestCount, 1)

	m.mu.Lock()
	m.requests = append(m.requests, r.Clone(r.Context()))

	var rule ResponseRule
	if len(m.rules) > 0 {
		rule = m.rules[0]
		m.rules = m.rules[1:]
	} else {
		rule = m.defaultRule
	}
	m.mu.Unlock()

	for k, v := range rule.Headers {
		w.Header().Set(k, v)
	}

	statusCode := rule.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	w.WriteHeader(statusCode)
	if rule.Body != "" {
		_, _ = fmt.Fprint(w, rule.Body)
	}
}

// EnqueueRule enqueues a rule to be returned for the next request in FIFO order.
func (m *MockUpstream) EnqueueRule(rule ResponseRule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append(m.rules, rule)
}

// SetDefaultRule updates the fallback response rule when no queued rules remain.
func (m *MockUpstream) SetDefaultRule(rule ResponseRule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultRule = rule
}

// RequestCount returns the total number of requests handled.
func (m *MockUpstream) RequestCount() uint64 {
	return atomic.LoadUint64(&m.requestCount)
}

// Close stops the mock upstream server.
func (m *MockUpstream) Close() {
	if m.Server != nil {
		m.Server.Close()
	}
}
