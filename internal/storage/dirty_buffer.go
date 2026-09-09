package storage

import (
	"sync"
	"time"
)

type dirtyValue struct {
	entry     CacheEntry
	expiresAt time.Time
}

// DirtyBuffer is the in-memory read-your-own-writes layer. Each value has a
// short configurable lifetime and is copied on both write and read.
type DirtyBuffer struct {
	values sync.Map
	ttl    time.Duration
	now    func() time.Time
}

// NewDirtyBuffer creates a synchronized buffer. A non-positive ttl selects
// the documented ten-second default; a nil clock uses time.Now.
func NewDirtyBuffer(ttl time.Duration, now func() time.Time) *DirtyBuffer {
	if ttl <= 0 {
		ttl = DefaultDirtyTTL
	}
	if now == nil {
		now = time.Now
	}
	return &DirtyBuffer{ttl: ttl, now: now}
}

// Set stores a defensive copy. Empty keys are ignored because they cannot be
// represented by the SQLite primary key.
func (buffer *DirtyBuffer) Set(entry CacheEntry) {
	if buffer == nil || entry.Key == "" {
		return
	}
	entry = normalizeEntry(entry)
	now := buffer.now()
	buffer.values.Store(entry.Key, &dirtyValue{entry: entry, expiresAt: now.Add(buffer.ttl)})
}

// Get returns a live defensive copy. Expired values are removed atomically.
func (buffer *DirtyBuffer) Get(key string) (CacheEntry, bool) {
	if buffer == nil || key == "" {
		return CacheEntry{}, false
	}
	raw, ok := buffer.values.Load(key)
	if !ok {
		return CacheEntry{}, false
	}
	value, ok := raw.(*dirtyValue)
	if !ok || value == nil {
		return CacheEntry{}, false
	}
	now := buffer.now()
	if !value.expiresAt.After(now) || !value.entry.Valid(now) {
		buffer.values.CompareAndDelete(key, raw)
		return CacheEntry{}, false
	}
	return value.entry.Clone(), true
}

// GetStale returns a value that is still retained by the dirty buffer even if
// its cache expiry has passed. It is used only for an explicitly configured
// stale-while-open fallback; the dirty-buffer retention TTL is still enforced.
func (buffer *DirtyBuffer) GetStale(key string) (CacheEntry, bool) {
	if buffer == nil || key == "" {
		return CacheEntry{}, false
	}
	raw, ok := buffer.values.Load(key)
	if !ok {
		return CacheEntry{}, false
	}
	value, ok := raw.(*dirtyValue)
	if !ok || value == nil {
		return CacheEntry{}, false
	}
	if !value.expiresAt.After(buffer.now()) {
		buffer.values.CompareAndDelete(key, raw)
		return CacheEntry{}, false
	}
	return value.entry.Clone(), true
}

// Delete removes key from the buffer.
func (buffer *DirtyBuffer) Delete(key string) {
	if buffer == nil || key == "" {
		return
	}
	buffer.values.Delete(key)
}

// deleteIfMatch removes only the value that was successfully persisted. A
// newer write for the same key is therefore never removed by an older batch.
func (buffer *DirtyBuffer) deleteIfMatch(entry CacheEntry) {
	if buffer == nil || entry.Key == "" {
		return
	}
	raw, ok := buffer.values.Load(entry.Key)
	if !ok {
		return
	}
	value, ok := raw.(*dirtyValue)
	if ok && value != nil && sameEntry(value.entry, entry) {
		buffer.values.CompareAndDelete(entry.Key, raw)
	}
}

func sameEntry(left, right CacheEntry) bool {
	if left.Key != right.Key || left.StatusCode != right.StatusCode ||
		!left.ExpiresAt.Equal(right.ExpiresAt) || !left.CreatedAt.Equal(right.CreatedAt) ||
		!left.UpdatedAt.Equal(right.UpdatedAt) || left.varySealed != right.varySealed ||
		left.NoStale != right.NoStale || !equalVary(left.Vary, right.Vary) {
		return false
	}
	if len(left.Body) != len(right.Body) {
		return false
	}
	for index := range left.Body {
		if left.Body[index] != right.Body[index] {
			return false
		}
	}
	return true
}
