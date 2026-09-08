package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// CleanerOptions controls periodic cleanup and quota eviction. A zero interval
// selects time.Hour; a zero MaxBytes disables quota enforcement.
type CleanerOptions struct {
	Interval          time.Duration
	MaxBytes          int64
	EvictionBatchSize int
	Logger            *log.Logger
}

// CleanerStats describes one RunOnce pass.
type CleanerStats struct {
	ExpiredDeleted int64
	Evicted        int64
	DatabaseBytes  int64
	Checkpointed   bool
	At             time.Time
}

// Cleaner removes expired rows, checkpoints the WAL passively, and evicts up
// to 500 oldest rows when the configured page quota is exceeded.
type Cleaner struct {
	repo *CacheRepository
	opts CleanerOptions

	stop chan struct{}
	done chan struct{}

	stateMu sync.Mutex
	started bool
	closed  bool

	lastErrMu sync.RWMutex
	lastErr   error
	passes    atomic.Uint64
	panics    atomic.Uint64
}

// NewCleaner constructs a stopped cleaner. Start is explicit so a caller can
// assemble all workers before launching any background goroutine.
func NewCleaner(repo *CacheRepository, options CleanerOptions) *Cleaner {
	if options.Interval <= 0 {
		options.Interval = time.Hour
	}
	if options.EvictionBatchSize <= 0 {
		options.EvictionBatchSize = DefaultEvictionBatchSize
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}
	return &Cleaner{
		repo: repo,
		opts: options,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// RunOnce performs one expiry/checkpoint/quota pass at now.
func (cleaner *Cleaner) RunOnce(ctx context.Context, now time.Time) (CleanerStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	stats := CleanerStats{At: now}
	if cleaner == nil || cleaner.repo == nil {
		return stats, errors.New("cache cleaner repository is nil")
	}
	deleted, err := cleaner.repo.DeleteExpired(ctx, now)
	stats.ExpiredDeleted = deleted
	if err != nil {
		cleaner.setLastError(err)
		return stats, err
	}
	if err := cleaner.repo.Checkpoint(ctx); err != nil {
		cleaner.setLastError(err)
		return stats, err
	}
	stats.Checkpointed = true

	if cleaner.opts.MaxBytes > 0 {
		size, err := cleaner.repo.DatabaseSize(ctx)
		if err != nil {
			cleaner.setLastError(err)
			return stats, err
		}
		stats.DatabaseBytes = size
		if size > cleaner.opts.MaxBytes {
			removed, err := cleaner.repo.EvictOldest(ctx, cleaner.opts.EvictionBatchSize)
			stats.Evicted = removed
			if err != nil {
				cleaner.setLastError(err)
				return stats, err
			}
			stats.DatabaseBytes, err = cleaner.repo.DatabaseSize(ctx)
			if err != nil {
				cleaner.setLastError(err)
				return stats, err
			}
			if removed > 0 {
				if err := cleaner.repo.Checkpoint(ctx); err != nil {
					cleaner.setLastError(err)
					return stats, err
				}
			}
		}
	}
	cleaner.passes.Add(1)
	return stats, nil
}

// Start launches periodic cleanup with an immediate first pass.
func (cleaner *Cleaner) Start(ctx context.Context) {
	if cleaner == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleaner.stateMu.Lock()
	if cleaner.started || cleaner.closed {
		cleaner.stateMu.Unlock()
		return
	}
	cleaner.started = true
	cleaner.stateMu.Unlock()
	go cleaner.run(ctx)
}

func (cleaner *Cleaner) run(ctx context.Context) {
	defer close(cleaner.done)
	defer func() {
		if recovered := recover(); recovered != nil {
			cleaner.panics.Add(1)
			cleaner.setLastError(fmt.Errorf("cache cleaner panic recovered: %v", recovered))
			cleaner.opts.Logger.Printf("[PANIC RECOVERED] cache cleaner: %v\nstack:\n%s", recovered, debug.Stack())
		}
	}()
	_, _ = cleaner.RunOnce(ctx, time.Now())
	ticker := time.NewTicker(cleaner.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, _ = cleaner.RunOnce(ctx, time.Now())
		case <-ctx.Done():
			return
		case <-cleaner.stop:
			return
		}
	}
}

// Shutdown stops a started cleaner and waits for its goroutine.
func (cleaner *Cleaner) Shutdown(ctx context.Context) error {
	if cleaner == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleaner.stateMu.Lock()
	if !cleaner.closed {
		cleaner.closed = true
		close(cleaner.stop)
	}
	started := cleaner.started
	cleaner.stateMu.Unlock()
	if !started {
		return cleaner.lastError()
	}
	select {
	case <-cleaner.done:
		return cleaner.lastError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done closes when a started cleaner exits.
func (cleaner *Cleaner) Done() <-chan struct{} {
	if cleaner == nil {
		channel := make(chan struct{})
		close(channel)
		return channel
	}
	cleaner.stateMu.Lock()
	started := cleaner.started
	cleaner.stateMu.Unlock()
	if !started {
		channel := make(chan struct{})
		close(channel)
		return channel
	}
	return cleaner.done
}

func (cleaner *Cleaner) lastError() error {
	cleaner.lastErrMu.RLock()
	defer cleaner.lastErrMu.RUnlock()
	return cleaner.lastErr
}

func (cleaner *Cleaner) setLastError(err error) {
	if err == nil {
		return
	}
	cleaner.lastErrMu.Lock()
	cleaner.lastErr = err
	cleaner.lastErrMu.Unlock()
}
