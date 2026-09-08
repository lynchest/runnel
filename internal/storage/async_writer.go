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

type cacheWriteTask struct {
	entry  CacheEntry
	key    string
	delete bool
}

// WriterOptions controls the bounded queue and batch policy. Zero values use
// the Phase 4 defaults.
type WriterOptions struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Logger        *log.Logger
}

// WriterStats contains monotonic writer counters.
type WriterStats struct {
	Enqueued uint64
	Written  uint64
	Dropped  uint64
	Failed   uint64
	Panics   uint64
}

type flushRequest struct {
	response chan error
}

// AsyncWriter is the sole asynchronous SQLite writer. Enqueue and
// EnqueueDelete never wait for capacity: a full queue is dropped and exposed
// through WriterStats.Dropped.
type AsyncWriter struct {
	repo  *CacheRepository
	dirty *DirtyBuffer
	opts  WriterOptions

	tasks   chan cacheWriteTask
	control chan flushRequest
	done    chan struct{}

	stateMu sync.Mutex
	closed  bool

	lastErrMu sync.RWMutex
	lastErr   error

	enqueued atomic.Uint64
	written  atomic.Uint64
	dropped  atomic.Uint64
	failed   atomic.Uint64
	panics   atomic.Uint64
}

// NewAsyncWriter validates dependencies before starting its worker. All
// worker dependencies are immutable after this function returns.
func NewAsyncWriter(repo *CacheRepository, dirty *DirtyBuffer, options WriterOptions) (*AsyncWriter, error) {
	if repo == nil {
		return nil, errors.New("cache writer repository is nil")
	}
	if dirty == nil {
		return nil, errors.New("cache writer dirty buffer is nil")
	}
	if options.QueueSize <= 0 {
		options.QueueSize = DefaultWriterBufferSize
	}
	if options.BatchSize <= 0 {
		options.BatchSize = DefaultWriterBatchSize
	}
	if options.FlushInterval <= 0 {
		options.FlushInterval = DefaultWriterFlushInterval
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}
	writer := &AsyncWriter{
		repo:    repo,
		dirty:   dirty,
		opts:    options,
		tasks:   make(chan cacheWriteTask, options.QueueSize),
		control: make(chan flushRequest, 1),
		done:    make(chan struct{}),
	}
	go writer.run()
	return writer, nil
}

// Enqueue accepts an entry without blocking. It returns false when the
// bounded queue is full, closed, or the entry key is empty.
func (writer *AsyncWriter) Enqueue(entry CacheEntry) bool {
	if writer == nil || entry.Key == "" {
		return false
	}
	task := cacheWriteTask{entry: entry.Clone()}
	writer.stateMu.Lock()
	defer writer.stateMu.Unlock()
	if writer.closed {
		writer.dropped.Add(1)
		return false
	}
	select {
	case writer.tasks <- task:
		writer.enqueued.Add(1)
		// Keep dirty-buffer ordering identical to queue ordering. This write
		// happens before the state lock is released, so concurrent producers
		// cannot make an older value overwrite a newer own-write.
		writer.dirty.Set(task.entry)
		return true
	default:
		writer.dropped.Add(1)
		return false
	}
}

// EnqueueDelete queues a durable delete without blocking.
func (writer *AsyncWriter) EnqueueDelete(key string) bool {
	if writer == nil || key == "" {
		return false
	}
	task := cacheWriteTask{key: key, delete: true}
	writer.stateMu.Lock()
	defer writer.stateMu.Unlock()
	if writer.closed {
		writer.dropped.Add(1)
		return false
	}
	select {
	case writer.tasks <- task:
		writer.enqueued.Add(1)
		writer.dirty.Delete(key)
		return true
	default:
		writer.dropped.Add(1)
		return false
	}
}

func (writer *AsyncWriter) run() {
	defer func() {
		if recovered := recover(); recovered != nil {
			writer.panics.Add(1)
			writer.setLastError(fmt.Errorf("cache writer panic recovered: %v", recovered))
			writer.opts.Logger.Printf("[PANIC RECOVERED] cache writer: %v\nstack:\n%s", recovered, debug.Stack())
		}
		close(writer.done)
	}()

	batch := make([]cacheWriteTask, 0, writer.opts.BatchSize)
	timer := time.NewTimer(writer.opts.FlushInterval)
	defer timer.Stop()

	for {
		select {
		case task, ok := <-writer.tasks:
			if !ok {
				writer.drainAndFlush(&batch)
				return
			}
			batch = append(batch, task)
			if len(batch) >= writer.opts.BatchSize {
				writer.flushBatch(batch)
				batch = batch[:0]
				resetTimer(timer, writer.opts.FlushInterval)
			}
		case request := <-writer.control:
			writer.drainAndFlush(&batch)
			request.response <- writer.lastError()
			resetTimer(timer, writer.opts.FlushInterval)
		case <-timer.C:
			if len(batch) > 0 {
				writer.flushBatch(batch)
				batch = batch[:0]
			}
			resetTimer(timer, writer.opts.FlushInterval)
		}
	}
}

