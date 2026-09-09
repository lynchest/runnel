package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
)

// Metrics contains the process counters exposed by the administrative
// metrics endpoint. The global counters are deliberately small and typed so
// request handlers can update them without taking a shared mutex.
// Per-domain counters live behind a bounded map (see maxTrackedDomains) so
// an empty domain allowlist cannot grow memory or scrape size without limit;
// domains beyond the bound still feed the global counters.
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
	droppedDomains        atomic.Uint64
	// totalQueueDepth is the exact sum of last-reported per-domain depths,
	// maintained by Swap deltas so the global gauge does not depend on the
	// bounded series map. Domains beyond the tracking bound still miss
	// here; tracking them exactly would need unbounded memory, which is
	// what the bound exists to prevent.
	totalQueueDepth atomic.Int64

	domainMu sync.Mutex
	domains  map[string]*DomainMetrics
}

// maxTrackedDomains bounds per-domain metric series. Untracked domains keep
// working and keep feeding the global counters; only their domain-labelled
// breakdown is missing, counted by DroppedDomains.
const maxTrackedDomains = 256

// DomainMetrics holds one domain's counters. Fields are atomic so handlers
// update them without holding the parent map lock.
type DomainMetrics struct {
	UpstreamRequests    atomic.Uint64
	UpstreamErrors      atomic.Uint64
	UpstreamRateLimited atomic.Uint64
	CircuitRejected     atomic.Uint64
	QueueRejected       atomic.Uint64
	QueueDepth          atomic.Int64
}

// DomainMetricsSnapshot is an atomically captured view of DomainMetrics.
type DomainMetricsSnapshot struct {
	UpstreamRequests    uint64
	UpstreamErrors      uint64
	UpstreamRateLimited uint64
	CircuitRejected     uint64
	QueueRejected       uint64
	QueueDepth          int64
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
	DroppedDomains        uint64
	Domains               map[string]DomainMetricsSnapshot
}

// normalizeMetricDomain canonicalizes a domain for metric labelling.
func normalizeMetricDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// Domain returns the counters for domain, creating them on first use. It
// returns nil once the tracking bound is reached; callers must handle nil by
// relying on the global counters alone.
func (m *Metrics) Domain(domain string) *DomainMetrics {
	if m == nil {
		return nil
	}
	name := normalizeMetricDomain(domain)
	if name == "" {
		return nil
	}
	m.domainMu.Lock()
	defer m.domainMu.Unlock()
	if dm := m.domains[name]; dm != nil {
		return dm
	}
	if len(m.domains) >= maxTrackedDomains {
		m.droppedDomains.Add(1)
		return nil
	}
	if m.domains == nil {
		m.domains = make(map[string]*DomainMetrics)
	}
	dm := &DomainMetrics{}
	m.domains[name] = dm
	return dm
}

// Snapshot returns all counters at one instant.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	snapshot := MetricsSnapshot{
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
		DroppedDomains:        m.droppedDomains.Load(),
		QueueDepth:            max(m.totalQueueDepth.Load(), 0),
		Domains:               make(map[string]DomainMetricsSnapshot),
	}
	m.domainMu.Lock()
	for name, dm := range m.domains {
		if dm == nil {
			continue
		}
		depth := dm.QueueDepth.Load()
		if depth < 0 {
			depth = 0
		}
		snapshot.Domains[name] = DomainMetricsSnapshot{
			UpstreamRequests:    dm.UpstreamRequests.Load(),
			UpstreamErrors:      dm.UpstreamErrors.Load(),
			UpstreamRateLimited: dm.UpstreamRateLimited.Load(),
			CircuitRejected:     dm.CircuitRejected.Load(),
			QueueRejected:       dm.QueueRejected.Load(),
			QueueDepth:          depth,
		}
	}
	m.domainMu.Unlock()
	return snapshot
}

// SetQueueDepth records one domain queue's depth and folds the delta into
// the global total, which therefore stays exact without scanning the
// bounded series map.
func (m *Metrics) SetQueueDepth(domain string, depth int) {
	dm := m.Domain(domain)
	if dm == nil {
		return
	}
	if depth < 0 {
		depth = 0
	}
	previous := dm.QueueDepth.Swap(int64(depth))
	m.totalQueueDepth.Add(int64(depth) - previous)
}

// escapeLabelValue escapes a Prometheus label value.
func escapeLabelValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
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
	writeMetric("runnel_metrics_dropped_domains_total", snapshot.DroppedDomains)
	names := make([]string, 0, len(snapshot.Domains))
	for name := range snapshot.Domains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dm := snapshot.Domains[name]
		label := `{domain="` + escapeLabelValue(name) + `"}`
		writeMetric("runnel_upstream_requests_total"+label, dm.UpstreamRequests)
		writeMetric("runnel_upstream_errors_total"+label, dm.UpstreamErrors)
		writeMetric("runnel_upstream_rate_limited_total"+label, dm.UpstreamRateLimited)
		writeMetric("runnel_circuit_rejected_total"+label, dm.CircuitRejected)
		writeMetric("runnel_queue_rejected_total"+label, dm.QueueRejected)
		_, _ = w.Write([]byte("runnel_queue_depth" + label + " " + strconv.FormatInt(dm.QueueDepth, 10) + "\n"))
		if handler != nil && handler.registry != nil {
			if breaker, ok := handler.registry.Lookup(name); ok && breaker != nil {
				state := string(breaker.State())
				_, _ = w.Write([]byte(`runnel_circuit_state{domain="` + escapeLabelValue(name) + `",state="` + state + `"} 1` + "\n"))
			}
		}
	}
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
