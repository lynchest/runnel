package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
	"github.com/lynchest/runnel/internal/queue"
	"github.com/lynchest/runnel/internal/storage"
	"golang.org/x/sync/singleflight"
)

const (
	defaultGatewayUpstreamTimeout = 15 * time.Second
	defaultGatewayQueueSize       = 200
	defaultGatewayQueueTimeout    = 15 * time.Second
	maxGatewayResponseBytes       = 64 * 1024 * 1024
)

var (
	ErrGatewayNilRequest       = errors.New("gateway request is nil")
	ErrGatewayMissingURL       = errors.New("gateway url query parameter is required")
	ErrGatewayResponseTooLarge = errors.New("upstream response is too large")
)

// ResponseCache is the typed cache seam used by Gateway. CacheStore is the
// production implementation; the interface also keeps gateway tests
// deterministic without requiring a database.
type ResponseCache interface {
	Get(context.Context, string) (storage.CacheEntry, bool, error)
	GetStale(context.Context, string) (storage.CacheEntry, bool, error)
	Set(context.Context, storage.CacheEntry) error
}

// RequestLimiter is the small admission seam needed by Gateway. limiter.Limiter
// satisfies it without making this package depend on a concrete limiter.
type RequestLimiter interface {
	Acquire(context.Context) error
}

// BreakerProvider resolves a per-domain circuit breaker.
type BreakerProvider func(string) *circuit.Breaker

// LimiterProvider resolves a per-domain rate limiter.
type LimiterProvider func(string) RequestLimiter

// QueueProvider resolves a per-domain queue used while a circuit is open.
type QueueProvider func(string) *queue.Queue

// AuthCookieProvider resolves the configured auth-cookie names for a domain.
// A nil or empty result uses Fingerprint's safe default cookie set.
type AuthCookieProvider func(string) []string

// GatewayResponse is a replayable upstream response. Buffering the bounded
// body is necessary because singleflight shares one upstream call among
// callers whose response bodies must remain independent.
type GatewayResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Clone returns an independent response value and body.
func (r GatewayResponse) Clone() GatewayResponse {
	return GatewayResponse{
		StatusCode: r.StatusCode,
		Header:     cloneHeader(r.Header),
		Body:       append([]byte(nil), r.Body...),
	}
}

// GatewayConfig configures a Gateway. Validator, Breaker, and Queue are
// optional; supplying them enables the corresponding safety controls. A nil
// validator still enforces absolute HTTP(S) URL syntax, which keeps local
// httptest transports usable while production callers can opt into SSRF pinning.
type GatewayConfig struct {
	Client           *http.Client
	Validator        *URLValidator
	Breaker          *circuit.Breaker
	BreakerFor       BreakerProvider
	Limiter          RequestLimiter
	LimiterFor       LimiterProvider
	Queue            *queue.Queue
	QueueFor         QueueProvider
	AuthCookiesFor   AuthCookieProvider
	EnableCORS       bool
	UpstreamTimeout  time.Duration
	QueueTimeout     time.Duration
	QueueSize        int
	ProbeSelector    *ProbeSelector
	Cache            ResponseCache
	CacheTTL         time.Duration
	ServeStaleOnOpen bool
	Metrics          *Metrics
}

// Gateway is an HTTP handler for /proxy?url=... and a typed singleflight
// coordinator for replayable upstream responses.
type Gateway struct {
	client           *http.Client
	validator        *URLValidator
	breaker          *circuit.Breaker
	breakerFor       BreakerProvider
	limiter          RequestLimiter
	limiterFor       LimiterProvider
	requestQueue     *queue.Queue
	queueFor         QueueProvider
	authCookiesFor   AuthCookieProvider
	enableCORS       bool
	upstreamTimeout  time.Duration
	probeSelector    *ProbeSelector
	cache            ResponseCache
	cacheTTL         time.Duration
	serveStaleOnOpen bool
	metrics          *Metrics
	requestGroup     singleflight.Group
}