func (writer *AsyncWriter) drainAndFlush(batch *[]cacheWriteTask) {
	for {
		select {
		case task, ok := <-writer.tasks:
			if !ok {
				if len(*batch) > 0 {
					writer.flushBatch(*batch)
					*batch = (*batch)[:0]
				}
				return
			}
			*batch = append(*batch, task)
			if len(*batch) >= writer.opts.BatchSize {
				writer.flushBatch(*batch)
				*batch = (*batch)[:0]
			}
		default:
			if len(*batch) > 0 {
				writer.flushBatch(*batch)
				*batch = (*batch)[:0]
			}
			return
		}
	}
}

func (writer *AsyncWriter) flushBatch(batch []cacheWriteTask) {
	if len(batch) == 0 {
		return
	}
	err := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				writer.panics.Add(1)
				err = fmt.Errorf("cache writer batch panic recovered: %v", recovered)
				writer.opts.Logger.Printf("[PANIC RECOVERED] cache batch: %v\nstack:\n%s", recovered, debug.Stack())
			}
		}()
		return writer.repo.writeBatch(context.Background(), batch)
	}()
	if err != nil {
		writer.failed.Add(uint64(len(batch)))
		writer.setLastError(err)
		return
	}
	writer.written.Add(uint64(len(batch)))
	for _, task := range batch {
		if !task.delete {
			writer.dirty.deleteIfMatch(task.entry)
		}
	}
}

// Flush waits for all tasks accepted before the call to be committed.
func (writer *AsyncWriter) Flush(ctx context.Context) error {
	if writer == nil {
		return ErrWriterClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writer.stateMu.Lock()
	if writer.closed {
		writer.stateMu.Unlock()
		select {
		case <-writer.done:
			return writer.lastError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	request := flushRequest{response: make(chan error, 1)}
	select {
	case writer.control <- request:
		writer.stateMu.Unlock()
	case <-ctx.Done():
		writer.stateMu.Unlock()
		return ctx.Err()
	}
	select {
	case err := <-request.response:
		return err
	case <-writer.done:
		return writer.lastError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown closes the queue and waits for the worker to drain every accepted
// task. A canceled context returns to the caller, while the worker continues
// its bounded drain and eventually closes Done.
func (writer *AsyncWriter) Shutdown(ctx context.Context) error {
	if writer == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writer.stateMu.Lock()
	if !writer.closed {
		writer.closed = true
		close(writer.tasks)
	}
	writer.stateMu.Unlock()
	select {
	case <-writer.done:
		return writer.lastError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done closes when the worker has exited.
func (writer *AsyncWriter) Done() <-chan struct{} {
	if writer == nil {
		channel := make(chan struct{})
		close(channel)
		return channel
	}
	return writer.done
}

// QueueLen and QueueCapacity expose queue pressure for metrics.
func (writer *AsyncWriter) QueueLen() int {
	if writer == nil {
		return 0
	}
	return len(writer.tasks)
}

func (writer *AsyncWriter) QueueCapacity() int {
	if writer == nil {
		return 0
	}
	return cap(writer.tasks)
}

func (writer *AsyncWriter) Stats() WriterStats {
	if writer == nil {
		return WriterStats{}
	}
	return WriterStats{
		Enqueued: writer.enqueued.Load(),
		Written:  writer.written.Load(),
		Dropped:  writer.dropped.Load(),
		Failed:   writer.failed.Load(),
		Panics:   writer.panics.Load(),
	}
}

func (writer *AsyncWriter) lastError() error {
	writer.lastErrMu.RLock()
	defer writer.lastErrMu.RUnlock()
	return writer.lastErr
}

func (writer *AsyncWriter) setLastError(err error) {
	if err == nil {
		return
	}
	writer.lastErrMu.Lock()
	writer.lastErr = err
	writer.lastErrMu.Unlock()
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
