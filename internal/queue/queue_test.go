package queue

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestQueueCancellationRemovesEntry(t *testing.T) {
	q := New(4, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	ticket, err := q.Add(ctx, Item{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	waitForQueueLen(t, q, 0)
	if err := q.Wait(ctx, ticket); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context canceled", err)
	}
}

func TestQueueTimeoutExposesRemainingRetryAfter(t *testing.T) {
	q := New(1, 70*time.Millisecond)
	ticket, err := q.Add(context.Background(), Item{
		Method:     http.MethodGet,
		RetryAfter: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = q.Wait(context.Background(), ticket)
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Wait error = %v, want TimeoutError", err)
	}
	if timeoutErr.RemainingRetryAfter < 100*time.Millisecond || timeoutErr.RemainingRetryAfter > 250*time.Millisecond {
		t.Fatalf("remaining Retry-After = %s, want a positive duration near 180ms", timeoutErr.RemainingRetryAfter)
	}
	if timeoutErr.RetryAfterSeconds() < 1 {
		t.Fatalf("Retry-After seconds = %d, want at least 1", timeoutErr.RetryAfterSeconds())
	}
	if q.Len() != 0 {
		t.Fatalf("queue length after timeout = %d, want 0", q.Len())
	}
}

func TestQueueTakeLightestGETLeavesWritesQueued(t *testing.T) {
	q := New(4, time.Second)
	writeContext, cancelWrite := context.WithCancel(context.Background())
	defer cancelWrite()
	write, err := q.Add(writeContext, Item{Method: http.MethodPost, Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	if _, err := q.Add(firstContext, Item{Method: http.MethodGet, Weight: 10}); err != nil {
		t.Fatal(err)
	}
	secondContext, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	if _, err := q.Add(secondContext, Item{Method: http.MethodGet, Weight: 2}); err != nil {
		t.Fatal(err)
	}

	selected, err := q.TakeLightestGET(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if selected.Item.Weight != 2 || selected.Item.Method != http.MethodGet {
		t.Fatalf("selected item = %+v, want lightest GET", selected.Item)
	}
	if q.Len() != 2 {
		t.Fatalf("queue length after selection = %d, want 2", q.Len())
	}
	if removed := q.Remove(write); !removed {
		t.Fatal("queued write was not removed")
	}
	if err := q.Wait(context.Background(), write); !errors.Is(err, ErrRemoved) {
		t.Fatalf("removed write result = %v, want ErrRemoved", err)
	}
}

func TestQueueCanceledWaitersDoNotAccumulate(t *testing.T) {
	q := New(200, time.Second)
	baseline := runtime.NumGoroutine()
	const waiters = 100
	var wg sync.WaitGroup
	wg.Add(waiters)
	for i := 0; i < waiters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func(ctx context.Context, cancel context.CancelFunc) {
			defer wg.Done()
			ticket, err := q.Add(ctx, Item{Method: http.MethodGet})
			if err != nil {
				cancel()
				return
			}
			cancel()
			if err := q.Wait(ctx, ticket); !errors.Is(err, context.Canceled) {
				t.Errorf("Wait error = %v, want context canceled", err)
			}
		}(ctx, cancel)
	}
	wg.Wait()
	waitForQueueLen(t, q, 0)

	deadline := time.Now().Add(500 * time.Millisecond)
	for runtime.NumGoroutine() > baseline+10 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+10 {
		t.Fatalf("goroutines after canceled waiters = %d, baseline %d", got, baseline)
	}
}

func waitForQueueLen(t *testing.T, q *Queue, want int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for q.Len() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := q.Len(); got != want {
		t.Fatalf("queue length = %d, want %d", got, want)
	}
}