// NewGateway creates a configured Gateway. The supplied client is copied
// before the redirect policy is installed, so constructing a gateway never
// mutates a caller-owned http.Client.
func NewGateway(config GatewayConfig) *Gateway {
	client := *http.DefaultClient
	if config.Client != nil {
		client = *config.Client
	}
	client.CheckRedirect = StopRedirects

	timeout := config.UpstreamTimeout
	if timeout <= 0 {
		timeout = defaultGatewayUpstreamTimeout
	}
	queueSize := config.QueueSize
	if queueSize == 0 {
		queueSize = defaultGatewayQueueSize
	}
	queueTimeout := config.QueueTimeout
	if queueTimeout == 0 {
		queueTimeout = defaultGatewayQueueTimeout
	}
	requestQueue := config.Queue
	if requestQueue == nil {
		requestQueue = queue.New(queueSize, queueTimeout)
	}
	selector := config.ProbeSelector
	if selector == nil {
		selector = NewProbeSelector(ProbeHeadersOnly)
	}
	return &Gateway{
		client:           &client,
		validator:        config.Validator,
		breaker:          config.Breaker,
		breakerFor:       config.BreakerFor,
		limiter:          config.Limiter,
		limiterFor:       config.LimiterFor,
		requestQueue:     requestQueue,
		queueFor:         config.QueueFor,
		authCookiesFor:   config.AuthCookiesFor,
		enableCORS:       config.EnableCORS,
		upstreamTimeout:  timeout,
		probeSelector:    selector,
		cache:            config.Cache,
		cacheTTL:         config.CacheTTL,
		serveStaleOnOpen: config.ServeStaleOnOpen,
		metrics:          config.Metrics,
	}
}

// Do coalesces duplicate calls by key. The first caller's values are used
// for the shared work, but context.WithoutCancel deliberately removes that
// client's cancellation from the upstream operation. Each caller still
// selects on its own context while waiting for DoChan, so a disconnected
// waiter returns without canceling or retaining the shared call.
func (g *Gateway) Do(ctx context.Context, key string, fn func(context.Context) (GatewayResponse, error)) (GatewayResponse, error) {
	if g == nil {
		return GatewayResponse{}, errors.New("gateway is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return GatewayResponse{}, errors.New("gateway operation is nil")
	}
	resultCh := g.requestGroup.DoChan(key, func() (interface{}, error) {
		workContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.operationTimeout())
		defer cancel()
		return fn(workContext)
	})
	select {
	case <-ctx.Done():
		return GatewayResponse{}, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return GatewayResponse{}, result.Err
		}
		response, ok := result.Val.(GatewayResponse)
		if !ok {
			return GatewayResponse{}, errors.New("gateway operation returned an invalid response")
		}
		return response.Clone(), nil
	}
}

// SelectProbe admits the lightest queued GET and applies the gateway's probe
// strategy. It is intended for the circuit worker that owns HALF-OPEN
// admission; ordinary handlers do not call it directly.
func (g *Gateway) SelectProbe(ctx context.Context) (*queue.Ticket, error) {
	if g == nil {
		return nil, errors.New("gateway is nil")
	}
	if g.requestQueue == nil {
		return nil, errors.New("gateway queue is nil")
	}
	selector := g.probeSelector
	if selector == nil {
		selector = NewProbeSelector(ProbeHeadersOnly)
	}
	return selector.Take(ctx, g.requestQueue)
}

