package limiter_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/limiter"
)

type fixedJitter struct {
	val time.Duration
}

func (f fixedJitter) Jitter() time.Duration {
	return f.val
}

func TestLimiterAllowBurst(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := limiter.NewMockClock(start)

	// Rate 2 req/sec, burst 3
	l := limiter.New(2.0, 3, limiter.WithClock(clock))

	// Bucket starts full with burst = 3
	if !l.Allow() {
		t.Fatal("expected 1st allow to succeed")
	}
	if !l.Allow() {
		t.Fatal("expected 2nd allow to succeed")
	}
	if !l.Allow() {
		t.Fatal("expected 3rd allow to succeed")
	}
	if l.Allow() {
		t.Fatal("expected 4th allow to fail due to exhausted burst")
	}

	// Advance clock by 500ms -> 0.5s * 2 tokens/sec = 1 token refilled
	clock.Advance(500 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("expected allow to succeed after 500ms refill")
	}
	if l.Allow() {
		t.Fatal("expected allow to fail after consuming newly refilled token")
	}

	// Advance clock by 5 seconds -> refills up to burst cap (3)
	clock.Advance(5 * time.Second)
	if tokens := l.Tokens(); tokens != 3.0 {
		t.Fatalf("expected tokens capped at burst 3.0, got %v", tokens)
	}
}

func TestLimiterWaitDeterministic(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := limiter.NewMockClock(start)

	// Rate 1 req/sec, burst 1
	l := limiter.New(1.0, 1, limiter.WithClock(clock))

	ctx := context.Background()

	// 1st Wait should consume initial token immediately
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("unexpected error on 1st wait: %v", err)
	}

	// 2nd Wait requires 1 second of time. Run in goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- l.Wait(ctx)
	}()

	// Wait until goroutine is blocked sleeping
	for i := 0; i < 50; i++ {
		if clock.WaiterCount() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if clock.WaiterCount() == 0 {
		t.Fatal("expected sleeper to be registered in mock clock")
	}

	// Advance clock by 1 second to satisfy wait
	clock.Advance(1 * time.Second)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error on 2nd wait: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waiter did not complete after clock advance")
	}
}

func TestLimiterWaitContextCancellation(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := limiter.NewMockClock(start)

	l := limiter.New(1.0, 1, limiter.WithClock(clock))

	ctx, cancel := context.WithCancel(context.Background())

	// Consume initial token
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- l.Wait(ctx)
	}()

	for i := 0; i < 50; i++ {
		if clock.WaiterCount() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("wait did not exit after context cancel")
	}

	if count := clock.WaiterCount(); count != 0 {
		t.Fatalf("expected 0 remaining waiters after cancel, got %d", count)
	}
}

func TestLimiterAcquireWithJitter(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := limiter.NewMockClock(start)

	jit := fixedJitter{val: 250 * time.Millisecond}
	l := limiter.New(1.0, 1, limiter.WithClock(clock), limiter.WithJitter(jit))

	ctx := context.Background()
	errCh := make(chan error, 1)

	// Acquire consumes token immediately, then sleeps for jitter (250ms)
	go func() {
		errCh <- l.Acquire(ctx)
	}()

	for i := 0; i < 50; i++ {
		if clock.WaiterCount() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if clock.WaiterCount() != 1 {
		t.Fatal("expected jitter sleeper in mock clock")
	}

	clock.Advance(250 * time.Millisecond)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error in acquire: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("acquire did not complete after advancing jitter duration")
	}
}

func TestLimiterBurstExceeded(t *testing.T) {
	l := limiter.New(10.0, 2)
	err := l.WaitN(context.Background(), 5)
	if err != limiter.ErrBurstExceeded {
		t.Fatalf("expected ErrBurstExceeded, got %v", err)
	}
}

func TestLimiterConcurrency(t *testing.T) {
	// Use real clock with zero jitter for high-throughput concurrency test
	l := limiter.New(1000.0, 50)

	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = l.Allow()
			}
		}()
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = l.Wait(ctx)
			}
		}()
	}

	wg.Wait()
}
