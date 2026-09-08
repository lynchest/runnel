package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lynchest/runnel/internal/circuit"
	"github.com/lynchest/runnel/internal/config"
	"github.com/lynchest/runnel/internal/limiter"
	"github.com/lynchest/runnel/internal/proxy"
	"github.com/lynchest/runnel/internal/queue"
	"github.com/lynchest/runnel/internal/storage"
)

const (
	defaultShutdownTimeout = 10 * time.Second
	defaultDirtyBufferTTL  = storage.DefaultDirtyTTL
	probeWorkerIdleWait    = 100 * time.Millisecond
	probeWorkerRetryWait   = 10 * time.Millisecond
	probeWorkerTakeWait    = 25 * time.Millisecond
)

// Application owns the complete runnel process graph. Construction opens
// durable dependencies, while Serve starts background workers; this makes
// tests able to assemble an app and provide an explicitly IPv4 listener.
type Application struct {
	cfg *config.Config

	store  *storage.SQLiteStore
	dirty  *storage.DirtyBuffer
	writer *storage.AsyncWriter
	cache  *storage.CacheStore

	registry *circuit.Registry
	metrics  *proxy.Metrics
	gateway  *proxy.Gateway
	admin    *proxy.AdminHandler
	server   *http.Server
	cleaner  *storage.Cleaner

	policyMu sync.Mutex
	queues   map[string]*queue.Queue
	limiters map[string]*limiter.Limiter

	workerMu     sync.Mutex
	workerCtx    context.Context
	workerCancel context.CancelFunc
	probeWorkers map[string]struct{}
	probeWG      sync.WaitGroup
	workersStart bool
	stopped      bool
	stopOnce     sync.Once
}

// NewApplication loads no files and uses the supplied configuration. A nil
// configuration selects the documented defaults. Call Serve with a listener,
// or use ListenAndServe in a long-running process.
func NewApplication(cfg *config.Config) (*Application, error) {
	return newApplication(cfg, nil, nil)
}

