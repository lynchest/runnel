package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
)

// Metrics contains the process counters exposed by the administrative
// metrics endpoint. The counters are deliberately small and typed so request
// handlers can update them without taking a shared mutex.
type Metrics struct {
	RequestsTotal         atomic.Uint64
	CacheHitsTotal        atomic.Uint64
	CacheMissesTotal      atomic.Uint64
	CacheStaleHitsTotal   atomic.Uint64
	CacheErrorsTotal      atomic.Uint64
	CacheWriteErrorsTotal atomic.Uint64
	UpstreamRequestsTotal atomic.Uint64
	UpstreamErrorsTotal   atomic.Uint64
	RateLimitedTotal      atomic.Uint64
	CircuitRejectedTotal  atomic.Uint64
	QueueRejectedTotal    atomic.Uint64
	queueDepth            atomic.Int64
}

// MetricsSnapshot is an atomically captured view of Metrics.
type MetricsSnapshot struct {
	RequestsTotal         uint64
	CacheHitsTotal        uint64
	CacheMissesTotal      uint64
	CacheStaleHitsTotal   uint64
	CacheErrorsTotal      uint64
	CacheWriteErrorsTotal uint64
	UpstreamRequestsTotal uint64
	UpstreamErrorsTotal   uint64
	RateLimitedTotal      uint64
	CircuitRejectedTotal  uint64
	QueueRejectedTotal    uint64
	QueueDepth            int64
}

// Snapshot returns all counters at one instant.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		RequestsTotal:         m.RequestsTotal.Load(),
		CacheHitsTotal:        m.CacheHitsTotal.Load(),
		CacheMissesTotal:      m.CacheMissesTotal.Load(),
		CacheStaleHitsTotal:   m.CacheStaleHitsTotal.Load(),
		CacheErrorsTotal:      m.CacheErrorsTotal.Load(),
		CacheWriteErrorsTotal: m.CacheWriteErrorsTotal.Load(),
		UpstreamRequestsTotal: m.UpstreamRequestsTotal.Load(),
		UpstreamErrorsTotal:   m.UpstreamErrorsTotal.Load(),
		RateLimitedTotal:      m.RateLimitedTotal.Load(),
		CircuitRejectedTotal:  m.CircuitRejectedTotal.Load(),
		QueueRejectedTotal:    m.QueueRejectedTotal.Load(),
		QueueDepth:            m.queueDepth.Load(),
	}
}

// SetQueueDepth updates the queue depth gauge shown in /_metrics.
func (m *Metrics) SetQueueDepth(depth int) {
	if m == nil {
		return
	}
	if depth < 0 {
		depth = 0
	}
	m.queueDepth.Store(int64(depth))
}

// AdminConfig wires the administrative endpoints to application state.
// HealthCheck is optional; a nil check means the process is considered
// healthy after its dependencies have been assembled.
type AdminConfig struct {
	Registry    *circuit.Registry
	Metrics     *Metrics
	AdminToken  string
	HealthCheck func(context.Context) error
}

// AdminHandler serves process health, circuit state, metrics, and the guarded
// circuit reset operation. It does not listen on its own socket: the caller
// mounts it on a server whose default address is loopback.
type AdminHandler struct {
	registry    *circuit.Registry
	metrics     *Metrics
	adminToken  string
	healthCheck func(context.Context) error
}

// NewAdminHandler constructs an administrative handler from typed state.
func NewAdminHandler(config AdminConfig) *AdminHandler {
	return &AdminHandler{
		registry:    config.Registry,
		metrics:     config.Metrics,
		adminToken:  config.AdminToken,
		healthCheck: config.HealthCheck,
	}
}

// ServeHTTP dispatches the four administrative endpoints.
func (handler *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if w == nil || r == nil {
		return
	}
	if r.URL == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case "/_healthz":
		handler.health(w, r)
	case "/_circuit":
		handler.circuit(w, r)
	case "/_metrics":
		handler.metricsEndpoint(w, r)
	case "/_circuit/reset":
		handler.reset(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (handler *AdminHandler) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminMethodNotAllowed(w, http.MethodGet)
		return
	}
	if handler != nil && handler.healthCheck != nil {
		if err := handler.healthCheck(r.Context()); err != nil {
			writeAdminJSON(w, http.StatusServiceUnavailable, marshalHealthJSON(adminHealthResponse{
				Status: "unhealthy",
				Error:  err.Error(),
			}))
			return
		}
	}
	writeAdminJSON(w, http.StatusOK, marshalHealthJSON(adminHealthResponse{Status: "ok"}))
}

