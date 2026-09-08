package circuit

import (
	"math"
	"sync"
	"time"
)

// State is the current state of a circuit breaker.
type State string

const (
	StateClosed   State = "CLOSED"
	StateOpen     State = "OPEN"
	StateHalfOpen State = "HALF-OPEN"
)

// Clock is the time source used for cooldowns. Tests can provide a fake
// implementation; a nil Clock uses the wall clock.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Config controls the exponential fallback cooldown used when an upstream
// response does not provide a usable Retry-After value.
type Config struct {
	InitialCooldown    time.Duration
	CooldownMultiplier float64
	MaxCooldown        time.Duration
}

const (
	defaultInitialCooldown = 5 * time.Minute
	defaultMultiplier      = 2
	defaultMaxCooldown     = 2 * time.Hour
)

func normalizeConfig(config Config) Config {
	if config.InitialCooldown <= 0 {
		config.InitialCooldown = defaultInitialCooldown
	}
	if config.CooldownMultiplier <= 0 || math.IsNaN(config.CooldownMultiplier) || math.IsInf(config.CooldownMultiplier, 0) {
		config.CooldownMultiplier = defaultMultiplier
	}
	if config.MaxCooldown <= 0 {
		config.MaxCooldown = defaultMaxCooldown
	}
	if config.MaxCooldown < config.InitialCooldown {
		config.MaxCooldown = config.InitialCooldown
	}
	return config
}

// Breaker is a concurrency-safe CLOSED/OPEN/HALF-OPEN state machine.
// CanExecute performs a non-blocking admission check. When an OPEN cooldown
// expires, the first caller atomically claims the one HALF-OPEN probe permit.
type Breaker struct {
	mu sync.Mutex

	state         State
	blockedUntil  time.Time
	failureCount  int
	probeInFlight bool

	config Config
	clock  Clock
}

// NewBreaker creates a breaker with the supplied policy and clock. Passing a
// nil clock selects time.Now.
func NewBreaker(config Config, clock Clock) *Breaker {
	if clock == nil {
		clock = wallClock{}
	}
	return &Breaker{
		state:  StateClosed,
		config: normalizeConfig(config),
		clock:  clock,
	}
}

// CanExecute reports whether a request may proceed. It never waits. The
// OPEN-to-HALF-OPEN transition and the probe claim happen under one lock, so
// concurrent callers cannot obtain more than one HALF-OPEN permit.
func (b *Breaker) CanExecute() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if now.Before(b.blockedUntil) {
			return false
		}
		b.state = StateHalfOpen
		b.probeInFlight = true
		return true
	case StateHalfOpen:
		if b.probeInFlight {
			return false
		}
		// This branch is conservative for a manually observed HALF-OPEN
		// state and still grants at most one permit.
		b.probeInFlight = true
		return true
	default:
		return false
	}
}

// OnSuccess reports a successful upstream response. A successful probe closes
// the circuit and clears the consecutive failure sequence.
func (b *Breaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.failureCount = 0
	case StateHalfOpen:
		b.state = StateClosed
		b.blockedUntil = time.Time{}
		b.failureCount = 0
		b.probeInFlight = false
	}
}

// OnFailure opens the circuit using the next exponential fallback cooldown.
func (b *Breaker) OnFailure() { b.trip(0, false) }

// OnRateLimit opens or extends the circuit using Retry-After delta-seconds.
// A negative value means that no usable value was available and selects the
// exponential fallback. Zero is a valid immediate Retry-After value.
func (b *Breaker) OnRateLimit(retryAfterSec int) {
	if retryAfterSec < 0 {
		b.OnFailure()
		return
	}
	delay, ok := secondsDuration(int64(retryAfterSec))
	if !ok {
		b.OnFailure()
		return
	}
	b.trip(delay, true)
}

// OnRateLimitHeader parses a Retry-After header using the breaker clock. An
// invalid header falls back to the configured exponential cooldown.
func (b *Breaker) OnRateLimitHeader(value string) {
	now := b.clock.Now()
	delay, err := ParseRetryAfter(value, now)
	if err != nil {
		b.OnFailure()
		return
	}
	b.trip(delay, true)
}

// State returns the current state. State changes to HALF-OPEN only when
// CanExecute claims its probe; observing an expired OPEN cooldown therefore
// does not consume the permit.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Snapshot is an atomically captured read-only view of a breaker.
type Snapshot struct {
	State               State
	BlockedUntil        time.Time
	ConsecutiveFailures int
	RemainingCooldown   time.Duration
	ProbeInFlight       bool
}

// Snapshot returns state and cooldown information at one instant.
func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	remaining := time.Duration(0)
	if b.state == StateOpen && b.blockedUntil.After(now) {
		remaining = b.blockedUntil.Sub(now)
	}
	return Snapshot{
		State:               b.state,
		BlockedUntil:        b.blockedUntil,
		ConsecutiveFailures: b.failureCount,
		RemainingCooldown:   remaining,
		ProbeInFlight:       b.probeInFlight,
	}
}

// Reset returns the breaker to CLOSED and clears its failure history. It is
// intended for an explicitly authorized administrative operation.
func (b *Breaker) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.state = StateClosed
	b.blockedUntil = time.Time{}
	b.failureCount = 0
	b.probeInFlight = false
	b.mu.Unlock()
}

func (b *Breaker) trip(delay time.Duration, supplied bool) {
	now := b.clock.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	// An expired OPEN circuit is logically a failed probe when a new failure
	// arrives before another caller claims the permit.
	if b.state == StateOpen && !now.Before(b.blockedUntil) {
		b.state = StateHalfOpen
		b.probeInFlight = true
	}

	switch b.state {
	case StateOpen:
		candidate := b.deadline(now, delay, supplied, b.failureCount)
		if candidate.After(b.blockedUntil) {
			b.blockedUntil = candidate
		}
	case StateClosed, StateHalfOpen:
		b.failureCount++
		b.state = StateOpen
		b.probeInFlight = false
		b.blockedUntil = b.deadline(now, delay, supplied, b.failureCount)
	}
}

func (b *Breaker) deadline(now time.Time, delay time.Duration, supplied bool, failure int) time.Time {
	if !supplied {
		delay = b.fallback(failure)
	}
	return now.Add(delay)
}

func (b *Breaker) fallback(failure int) time.Duration {
	if failure < 1 {
		failure = 1
	}
	delay := b.config.InitialCooldown
	for i := 1; i < failure; i++ {
		if delay >= b.config.MaxCooldown {
			return b.config.MaxCooldown
		}
		next := float64(delay) * b.config.CooldownMultiplier
		if math.IsNaN(next) || math.IsInf(next, 0) || next >= float64(b.config.MaxCooldown) {
			return b.config.MaxCooldown
		}
		if next < float64(time.Nanosecond) {
			return time.Nanosecond
		}
		delay = time.Duration(next)
	}
	return delay
}

func secondsDuration(seconds int64) (time.Duration, bool) {
	if seconds < 0 || seconds > int64(math.MaxInt64/int64(time.Second)) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