// ServeHTTP validates the proxy target, applies circuit admission, and sends
// a replayable, hop-by-hop-cleaned upstream response to the client.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if w == nil || r == nil {
		return
	}
	if g == nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if g.metrics != nil {
		g.metrics.RequestsTotal.Add(1)
	}
	if g.enableCORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if WriteCORSPreflight(w, r) {
			return
		}
	}
	if r.URL == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	rawTarget := r.URL.Query().Get("url")
	if strings.TrimSpace(rawTarget) == "" {
		http.Error(w, ErrGatewayMissingURL.Error(), http.StatusBadRequest)
		return
	}
	target, validated, err := g.target(r.Context(), rawTarget)
	if err != nil {
		status := http.StatusBadRequest
		var validationErr *ValidationError
		if errors.As(err, &validationErr) {
			status = validationErr.StatusCode()
		}
		http.Error(w, http.StatusText(status), status)
		return
	}

	key := g.requestKey(r, target)
	domain := target.Hostname()
	cacheable := isCacheableMethod(r.Method)
	bypassCache := requestsCacheBypass(r)
	if cacheable && !bypassCache && g.cache != nil && g.cacheTTL > 0 {
		entry, hit, cacheErr := g.cache.Get(r.Context(), key)
		if cacheErr != nil {
			if g.metrics != nil {
				g.metrics.CacheErrorsTotal.Add(1)
			}
		} else if hit && isCacheableResponseStatus(entry.StatusCode) {
			if g.metrics != nil {
				g.metrics.CacheHitsTotal.Add(1)
			}
			writeCacheEntry(w, entry, "HIT")
			return
		} else if g.metrics != nil {
			g.metrics.CacheMissesTotal.Add(1)
		}
	}

	breaker := g.breakerForDomain(domain)
	requestQueue := g.queueForDomain(domain)
	if g.serveStaleOnOpen && cacheable && g.cacheTTL > 0 && g.serveStale(w, r, key, breaker) {
		return
	}
	if !g.admitWith(w, r, breaker, requestQueue) {
		return
	}
	if limiter := g.limiterForDomain(domain); limiter != nil {
		if err := limiter.Acquire(r.Context()); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if g.metrics != nil {
				g.metrics.RateLimitedTotal.Add(1)
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}
	// A queued request may have been released while CLOSED and then delayed by
	// the limiter. Refuse it if another request reopened the circuit meanwhile.
	postLimitSnapshot := circuit.Snapshot{}
	if breaker != nil {
		postLimitSnapshot = breaker.Snapshot()
	}
	if postLimitSnapshot.State == circuit.StateOpen {
		if g.metrics != nil {
			g.metrics.CircuitRejectedTotal.Add(1)
		}
		writeUnavailable(w, postLimitSnapshot.RemainingCooldown)
		return
	}

	fetch := func(ctx context.Context) (GatewayResponse, error) {
		return g.fetchWithBreaker(ctx, r, target, validated, breaker)
	}
	var response GatewayResponse
	if isNonIdempotentMethod(r.Method) {
		response, err = g.doBounded(r.Context(), fetch)
	} else {
		response, err = g.Do(r.Context(), key, fetch)
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if g.serveStaleOnOpen && cacheable && g.cacheTTL > 0 && g.serveStale(w, r, key, breaker) {
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	if cacheable && g.cache != nil && g.cacheTTL > 0 && isCacheableResponseStatus(response.StatusCode) {
		if response.Header == nil {
			response.Header = make(http.Header)
		}
		response.Header.Set("X-Cache", "MISS")
		cacheHeaders := response.Header.Clone()
		stripSensitiveCacheHeaders(cacheHeaders)
		entry := storage.CacheEntry{
			Key:        key,
			StatusCode: response.StatusCode,
			Headers:    cacheHeaders,
			Body:       append([]byte(nil), response.Body...),
			ExpiresAt:  time.Now().Add(g.cacheTTL),
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		if err := g.cache.Set(r.Context(), entry); err != nil && g.metrics != nil {
			g.metrics.CacheWriteErrorsTotal.Add(1)
		}
	}
	writeGatewayResponse(w, response)
}

func isCacheableResponseStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices && status != http.StatusPartialContent
}

func stripSensitiveCacheHeaders(header http.Header) {
	if header == nil {
		return
	}
	for name := range header {
		if strings.EqualFold(name, "Set-Cookie") || strings.EqualFold(name, "WWW-Authenticate") || strings.EqualFold(name, "Proxy-Authenticate") {
			delete(header, name)
		}
	}
}

func (g *Gateway) doBounded(ctx context.Context, fn func(context.Context) (GatewayResponse, error)) (GatewayResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workContext, cancel := context.WithTimeout(ctx, g.operationTimeout())
	defer cancel()
	return fn(workContext)
}

func (g *Gateway) operationTimeout() time.Duration {
	if g == nil || g.upstreamTimeout <= 0 {
		return defaultGatewayUpstreamTimeout
	}
	return g.upstreamTimeout
}

func (g *Gateway) target(ctx context.Context, rawTarget string) (*url.URL, *ValidatedTarget, error) {
	if g.validator != nil {
		validated, err := g.validator.Validate(ctx, rawTarget)
		if err != nil {
			return nil, nil, err
		}
		return validated.URL, validated, nil
	}
	target, err := url.Parse(rawTarget)
	if err != nil || target == nil || !isHTTPOrHTTPS(target.Scheme) || target.Host == "" || target.Hostname() == "" || target.User != nil {
		if err == nil {
			err = ErrInvalidTarget
		}
		return nil, nil, err
	}
	return target, nil, nil
}

func (g *Gateway) breakerForDomain(domain string) *circuit.Breaker {
	if g == nil {
		return nil
	}
	if g.breakerFor != nil {
		if breaker := g.breakerFor(domain); breaker != nil {
			return breaker
		}
	}
	return g.breaker
}

func (g *Gateway) limiterForDomain(domain string) RequestLimiter {
	if g == nil {
		return nil
	}
	if g.limiterFor != nil {
		if limiter := g.limiterFor(domain); limiter != nil {
			return limiter
		}
	}
	return g.limiter
}

func (g *Gateway) queueForDomain(domain string) *queue.Queue {
	if g == nil {
		return nil
	}
	if g.queueFor != nil {
		if requestQueue := g.queueFor(domain); requestQueue != nil {
			return requestQueue
		}
	}
	return g.requestQueue
}

func (g *Gateway) requestKey(r *http.Request, target *url.URL) string {
	if g == nil || g.authCookiesFor == nil || target == nil {
		return gatewayRequestKey(r, target)
	}
	return gatewayRequestKeyWithCookies(r, target, g.authCookiesFor(target.Hostname()))
}

func (g *Gateway) serveStale(w http.ResponseWriter, r *http.Request, key string, breaker *circuit.Breaker) bool {
	if g == nil || g.cache == nil || breaker == nil || r == nil {
		return false
	}
	snapshot := breaker.Snapshot()
	if snapshot.State != circuit.StateOpen || snapshot.RemainingCooldown <= 0 {
		return false
	}
	entry, hit, err := g.cache.GetStale(r.Context(), key)
	if err != nil || !hit || !isCacheableResponseStatus(entry.StatusCode) {
		return false
	}
	if entry.Valid(time.Now()) {
		if g.metrics != nil {
			g.metrics.CacheHitsTotal.Add(1)
		}
		writeCacheEntry(w, entry, "HIT")
		return true
	}
	if g.metrics != nil {
		g.metrics.CacheStaleHitsTotal.Add(1)
	}
	writeCacheEntry(w, entry, "STALE")
	return true
}

func isCacheableMethod(method string) bool {
	return strings.EqualFold(strings.TrimSpace(method), http.MethodGet) ||
		strings.EqualFold(strings.TrimSpace(method), http.MethodHead)
}

func requestsCacheBypass(r *http.Request) bool {
	if r == nil {
		return false
	}
	cc := strings.ToLower(r.Header.Get("Cache-Control"))
	if strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") || strings.Contains(cc, "max-age=0") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Pragma")), "no-cache")
}

