// Package queue provides a bounded, context-aware queue for requests parked
// while an upstream circuit is unavailable.
package queue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	// ErrFull is returned when the queue has reached its configured capacity.
	ErrFull = errors.New("request queue is full")
	// ErrClosed is returned when a queue is closed before a waiter is admitted.
	ErrClosed = errors.New("request queue is closed")
	// ErrRemoved is returned when a queued item is explicitly removed.
	ErrRemoved = errors.New("request was removed from the queue")
	// ErrTimeout is wrapped by TimeoutError when a queued item reaches its
	// queue deadline.
	ErrTimeout = errors.New("request queue wait timed out")
)

// Item is the small amount of request metadata needed by the queue and probe
// selector. Request is optional for callers that only need to park metadata.
// Weight is an application-provided estimate used to choose a light probe;
// lower values are preferred. RetryAfter is the circuit's remaining cooldown
// at enqueue time and is reported, minus time spent waiting, on timeout.
type Item struct {
	ID         uint64
	Request    *http.Request
	Method     string
	Weight     int64
	RetryAfter time.Duration
}

// NewItem builds an Item from an HTTP request. A nil request is accepted so a
// caller can fill metadata independently; its method then defaults to GET.
func NewItem(req *http.Request, weight int64) Item {
	item := Item{Request: req, Weight: weight}
	if req == nil {
		item.Method = http.MethodGet
		return item
	}
	item.Method = req.Method
	if item.Method == "" {
		item.Method = http.MethodGet
	}
	return item
}

// TimeoutError identifies a queue timeout and preserves the remaining
// Retry-After duration for an HTTP response. RemainingRetryAfter is never
// negative.
type TimeoutError struct {
	Waited              time.Duration
	RemainingRetryAfter time.Duration
}

func (e *TimeoutError) Error() string {
	if e == nil {
		return ErrTimeout.Error()
	}
	return fmt.Sprintf("%v after %s (Retry-After remaining %s)", ErrTimeout, e.Waited, e.RemainingRetryAfter)
}

func (e *TimeoutError) Unwrap() error { return ErrTimeout }

// RetryAfterSeconds returns a header-safe, rounded-up number of seconds.
func (e *TimeoutError) RetryAfterSeconds() int64 {
	if e == nil || e.RemainingRetryAfter <= 0 {
		return 0
	}
	return int64((e.RemainingRetryAfter + time.Second - 1) / time.Second)
}

// RetryAfterHeader returns the value suitable for an HTTP Retry-After header.
func (e *TimeoutError) RetryAfterHeader() string {
	return fmt.Sprintf("%d", e.RetryAfterSeconds())
}

// Ticket identifies one queued item. A ticket is returned by Add and is
// useful when a caller needs to keep enqueueing separate from waiting. The
// Item's ID is stable for the lifetime of the ticket.
type Ticket struct {
	Item Item

	finished chan struct{}
	mu       sync.Mutex
	err      error
}

func (t *Ticket) result() error {
	if t == nil {
		return ErrRemoved
	}
	t.mu.Lock()
	err := t.err
	t.mu.Unlock()
	return err
}

func (t *Ticket) finish(err error) {
	t.mu.Lock()
	if t.finished == nil {
		t.finished = make(chan struct{})
	}
	if isFinished(t.finished) {
		t.mu.Unlock()
		return
	}
	t.err = err
	close(t.finished)
	t.mu.Unlock()
}

