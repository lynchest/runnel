package circuit

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now} }

func (c *testClock) Now() time.Time {
	c.mu.RLock()
	now := c.now
	c.mu.RUnlock()
	return now
}

func (c *testClock) Advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}

func TestBreakerStateTransitionsAndExponentialFallback(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	breaker := NewBreaker(Config{
		InitialCooldown:    2 * time.Second,
		CooldownMultiplier: 2,
		MaxCooldown:        5 * time.Second,
	}, clock)

	if got := breaker.State(); got != StateClosed {
		t.Fatalf("new breaker state = %s, want %s", got, StateClosed)
	}
	if !breaker.CanExecute() {
		t.Fatal("closed breaker denied a request")
	}

	breaker.OnFailure()
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("after failure state = %s, want %s", got, StateOpen)
	}
	if got := breaker.Snapshot().RemainingCooldown; got != 2*time.Second {
		t.Fatalf("first fallback cooldown = %s, want 2s", got)
	}
	if breaker.CanExecute() {
		t.Fatal("open breaker allowed a request before cooldown")
	}

	clock.Advance(2 * time.Second)
	if breaker.State() != StateOpen {
		t.Fatal("observing state consumed the HALF-OPEN permit")
	}
	if !breaker.CanExecute() || breaker.CanExecute() {
		t.Fatal("HALF-OPEN did not enforce one probe permit")
	}

	breaker.OnFailure()
	if got := breaker.Snapshot().ConsecutiveFailures; got != 2 {
		t.Fatalf("failed probe count = %d, want 2", got)
	}
	if got := breaker.Snapshot().RemainingCooldown; got != 4*time.Second {
		t.Fatalf("second fallback cooldown = %s, want 4s", got)
	}

	clock.Advance(4 * time.Second)
	if !breaker.CanExecute() || breaker.CanExecute() {
		t.Fatal("second HALF-OPEN window did not enforce one permit")
	}
	breaker.OnFailure()
	if got := breaker.Snapshot().RemainingCooldown; got != 5*time.Second {
		t.Fatalf("fallback cooldown was not capped: got %s, want 5s", got)
	}

	clock.Advance(5 * time.Second)
	if !breaker.CanExecute() {
		t.Fatal("breaker denied the probe after capped cooldown")
	}
	breaker.OnSuccess()
	if got := breaker.State(); got != StateClosed {
		t.Fatalf("successful probe state = %s, want %s", got, StateClosed)
	}
	if got := breaker.Snapshot().ConsecutiveFailures; got != 0 {
		t.Fatalf("successful probe failure count = %d, want 0", got)
	}
}

func TestBreakerConcurrentRateLimitsCountOneTrip(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	breaker := NewBreaker(Config{
		InitialCooldown:    30 * time.Second,
		CooldownMultiplier: 2,
		MaxCooldown:        2 * time.Minute,
	}, clock)

	const callers = 50
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			breaker.OnRateLimit(10)
		}()
	}
	wg.Wait()

	snapshot := breaker.Snapshot()
	if snapshot.State != StateOpen {
		t.Fatalf("concurrent rate limits state = %s, want %s", snapshot.State, StateOpen)
	}
	if snapshot.ConsecutiveFailures != 1 {
		t.Fatalf("concurrent rate limits counted %d trips, want 1", snapshot.ConsecutiveFailures)
	}
	if got, want := snapshot.BlockedUntil, clock.Now().Add(10*time.Second); !got.Equal(want) {
		t.Fatalf("blocked deadline = %s, want %s", got, want)
	}
}

func TestBreakerRateLimitHeaderUsesParsedDuration(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	breaker := NewBreaker(Config{
		InitialCooldown:    2 * time.Second,
		CooldownMultiplier: 2,
		MaxCooldown:        10 * time.Second,
	}, clock)

	breaker.OnRateLimitHeader(clock.Now().Add(7 * time.Second).Format(http.TimeFormat))
	if got := breaker.Snapshot().RemainingCooldown; got != 7*time.Second {
		t.Fatalf("Retry-After header cooldown = %s, want 7s", got)
	}
}

func TestBreakerClampsSuppliedRetryAfterToMaxCooldown(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	breaker := NewBreaker(Config{InitialCooldown: time.Second, MaxCooldown: 10 * time.Second}, clock)
	breaker.OnRateLimitHeader(clock.Now().Add(24 * time.Hour).Format(http.TimeFormat))
	if got := breaker.Snapshot().RemainingCooldown; got != 10*time.Second {
		t.Fatalf("supplied cooldown = %s, want 10s", got)
	}
}

func TestBreakerExactlyOneHalfOpenPermitAmongContenders(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	breaker := NewBreaker(Config{InitialCooldown: time.Second, MaxCooldown: time.Second}, clock)
	breaker.OnRateLimit(1)
	clock.Advance(time.Second)

	const contenders = 20
	permits := make(chan bool, contenders)
	var wg sync.WaitGroup
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			defer wg.Done()
			permits <- breaker.CanExecute()
		}()
	}
	wg.Wait()
	close(permits)

	allowed := 0
	for permit := range permits {
		if permit {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("HALF-OPEN permits among %d contenders = %d, want exactly 1", contenders, allowed)
	}
}