func newApplication(cfg *config.Config, client *http.Client, resolver proxy.Resolver) (*Application, error) {
	if cfg == nil {
		cfg = config.NewDefaultConfig()
	}
	copyConfig := *cfg
	if strings.TrimSpace(copyConfig.Security.AdminToken) == "" {
		copyConfig.Security.AdminToken = strings.TrimSpace(os.Getenv("RUNNEL_ADMIN_TOKEN"))
	}
	if h := strings.TrimSpace(os.Getenv("RUNNEL_HOST")); h != "" {
		copyConfig.Server.Host = h
	}
	if p := strings.TrimSpace(os.Getenv("RUNNEL_PORT")); p != "" {
		if portNum, err := strconv.Atoi(p); err == nil && portNum > 0 && portNum <= 65535 {
			copyConfig.Server.Port = portNum
		}
	}
	if err := copyConfig.Validate(); err != nil {
		return nil, fmt.Errorf("validate application config: %w", err)
	}
	store, err := storage.OpenSQLite(copyConfig.Storage.DBPath)
	if err != nil {
		return nil, err
	}
	dirty := storage.NewDirtyBuffer(defaultDirtyBufferTTL, nil)
	writer, err := storage.NewAsyncWriter(store.Repository(), dirty, storage.WriterOptions{
		QueueSize:     storage.DefaultWriterBufferSize,
		BatchSize:     copyConfig.Storage.WriteBatchSize,
		FlushInterval: time.Duration(copyConfig.Storage.WriteFlushIntervalMS) * time.Millisecond,
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	cache, err := storage.NewCacheStore(store.Repository(), dirty, writer)
	if err != nil {
		_ = writer.Shutdown(context.Background())
		_ = store.Close()
		return nil, err
	}

	defaultCircuitConfig := circuit.Config{
		InitialCooldown:    time.Duration(copyConfig.Defaults.CooldownInitialSec) * time.Second,
		CooldownMultiplier: copyConfig.Defaults.CooldownMultiplier,
		MaxCooldown:        time.Duration(copyConfig.Defaults.CooldownMaxSec) * time.Second,
	}
	registry := circuit.NewRegistry(defaultCircuitConfig, nil)
	metrics := &proxy.Metrics{}
	validator := proxy.NewURLValidator(resolver, copyConfig.Security.AllowedDomains)
	validator.AllowPrivateIPs = !copyConfig.Security.BlockPrivateIPs
	app := &Application{
		cfg:          &copyConfig,
		store:        store,
		dirty:        dirty,
		writer:       writer,
		cache:        cache,
		registry:     registry,
		metrics:      metrics,
		queues:       make(map[string]*queue.Queue),
		limiters:     make(map[string]*limiter.Limiter),
		probeWorkers: make(map[string]struct{}),
	}
	app.cleaner = storage.NewCleaner(store.Repository(), storage.CleanerOptions{
		Interval:          time.Duration(copyConfig.Storage.CacheCleanupIntervalSec) * time.Second,
		MaxBytes:          int64(copyConfig.Storage.MaxCacheSizeMB) * 1024 * 1024,
		EvictionBatchSize: storage.DefaultEvictionBatchSize,
	})
	app.gateway = proxy.NewGateway(proxy.GatewayConfig{
		Client:           client,
		Validator:        validator,
		BreakerFor:       app.breakerFor,
		LimiterFor:       app.limiterFor,
		QueueFor:         app.queueFor,
		UpstreamTimeout:  time.Duration(copyConfig.Server.WriteTimeoutSec) * time.Second,
		QueueTimeout:     time.Duration(copyConfig.Defaults.QueueTimeoutSec) * time.Second,
		QueueSize:        copyConfig.Defaults.QueueMaxSize,
		ProbeSelector:    proxy.NewProbeSelector(proxy.ProbeStrategy(copyConfig.Defaults.ProbeStrategy)),
		AuthCookiesFor:   app.authCookiesFor,
		EnableCORS:       copyConfig.Security.EnableCORS,
		Cache:            cache,
		CacheTTL:         time.Duration(copyConfig.Defaults.DefaultCacheTTLSec) * time.Second,
		ServeStaleOnOpen: copyConfig.Defaults.ServeStaleOnOpen,
		Metrics:          metrics,
	})
	app.admin = proxy.NewAdminHandler(proxy.AdminConfig{
		Registry:    registry,
		Metrics:     metrics,
		AdminToken:  copyConfig.Security.AdminToken,
		HealthCheck: app.health,
	})
	app.server = &http.Server{
		ReadTimeout:  time.Duration(copyConfig.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(copyConfig.Server.WriteTimeoutSec) * time.Second,
		Handler:      app.routes(),
	}
	app.server.RegisterOnShutdown(func() { _ = app.stopBackground(context.Background()) })
	return app, nil
}

// Listen opens the configured address using an IPv4 TCP listener. The
// documented default host is loopback, so administrative endpoints are not
// exposed beyond the local machine unless configuration explicitly changes it.
func (app *Application) Listen() (net.Listener, error) {
	if app == nil || app.cfg == nil {
		return nil, errors.New("application is nil")
	}
	host := strings.TrimSpace(app.cfg.Server.Host)
	if host == "" {
		host = config.DefaultServerHost
	}
	address := net.JoinHostPort(host, strconv.Itoa(app.cfg.Server.Port))
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	return listener, nil
}

// Serve starts background workers and serves HTTP on listener until it is
// closed. A normal Shutdown returns a nil error from Serve.
func (app *Application) Serve(listener net.Listener) error {
	if app == nil || app.server == nil {
		return errors.New("application is nil")
	}
	if listener == nil {
		return errors.New("application listener is nil")
	}
	app.startWorkers()
	err := app.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ListenAndServe is the blocking process entry point used by callers that do
// not need to provide their own listener.
func (app *Application) ListenAndServe() error {
	listener, err := app.Listen()
	if err != nil {
		return err
	}
	return app.Serve(listener)
}

// Shutdown performs the required order: stop accepting HTTP and drain active
// handlers, stop producers/background workers, drain accepted cache writes,
// then run SQLite's TRUNCATE checkpoint and close the store.
func (app *Application) Shutdown(ctx context.Context) error {
	if app == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var shutdownErrors []error
	if app.server != nil {
		if err := app.server.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	if err := app.stopBackground(ctx); err != nil {
		shutdownErrors = append(shutdownErrors, err)
	}
	if app.cache != nil {
		if err := app.cache.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	if app.store != nil {
		if err := app.store.Close(); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	return errors.Join(shutdownErrors...)
}

// Handler returns the fully composed HTTP handler, useful for in-process
// integration tests.
func (app *Application) Handler() http.Handler {
	if app == nil || app.server == nil {
		return http.NotFoundHandler()
	}
	return app.server.Handler
}

// Gateway returns the configured proxy gateway.
func (app *Application) Gateway() *proxy.Gateway {
	if app == nil {
		return nil
	}
	return app.gateway
}

// Registry returns the per-domain circuit registry.
func (app *Application) Registry() *circuit.Registry {
	if app == nil {
		return nil
	}
	return app.registry
}

// Metrics returns the process counters.
func (app *Application) Metrics() *proxy.Metrics {
	if app == nil {
		return nil
	}
	return app.metrics
}

// Store returns the SQLite store owned by the application.
func (app *Application) Store() *storage.SQLiteStore {
	if app == nil {
		return nil
	}
	return app.store
}

func (app *Application) routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil || r.URL == nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/proxy":
			if !proxy.EnforceBodyLimit(w, r, app.cfg.Server.MaxBodyBytes) {
				return
			}
			app.gateway.ServeHTTP(w, r)
		case "/_healthz", "/_circuit", "/_metrics", "/_circuit/reset":
			app.admin.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (app *Application) startWorkers() {
	if app == nil || app.cleaner == nil {
		return
	}
	app.workerMu.Lock()
	if app.workersStart || app.stopped {
		app.workerMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.workerCtx = ctx
	app.workerCancel = cancel
	app.workersStart = true
	app.workerMu.Unlock()
	app.cleaner.Start(ctx)
}

func (app *Application) stopBackground(ctx context.Context) error {
	if app == nil {
		return nil
	}
	app.stopOnce.Do(func() {
		app.workerMu.Lock()
		cancel := app.workerCancel
		app.workerCancel = nil
		app.stopped = true
		app.workerMu.Unlock()
		if cancel != nil {
			cancel()
		}
		app.policyMu.Lock()
		for _, requestQueue := range app.queues {
			requestQueue.Close()
		}
		app.policyMu.Unlock()
	})
	app.probeWG.Wait()
	if app.cleaner == nil {
		return nil
	}
	return app.cleaner.Shutdown(ctx)
}

func (app *Application) breakerFor(domain string) *circuit.Breaker {
	canonical := canonicalDomain(domain)
	policy := app.domainPolicy(canonical)
	app.registry.SetConfig(canonical, circuit.Config{
		InitialCooldown:    time.Duration(policy.CooldownInitialSec) * time.Second,
		CooldownMultiplier: policy.CooldownMultiplier,
		MaxCooldown:        time.Duration(policy.CooldownMaxSec) * time.Second,
	})
	return app.registry.Get(canonical)
}

func (app *Application) limiterFor(domain string) proxy.RequestLimiter {
	canonical := canonicalDomain(domain)
	app.policyMu.Lock()
	defer app.policyMu.Unlock()
	if rateLimiter := app.limiters[canonical]; rateLimiter != nil {
		return rateLimiter
	}
	policy := app.domainPolicy(canonical)
	options := make([]limiter.Option, 0, 1)
	if policy.JitterMaxMS > 0 {
		options = append(options, limiter.WithJitter(limiter.NewJitter(
			time.Duration(policy.JitterMinMS)*time.Millisecond,
			time.Duration(policy.JitterMaxMS)*time.Millisecond,
		)))
	}
	rateLimiter := limiter.New(policy.RequestsPerSec, 1, options...)
	app.limiters[canonical] = rateLimiter
	return rateLimiter
}

func (app *Application) queueFor(domain string) *queue.Queue {
	app.startWorkers()
	canonical := canonicalDomain(domain)
	app.policyMu.Lock()
	if requestQueue := app.queues[canonical]; requestQueue != nil {
		app.policyMu.Unlock()
		return requestQueue
	}
	policy := app.domainPolicy(canonical)
	requestQueue := queue.New(policy.QueueMaxSize, time.Duration(policy.QueueTimeoutSec)*time.Second)
	app.queues[canonical] = requestQueue
	app.policyMu.Unlock()
	if !app.startProbeWorker(canonical, requestQueue) {
		requestQueue.Close()
	}
	return requestQueue
}

func (app *Application) startProbeWorker(domain string, requestQueue *queue.Queue) bool {
	if app == nil || requestQueue == nil {
		return false
	}
	app.workerMu.Lock()
	if !app.workersStart || app.stopped || app.workerCtx == nil {
		app.workerMu.Unlock()
		return false
	}
	if _, exists := app.probeWorkers[domain]; exists {
		app.workerMu.Unlock()
		return true
	}
	ctx := app.workerCtx
	app.probeWorkers[domain] = struct{}{}
	app.probeWG.Add(1)
	app.workerMu.Unlock()
	go func() {
		defer app.probeWG.Done()
		app.runProbeWorker(ctx, domain, requestQueue)
	}()
	return true
}

func (app *Application) runProbeWorker(ctx context.Context, domain string, requestQueue *queue.Queue) {
	breaker := app.breakerFor(domain)
	policy := app.domainPolicy(domain)
	selector := proxy.NewProbeSelector(proxy.ProbeStrategy(policy.ProbeStrategy))
	for {
		if requestQueue.Len() == 0 {
			if !waitForProbeWorker(ctx, probeWorkerIdleWait) {
				return
			}
			continue
		}
		snapshot := breaker.Snapshot()
		switch snapshot.State {
		case circuit.StateClosed:
			if !app.releaseQueued(ctx, breaker, requestQueue) {
				return
			}
		case circuit.StateOpen:
			if snapshot.RemainingCooldown > 0 {
				if !waitForProbeWorker(ctx, snapshot.RemainingCooldown) {
					return
				}
				continue
			}
			app.releaseProbe(ctx, breaker, requestQueue, selector)
			if !waitForProbeWorker(ctx, probeWorkerRetryWait) {
				return
			}
		case circuit.StateHalfOpen:
			if !snapshot.ProbeInFlight {
				app.releaseProbe(ctx, breaker, requestQueue, selector)
				if !waitForProbeWorker(ctx, probeWorkerRetryWait) {
					return
				}
				continue
			}
			if !waitForProbeWorker(ctx, probeWorkerRetryWait) {
				return
			}
		}
	}
}

func (app *Application) releaseProbe(ctx context.Context, breaker *circuit.Breaker, requestQueue *queue.Queue, selector *proxy.ProbeSelector) {
	probeContext, cancel := context.WithTimeout(ctx, probeWorkerTakeWait)
	defer cancel()
	_, err := requestQueue.TakeLightestGETWith(probeContext, func(item *queue.Item) error {
		if !breaker.CanExecute() {
			return errors.New("circuit probe is not ready")
		}
		if item.Request == nil {
			breaker.OnFailure()
			return errors.New("queued probe request is nil")
		}
		if err := selector.Apply(item.Request); err != nil {
			breaker.OnFailure()
			return err
		}
		return nil
	})
	if app.metrics != nil {
		app.metrics.SetQueueDepth(requestQueue.Len())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
}

func (app *Application) releaseQueued(ctx context.Context, breaker *circuit.Breaker, requestQueue *queue.Queue) bool {
	for requestQueue.Len() > 0 && breaker.State() == circuit.StateClosed {
		takeContext, cancel := context.WithTimeout(ctx, probeWorkerTakeWait)
		_, err := requestQueue.Take(takeContext)
		cancel()
		if errors.Is(err, context.Canceled) {
			return false
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, queue.ErrClosed) {
			return ctx.Err() == nil
		}
		if err != nil {
			return true
		}
		if app.metrics != nil {
			app.metrics.SetQueueDepth(requestQueue.Len())
		}
	}
	return ctx.Err() == nil
}

func waitForProbeWorker(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		wait = probeWorkerRetryWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (app *Application) domainPolicy(domain string) config.DomainDefaults {
	policy := app.cfg.Defaults
	bestRank := -1
	bestLength := -1
	for _, override := range app.cfg.Domains {
		pattern := canonicalDomain(override.Match)
		rank, length, ok := domainMatchRank(domain, pattern)
		if !ok || rank < bestRank || (rank == bestRank && length < bestLength) {
			continue
		}
		bestRank = rank
		bestLength = length
		if override.CooldownInitialSec > 0 {
			policy.CooldownInitialSec = override.CooldownInitialSec
		}
		if override.CooldownMultiplier > 0 {
			policy.CooldownMultiplier = override.CooldownMultiplier
		}
		if override.CooldownMaxSec > 0 {
			policy.CooldownMaxSec = override.CooldownMaxSec
		}
		if override.RequestsPerSec > 0 {
			policy.RequestsPerSec = override.RequestsPerSec
		}
		if override.JitterMinMS > 0 {
			policy.JitterMinMS = override.JitterMinMS
		}
		if override.JitterMaxMS > 0 {
			policy.JitterMaxMS = override.JitterMaxMS
		}
		if override.QueueMaxSize > 0 {
			policy.QueueMaxSize = override.QueueMaxSize
		}
		if override.QueueTimeoutSec > 0 {
			policy.QueueTimeoutSec = override.QueueTimeoutSec
		}
		if override.ProbeStrategy != "" {
			policy.ProbeStrategy = override.ProbeStrategy
		}
	}
	return policy
}

func (app *Application) authCookiesFor(domain string) []string {
	if app == nil || app.cfg == nil {
		return nil
	}
	domain = canonicalDomain(domain)
	bestRank := -1
	bestLength := -1
	var names []string
	for _, override := range app.cfg.Domains {
		pattern := canonicalDomain(override.Match)
		rank, length, ok := domainMatchRank(domain, pattern)
		if !ok || rank < bestRank || (rank == bestRank && length < bestLength) {
			continue
		}
		bestRank = rank
		bestLength = length
		if len(override.AuthCookies) == 0 {
			names = nil
			continue
		}
		names = append([]string(nil), override.AuthCookies...)
	}
	return names
}

func (app *Application) health(ctx context.Context) error {
	if app == nil || app.store == nil || app.store.DB() == nil {
		return errors.New("SQLite store is not ready")
	}
	return app.store.DB().PingContext(ctx)
}

func canonicalDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func domainMatchRank(domain, pattern string) (int, int, bool) {
	if domain == "" || pattern == "" {
		return 0, 0, false
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*.")
		if domain == suffix || !strings.HasSuffix(domain, "."+suffix) {
			return 0, 0, false
		}
		return 1, len(suffix), true
	}
	if domain == pattern {
		return 2, len(pattern), true
	}
	return 0, 0, false
}

func loadConfig(path string) (*config.Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return config.NewDefaultConfig(), nil
	}
	return config.LoadFile(path)
}

func main() {
	configPath := flag.String("config", "", "path to an optional YAML configuration file")
	flag.Parse()
	path := strings.TrimSpace(*configPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("RUNNEL_CONFIG"))
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatal(err)
	}
	app, err := NewApplication(cfg)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := app.Listen()
	if err != nil {
		_ = app.Shutdown(context.Background())
		log.Fatal(err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- app.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serveErrors:
		if err != nil {
			log.Print(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		_ = app.Shutdown(ctx)
		cancel()
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			log.Print(err)
		}
		if err := <-serveErrors; err != nil {
			log.Print(err)
		}
	}
}
