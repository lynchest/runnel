package limiter

import (
	"context"
	"sync"
	"time"
)

// Clock provides the current time and sleep mechanism.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock implements Clock using the standard time package.
type RealClock struct{}

// Now returns the current local time.
func (RealClock) Now() time.Time {
	return time.Now()
}

// Sleep blocks until duration d elapses or ctx is canceled.
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MockClock is a controllable, concurrency-safe Clock for deterministic testing.
type MockClock struct {
	mu      sync.Mutex
	current time.Time
	waiters []*mockWaiter
}

type mockWaiter struct {
	target time.Time
	ch     chan struct{}
}

// NewMockClock creates a MockClock initialized to start time.
func NewMockClock(start time.Time) *MockClock {
	return &MockClock{
		current: start,
	}
}

// Now returns the current simulated time.
func (m *MockClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Advance moves simulated time forward by d and wakes any expired waiters.
func (m *MockClock) Advance(d time.Duration) {
	m.mu.Lock()
	m.current = m.current.Add(d)
	now := m.current

	var remaining []*mockWaiter
	for _, w := range m.waiters {
		if !w.target.After(now) {
			close(w.ch)
		} else {
			remaining = append(remaining, w)
		}
	}
	m.waiters = remaining
	m.mu.Unlock()
}

// WaiterCount returns the number of currently blocked sleepers in the mock clock.
func (m *MockClock) WaiterCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waiters)
}

// Sleep blocks until simulated time reaches Now() + d or ctx is canceled.
func (m *MockClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	m.mu.Lock()
	target := m.current.Add(d)
	ch := make(chan struct{})
	w := &mockWaiter{target: target, ch: ch}
	m.waiters = append(m.waiters, w)
	m.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		for i, item := range m.waiters {
			if item == w {
				m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		return ctx.Err()
	}
}