func (handler *AdminHandler) circuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminMethodNotAllowed(w, http.MethodGet)
		return
	}
	snapshots := make(map[string]adminCircuitState)
	if handler != nil && handler.registry != nil {
		for domain, snapshot := range handler.registry.Snapshots() {
			snapshots[domain] = adminCircuitState{
				State:               snapshot.State,
				BlockedUntil:        snapshot.BlockedUntil,
				ConsecutiveFailures: snapshot.ConsecutiveFailures,
				RemainingCooldown:   snapshot.RemainingCooldown.String(),
				ProbeInFlight:       snapshot.ProbeInFlight,
			}
		}
	}
	writeAdminJSON(w, http.StatusOK, marshalCircuitJSON(adminCircuitResponse{Circuits: snapshots}))
}

func (handler *AdminHandler) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	snapshot := MetricsSnapshot{}
	if handler != nil {
		snapshot = handler.metrics.Snapshot()
	}
	writeMetric := func(name string, value uint64) {
		_, _ = w.Write([]byte(name + " " + strconv.FormatUint(value, 10) + "\n"))
	}
	writeMetric("runnel_requests_total", snapshot.RequestsTotal)
	writeMetric("runnel_cache_hits_total", snapshot.CacheHitsTotal)
	writeMetric("runnel_cache_misses_total", snapshot.CacheMissesTotal)
	writeMetric("runnel_cache_stale_hits_total", snapshot.CacheStaleHitsTotal)
	writeMetric("runnel_cache_errors_total", snapshot.CacheErrorsTotal)
	writeMetric("runnel_cache_write_errors_total", snapshot.CacheWriteErrorsTotal)
	writeMetric("runnel_upstream_requests_total", snapshot.UpstreamRequestsTotal)
	writeMetric("runnel_upstream_errors_total", snapshot.UpstreamErrorsTotal)
	writeMetric("runnel_rate_limited_total", snapshot.RateLimitedTotal)
	writeMetric("runnel_circuit_rejected_total", snapshot.CircuitRejectedTotal)
	writeMetric("runnel_queue_rejected_total", snapshot.QueueRejectedTotal)
	_, _ = w.Write([]byte("runnel_queue_depth " + strconv.FormatInt(snapshot.QueueDepth, 10) + "\n"))
}

func (handler *AdminHandler) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminMethodNotAllowed(w, http.MethodPost)
		return
	}
	if handler == nil || handler.adminToken == "" || !constantTimeToken(adminToken(r), handler.adminToken) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if handler.registry == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		handler.registry.ResetAll()
		writeAdminJSON(w, http.StatusOK, marshalResetJSON(adminResetResponse{Status: "reset"}))
		return
	}
	if !handler.registry.Reset(domain) {
		http.Error(w, "circuit not found", http.StatusNotFound)
		return
	}
	writeAdminJSON(w, http.StatusOK, marshalResetJSON(adminResetResponse{Status: "reset", Domain: domain}))
}

func adminToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	if token := strings.TrimSpace(r.Header.Get("X-Admin-Token")); token != "" {
		return token
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authorization) >= len("Bearer ") && strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(authorization[len("Bearer "):])
	}
	return ""
}

func constantTimeToken(provided, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

type adminHealthResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type adminCircuitResponse struct {
	Circuits map[string]adminCircuitState `json:"circuits"`
}

type adminCircuitState struct {
	State               circuit.State `json:"state"`
	BlockedUntil        time.Time     `json:"blocked_until,omitempty"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
	RemainingCooldown   string        `json:"remaining_cooldown"`
	ProbeInFlight       bool          `json:"probe_in_flight"`
}

type adminResetResponse struct {
	Status string `json:"status"`
	Domain string `json:"domain,omitempty"`
}

func marshalHealthJSON(value adminHealthResponse) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"failed to encode response"}`)
	}
	return payload
}

func marshalCircuitJSON(value adminCircuitResponse) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"failed to encode response"}`)
	}
	return payload
}

func marshalResetJSON(value adminResetResponse) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"failed to encode response"}`)
	}
	return payload
}

func writeAdminJSON(w http.ResponseWriter, status int, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

func writeAdminMethodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	w.WriteHeader(http.StatusMethodNotAllowed)
}