func (g *Gateway) admitWith(w http.ResponseWriter, r *http.Request, breaker *circuit.Breaker, requestQueue *queue.Queue) bool {
	if breaker == nil {
		return true
	}
	snapshot := breaker.Snapshot()
	if snapshot.State == circuit.StateClosed {
		if !breaker.CanExecute() {
			return g.waitInQueueWithQueue(w, r, requestQueue, snapshot.RemainingCooldown)
		}
		return true
	}
	// A write with side effects must not be parked or used as a HALF-OPEN
	// probe. Return its cooldown immediately. HEAD is idempotent and may wait,
	// although it is intentionally not selected as the probe itself.
	if isNonIdempotentMethod(r.Method) {
		if g.metrics != nil {
			g.metrics.CircuitRejectedTotal.Add(1)
		}
		writeUnavailable(w, snapshot.RemainingCooldown)
		return false
	}
	if snapshot.State == circuit.StateHalfOpen {
		if isProbeGETMethod(r.Method) {
			if breaker.CanExecute() {
				return true
			}
			return g.waitInQueueWithQueue(w, r, requestQueue, 0)
		}
		return g.waitInQueueWithQueue(w, r, requestQueue, snapshot.RemainingCooldown)
	}
	if breaker.CanExecute() {
		return true
	}
	return g.waitInQueueWithQueue(w, r, requestQueue, snapshot.RemainingCooldown)
}

func (g *Gateway) waitInQueueWithQueue(w http.ResponseWriter, r *http.Request, requestQueue *queue.Queue, retryAfter time.Duration) bool {
	if requestQueue == nil {
		if g.metrics != nil {
			g.metrics.QueueRejectedTotal.Add(1)
		}
		writeUnavailable(w, retryAfter)
		return false
	}
	item := queue.NewItem(r, requestWeight(r))
	item.RetryAfter = retryAfter
	ticket, err := requestQueue.Add(r.Context(), item)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		if errors.Is(err, queue.ErrFull) {
			if g.metrics != nil {
				g.metrics.QueueRejectedTotal.Add(1)
			}
			writeUnavailable(w, retryAfter)
			return false
		}
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return false
	}
	if g.metrics != nil {
		g.metrics.SetQueueDepth(requestQueue.Len())
	}
	err = requestQueue.Wait(r.Context(), ticket)
	if g.metrics != nil {
		g.metrics.SetQueueDepth(requestQueue.Len())
	}
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var timeoutErr *queue.TimeoutError
	if errors.As(err, &timeoutErr) {
		w.Header().Set("Retry-After", timeoutErr.RetryAfterHeader())
		w.WriteHeader(http.StatusServiceUnavailable)
		return false
	}
	writeUnavailable(w, retryAfter)
	return false
}

