package storage

import (
	"context"
	"errors"
)

// CacheStore composes the dirty buffer, asynchronous writer, and durable
// repository into the cache used by the gateway.
type CacheStore struct {
	repo   *CacheRepository
	dirty  *DirtyBuffer
	writer *AsyncWriter
}

// NewCacheStore validates and retains all three layers. The writer must have
// been created with the same repository and dirty buffer; dependencies cannot
// be changed after the writer starts.
func NewCacheStore(repo *CacheRepository, dirty *DirtyBuffer, writer *AsyncWriter) (*CacheStore, error) {
	if repo == nil {
		return nil, errors.New("cache store repository is nil")
	}
	if dirty == nil {
		return nil, errors.New("cache store dirty buffer is nil")
	}
	if writer == nil {
		return nil, errors.New("cache store writer is nil")
	}
	if writer.repo != repo || writer.dirty != dirty {
		return nil, errors.New("cache store dependencies do not match writer")
	}
	return &CacheStore{repo: repo, dirty: dirty, writer: writer}, nil
}

// Get checks dirty memory before SQLite, so an accepted write is immediately
// visible to subsequent reads.
func (store *CacheStore) Get(ctx context.Context, key string) (CacheEntry, bool, error) {
	if store == nil {
		return CacheEntry{}, false, errors.New("cache store is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if entry, hit := store.dirty.Get(key); hit {
		return entry, true, nil
	}
	return store.repo.Get(ctx, key)
}

// GetStale checks dirty memory before allowing an expired durable row.
func (store *CacheStore) GetStale(ctx context.Context, key string) (CacheEntry, bool, error) {
	if store == nil {
		return CacheEntry{}, false, errors.New("cache store is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if entry, hit := store.dirty.GetStale(key); hit {
		return entry, true, nil
	}
	return store.repo.GetStale(ctx, key)
}

// Set enqueues an entry without blocking. The writer records it in dirty
// memory before returning, or returns ErrQueueFull if the queue is saturated.
func (store *CacheStore) Set(ctx context.Context, entry CacheEntry) error {
	if store == nil {
		return errors.New("cache store is nil")
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if !store.writer.Enqueue(entry) {
		return ErrQueueFull
	}
	return nil
}

// Delete removes a key from memory and enqueues its durable deletion.
func (store *CacheStore) Delete(ctx context.Context, key string) error {
	if store == nil {
		return errors.New("cache store is nil")
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if key == "" {
		return ErrInvalidCacheKey
	}
	if !store.writer.EnqueueDelete(key) {
		return ErrQueueFull
	}
	return nil
}

// Flush waits until all writes accepted before the call are committed.
func (store *CacheStore) Flush(ctx context.Context) error {
	if store == nil {
		return errors.New("cache store is nil")
	}
	return store.writer.Flush(ctx)
}

// Shutdown drains the writer. The SQLiteStore is closed by its owner after
// this call so other components can coordinate their shutdown ordering.
func (store *CacheStore) Shutdown(ctx context.Context) error {
	if store == nil {
		return nil
	}
	return store.writer.Shutdown(ctx)
}
