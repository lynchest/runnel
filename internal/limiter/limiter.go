package limiter

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Common limiter errors.
var (
	ErrBurstExceeded = errors.New("requested tokens exceed limiter burst capacity")
)

// Limiter implements a concurrency-safe token bucket rate limiter supporting
// floating requests_per_sec, burst capacity, context-aware token acquisition,
// anti-bot jitter delays, and injectable clock/timer seams.
type Limiter struct {
	mu         sync.Mutex
	clock      Clock
	rate       float64 // tokens per second
	burst      float64 // maximum bucket capacity
	tokens     float64 // current available tokens
	lastRefill time.Time
	jitter     JitterSource
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock injects a custom Clock for deterministic time/sleep control.
func WithClock(c Clock) Option {
	return func(l *Limiter) {
		if c != nil {
			l.clock = c
		}
	}
}

// WithJitter injects a custom JitterSource for anti-bot delays.
func WithJitter(j JitterSource) Option {
	return func(l *Limiter) {
		l.jitter = j
	}
}

// New creates a new Limiter with the given rate (requests_per_sec) and burst.
// If rate <= 0, tokens are not refilled over time.
// If burst < 1, burst is defaulted to 1.
// The bucket starts full (tokens = burst).
func New(requestsPerSec float64, burst int, opts ...Option) *Limiter {
	burstCap := float64(burst)
	if burstCap < 1 {
		burstCap = 1
	}
	if requestsPerSec < 0 {
		requestsPerSec = 0
	}

	l := &Limiter{
		clock:  RealClock{},
		rate:   requestsPerSec,
		burst:  burstCap,
		tokens: burstCap,
	}

	for _, opt := range opts {
		opt(l)
	}

	l.lastRefill = l.clock.Now()
	return l
}

// Allow reports whether 1 token may be consumed immediately without blocking or jitter.
func (l *Limiter) Allow() bool {
	return l.AllowN(l.clock.Now(), 1)
}

// AllowN reports whether n tokens may be consumed at time now without blocking or jitter.
func (l *Limiter) AllowN(now time.Time, n int) bool {
	if n <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.refillLocked(now)

	req := float64(n)
	if req > l.burst {
		return false
	}

	if l.tokens >= req {
		l.tokens -= req
		return true
	}
	return false
}

// Acquire blocks until 1 token is available and context is satisfied, plus applying any anti-bot jitter.
func (l *Limiter) Acquire(ctx context.Context) error {
	return l.AcquireN(ctx, 1)
}

// AcquireN blocks until n tokens are available and context is satisfied, plus applying any anti-bot jitter.
func (l *Limiter) AcquireN(ctx context.Context, n int) error {
	if err := l.WaitN(ctx, n); err != nil {
		return err
	}

	// Apply anti-bot jitter if configured.
	if l.jitter != nil {
		jDur := l.jitter.Jitter()
		if jDur > 0 {
			if err := l.clock.Sleep(ctx, jDur); err != nil {
				return err
			}
		}
	}

	return nil
}

// Wait blocks until 1 token is available without applying jitter.
func (l *Limiter) Wait(ctx context.Context) error {
	return l.WaitN(ctx, 1)
}

// WaitN blocks until n tokens are available according to the token bucket schedule.
func (l *Limiter) WaitN(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}

	req := float64(n)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		waitDur, err := l.reserveOrWaitDuration(req)
		if err != nil {
			return err
		}

		if waitDur <= 0 {
			// Acquired successfully.
			return nil
		}

		if err := l.clock.Sleep(ctx, waitDur); err != nil {
			return err
		}
	}
}

// reserveOrWaitDuration checks if tokens are available now. If so, consumes them and returns 0.
// Otherwise, returns the duration to sleep until tokens can refill, without consuming them yet.
func (l *Limiter) reserveOrWaitDuration(n float64) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if n > l.burst {
		return 0, ErrBurstExceeded
	}

	now := l.clock.Now()
	l.refillLocked(now)

	if l.tokens >= n {
		l.tokens -= n
		return 0, nil
	}

	if l.rate <= 0 {
		return 0, errors.New("limiter rate is zero; tokens will never refill")
	}

	missing := n - l.tokens
	neededSec := missing / l.rate
	waitDur := time.Duration(neededSec * float64(time.Second))
	if waitDur <= 0 {
		waitDur = time.Nanosecond
	}

	return waitDur, nil
}

// Tokens returns the current token count after refilling up to now.
func (l *Limiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked(l.clock.Now())
	return l.tokens
}

// Refill fills tokens according to time elapsed since last refill up to burst limit.
func (l *Limiter) refillLocked(now time.Time) {
	if l.lastRefill.IsZero() {
		l.lastRefill = now
		return
	}

	elapsed := now.Sub(l.lastRefill)
	if elapsed <= 0 {
		return
	}

	l.lastRefill = now
	if l.rate > 0 {
		l.tokens += elapsed.Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
	}
}