func (g *Gateway) fetchWithBreaker(ctx context.Context, original *http.Request, target *url.URL, validated *ValidatedTarget, breaker *circuit.Breaker) (result GatewayResponse, err error) {
	if original == nil || target == nil {
		return GatewayResponse{}, ErrGatewayNilRequest
	}
	if g.metrics != nil {
		g.metrics.UpstreamRequestsTotal.Add(1)
	}
	outbound := original.Clone(ctx)
	targetCopy := *target
	outbound.URL = &targetCopy
	outbound.RequestURI = ""
	if err := SynchronizeOutboundHost(outbound, &targetCopy); err != nil {
		return GatewayResponse{}, err
	}
	StripHopByHopRequest(outbound)

	client := g.client
	if client == nil {
		client = http.DefaultClient
	}
	if validated != nil && g.validator != nil {
		copy := *client
		copy.Transport = g.validator.NewSafeTransport(validated)
		client = &copy
	}
	response, err := client.Do(outbound)
	if err != nil {
		if g.metrics != nil {
			g.metrics.UpstreamErrorsTotal.Add(1)
		}
		if breaker != nil {
			breaker.OnFailure()
		}
		return GatewayResponse{}, err
	}
	if response == nil {
		if g.metrics != nil {
			g.metrics.UpstreamErrorsTotal.Add(1)
		}
		if breaker != nil {
			breaker.OnFailure()
		}
		return GatewayResponse{}, errors.New("upstream returned a nil response")
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	if response.Request == nil {
		response.Request = outbound
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		if err := RewriteResponseLocation(response); err != nil {
			return GatewayResponse{}, err
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGatewayResponseBytes+1))
	if err != nil {
		if g.metrics != nil {
			g.metrics.UpstreamErrorsTotal.Add(1)
		}
		if breaker != nil {
			breaker.OnFailure()
		}
		return GatewayResponse{}, err
	}
	if int64(len(body)) > maxGatewayResponseBytes {
		if g.metrics != nil {
			g.metrics.UpstreamErrorsTotal.Add(1)
		}
		if breaker != nil {
			breaker.OnFailure()
		}
		return GatewayResponse{}, ErrGatewayResponseTooLarge
	}
	result = GatewayResponse{StatusCode: response.StatusCode, Header: cloneHeader(response.Header), Body: body}
	RemoveHopByHopHeaders(result.Header)
	if breaker != nil {
		switch {
		case response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable:
			if g.metrics != nil {
				g.metrics.RateLimitedTotal.Add(1)
			}
			breaker.OnRateLimitHeader(response.Header.Get("Retry-After"))
		case response.StatusCode >= http.StatusInternalServerError:
			breaker.OnFailure()
		case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusBadRequest:
			breaker.OnSuccess()
		}
	}
	return result, nil
}

func writeCacheEntry(w http.ResponseWriter, entry storage.CacheEntry, cacheState string) {
	header := entry.Headers.Clone()
	stripSensitiveCacheHeaders(header)
	if cacheState != "" {
		header.Set("X-Cache", cacheState)
	}
	if cacheState == "STALE" {
		header.Set("Warning", `110 - "Response is stale"`)
	}
	writeGatewayResponse(w, GatewayResponse{
		StatusCode: entry.StatusCode,
		Header:     header,
		Body:       entry.Payload(),
	})
}

func writeGatewayResponse(w http.ResponseWriter, response GatewayResponse) {
	header := cloneHeader(response.Header)
	RemoveHopByHopHeaders(header)
	for key, values := range header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := response.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(response.Body)
}

func writeUnavailable(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter < 0 {
		retryAfter = 0
	}
	seconds := int64(0)
	if retryAfter > 0 {
		seconds = int64((retryAfter + time.Second - 1) / time.Second)
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	w.WriteHeader(http.StatusServiceUnavailable)
}

func gatewayRequestKey(r *http.Request, target *url.URL) string {
	return gatewayRequestKeyWithCookies(r, target, nil)
}

func gatewayRequestKeyWithCookies(r *http.Request, target *url.URL, authCookieNames []string) string {
	clone := r.Clone(context.Background())
	clone.URL = target
	if len(authCookieNames) > 0 {
		return Fingerprint(clone, authCookieNames)
	}
	return Fingerprint(clone)
}

func requestWeight(r *http.Request) int64 {
	if r == nil || r.ContentLength < 0 {
		return 0
	}
	return r.ContentLength
}

func isProbeGETMethod(method string) bool {
	return strings.EqualFold(strings.TrimSpace(method), http.MethodGet)
}

func isNonIdempotentMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return make(http.Header)
	}
	return header.Clone()
}