func isFinished(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type entry struct {
	ticket   *Ticket
	ctx      context.Context
	enqueued time.Time
	deadline time.Time
}

// Queue is a concurrency-safe bounded queue. Add never blocks on capacity;
// Wait blocks only for a caller's own ticket and can return immediately when
// that caller's context is canceled. Enqueue is the convenience form that
// performs both operations.
type Queue struct {
	mu      sync.Mutex
	maxSize int
	timeout time.Duration
	nextID  uint64
	closed  bool
	entries []*entry
	changed chan struct{}
}

// New creates a queue with maxSize entries and a per-entry wait timeout. A
// non-positive maxSize disables admission; a non-positive timeout makes
// entries expire on the next scheduler turn.
func New(maxSize int, timeout time.Duration) *Queue {
	if maxSize < 0 {
		maxSize = 0
	}
	return &Queue{
		maxSize: maxSize,
		timeout: timeout,
		changed: make(chan struct{}),
	}
}

// Add appends an item and returns its ticket. Context cancellation is watched
// even when the caller delays Wait, so canceled clients do not remain in the
// queue. The watcher exits when the item is admitted, canceled, removed, or
// expires.
func (q *Queue) Add(ctx context.Context, item Item) (*Ticket, error) {
	if q == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if item.Method == "" {
		if item.Request != nil && item.Request.Method != "" {
			item.Method = item.Request.Method
		} else {
			item.Method = http.MethodGet
		}
	}

	q.mu.Lock()
	q.pruneLocked(time.Now())
	if q.closed {
		q.mu.Unlock()
		return nil, ErrClosed
	}
	if q.maxSize == 0 || len(q.entries) >= q.maxSize {
		q.mu.Unlock()
		return nil, ErrFull
	}
	q.nextID++
	item.ID = q.nextID
	now := time.Now()
	ticket := &Ticket{Item: item, finished: make(chan struct{})}
	e := &entry{ticket: ticket, ctx: ctx, enqueued: now}
	if q.timeout > 0 {
		e.deadline = now.Add(q.timeout)
	} else {
		e.deadline = now
	}
	q.entries = append(q.entries, e)
	q.signalLocked()
	q.mu.Unlock()

	go q.watch(e)
	return ticket, nil
}

// Enqueue appends an item and waits until it is admitted or the caller's
// context/queue deadline wins. It is the usual handler-facing operation.
func (q *Queue) Enqueue(ctx context.Context, item Item) error {
	ticket, err := q.Add(ctx, item)
	if err != nil {
		return err
	}
	return q.Wait(ctx, ticket)
}

// Wait waits for the ticket to be admitted. The ticket is removed before a
// context cancellation or timeout is returned.
func (q *Queue) Wait(ctx context.Context, ticket *Ticket) error {
	if ticket == nil {
		return ErrRemoved
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ticket.finished:
		return ticket.result()
	case <-ctx.Done():
		q.remove(ticket.Item.ID, ctx.Err())
		return ctx.Err()
	}
}

// Take removes and admits the oldest live entry. It waits for an entry or
// the caller's context cancellation.
func (q *Queue) Take(ctx context.Context) (*Ticket, error) {
	return q.take(ctx, false, nil)
}

// TakeLightestGET removes and admits the lowest-weight queued GET. Ties keep
// enqueue order. Non-GET entries are left in place.
func (q *Queue) TakeLightestGET(ctx context.Context) (*Ticket, error) {
	return q.take(ctx, true, nil)
}

// TakeLightestGETWith admits the lowest-weight GET after prepare has run on
// the item while its ticket is still unpublished. This lets a probe selector
// update the request atomically with queue admission.
func (q *Queue) TakeLightestGETWith(ctx context.Context, prepare func(*Item) error) (*Ticket, error) {
	return q.take(ctx, true, prepare)
}

func (q *Queue) take(ctx context.Context, lightestGET bool, prepare func(*Item) error) (*Ticket, error) {
	if q == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		q.mu.Lock()
		q.pruneLocked(time.Now())
		if len(q.entries) > 0 {
			index := 0
			if lightestGET {
				index = -1
				for i, candidate := range q.entries {
					if !isGET(candidate.ticket.Item) {
						continue
					}
					if index < 0 || candidate.ticket.Item.Weight < q.entries[index].ticket.Item.Weight {
						index = i
					}
				}
			}
			if index >= 0 {
				selected := q.entries[index]
				if prepare != nil {
					item := selected.ticket.Item
					if err := prepare(&item); err != nil {
						q.mu.Unlock()
						return nil, err
					}
					selected.ticket.Item = item
				}
				q.entries = removeEntry(q.entries, index)
				selected.ticket.finish(nil)
				q.signalLocked()
				q.mu.Unlock()
				return selected.ticket, nil
			}
		}
		if q.closed {
			q.mu.Unlock()
			return nil, ErrClosed
		}
		changed := q.changed
		q.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Remove removes a queued ticket and completes it with ErrRemoved. It
// returns false if another operation already admitted or removed it.
func (q *Queue) Remove(ticket *Ticket) bool {
	if ticket == nil {
		return false
	}
	return q.remove(ticket.Item.ID, ErrRemoved)
}

func (q *Queue) remove(id uint64, err error) bool {
	if q == nil || id == 0 {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, candidate := range q.entries {
		if candidate.ticket.Item.ID != id {
			continue
		}
		q.entries = removeEntry(q.entries, i)
		candidate.ticket.finish(err)
		q.signalLocked()
		return true
	}
	return false
}

// Snapshot returns a copy of currently live items in queue order. The
// returned slice and items can be modified by the caller.
func (q *Queue) Snapshot() []Item {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	q.pruneLocked(time.Now())
	items := make([]Item, 0, len(q.entries))
	for _, e := range q.entries {
		items = append(items, e.ticket.Item)
	}
	q.mu.Unlock()
	return items
}

// Len reports the number of live entries.
func (q *Queue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	q.pruneLocked(time.Now())
	length := len(q.entries)
	q.mu.Unlock()
	return length
}

// Capacity reports the configured maximum number of entries.
func (q *Queue) Capacity() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	capacity := q.maxSize
	q.mu.Unlock()
	return capacity
}

// Close rejects new entries and completes currently queued entries. It is
// safe to call more than once.
func (q *Queue) Close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	entries := q.entries
	q.entries = nil
	for _, e := range entries {
		e.ticket.finish(ErrClosed)
	}
	q.signalLocked()
	q.mu.Unlock()
}

func (q *Queue) watch(e *entry) {
	if e == nil || e.ticket == nil {
		return
	}
	var timer *time.Timer
	if !e.deadline.IsZero() {
		delay := time.Until(e.deadline)
		if delay < 0 {
			delay = 0
		}
		timer = time.NewTimer(delay)
		defer timer.Stop()
	}
	if timer == nil {
		select {
		case <-e.ctx.Done():
			q.remove(e.ticket.Item.ID, e.ctx.Err())
		case <-e.ticket.finished:
		}
		return
	}
	select {
	case <-e.ctx.Done():
		q.remove(e.ticket.Item.ID, e.ctx.Err())
	case <-timer.C:
		q.expire(e)
	case <-e.ticket.finished:
	}
}

func (q *Queue) expire(e *entry) {
	if e == nil || e.ticket == nil {
		return
	}
	now := time.Now()
	waited := now.Sub(e.enqueued)
	remaining := e.ticket.Item.RetryAfter - waited
	if remaining < 0 {
		remaining = 0
	}
	q.remove(e.ticket.Item.ID, &TimeoutError{
		Waited:              waited,
		RemainingRetryAfter: remaining,
	})
}

func (q *Queue) pruneLocked(now time.Time) {
	if len(q.entries) == 0 {
		return
	}
	original := q.entries
	kept := original[:0]
	for _, e := range original {
		if e == nil || e.ticket == nil {
			continue
		}
		if e.ctx != nil && e.ctx.Err() != nil {
			e.ticket.finish(e.ctx.Err())
			continue
		}
		if !e.deadline.IsZero() && !now.Before(e.deadline) {
			waited := now.Sub(e.enqueued)
			remaining := e.ticket.Item.RetryAfter - waited
			if remaining < 0 {
				remaining = 0
			}
			e.ticket.finish(&TimeoutError{Waited: waited, RemainingRetryAfter: remaining})
			continue
		}
		kept = append(kept, e)
	}
	for i := len(kept); i < len(original); i++ {
		original[i] = nil
	}
	q.entries = kept
}

func (q *Queue) signalLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func isGET(item Item) bool {
	method := item.Method
	if method == "" && item.Request != nil {
		method = item.Request.Method
	}
	return strings.EqualFold(method, http.MethodGet)
}

func removeEntry(entries []*entry, index int) []*entry {
	copy(entries[index:], entries[index+1:])
	entries[len(entries)-1] = nil
	return entries[:len(entries)-1]
}
