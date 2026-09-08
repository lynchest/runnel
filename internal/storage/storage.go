// Package storage provides runnel's two-layer HTTP response cache.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultDirtyTTL is the lifetime of an entry in the in-memory write
	// buffer. It is deliberately short because SQLite is the durable layer.
	DefaultDirtyTTL = 10 * time.Second

	// DefaultWriterBufferSize bounds the asynchronous write queue.
	DefaultWriterBufferSize = 2048
	// DefaultWriterBatchSize and DefaultWriterFlushInterval control commits.
	DefaultWriterBatchSize     = 50
	DefaultWriterFlushInterval = 100 * time.Millisecond
	// DefaultEvictionBatchSize is the quota eviction batch size.
	DefaultEvictionBatchSize = 500
)

var (
	ErrInvalidCacheKey = errors.New("cache key cannot be empty")
	ErrQueueFull       = errors.New("cache writer queue is full")
	ErrWriterClosed    = errors.New("cache writer is closed")
)

// CacheEntry is one typed HTTP response in the cache.
type CacheEntry struct {
	Key        string
	StatusCode int
	Headers    http.Header
	Body       []byte
	ExpiresAt  time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewCacheEntry creates a 200 response entry with an optional expiry TTL.
func NewCacheEntry(key string, body []byte, ttl time.Duration) CacheEntry {
	now := time.Now()
	entry := CacheEntry{
		Key:        key,
		StatusCode: http.StatusOK,
		Headers:    make(http.Header),
		Body:       cloneBytes(body),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if ttl > 0 {
		entry.ExpiresAt = now.Add(ttl)
	}
	return normalizeEntry(entry)
}

// Valid reports whether the entry can be served at now. Zero expiry means no
// expiry and is useful for state-like rows.
func (entry CacheEntry) Valid(now time.Time) bool {
	return entry.ExpiresAt.IsZero() || entry.ExpiresAt.After(now)
}

// Clone returns a defensive copy suitable for crossing goroutine boundaries.
func (entry CacheEntry) Clone() CacheEntry {
	return normalizeEntry(entry)
}

// Payload returns a defensive copy of Body.
func (entry CacheEntry) Payload() []byte {
	return cloneBytes(entry.Body)
}

func normalizeEntry(entry CacheEntry) CacheEntry {
	if entry.StatusCode == 0 {
		entry.StatusCode = http.StatusOK
	}
	entry.Headers = cloneHeader(entry.Headers)
	entry.Body = cloneBytes(entry.Body)
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = entry.CreatedAt
	}
	return entry
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func cloneHeader(value http.Header) http.Header {
	if value == nil {
		return make(http.Header)
	}
	result := make(http.Header, len(value))
	for key, values := range value {
		result[key] = append([]string(nil), values...)
	}
	return result
}

// CacheRepository is the typed SQLite cache repository returned by
// SQLiteStore.Repository. Its writes are serialized in process; SQLite's WAL
// mode handles readers while the single writer is committing.
type CacheRepository struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

// Get returns a fresh entry and hit flag. Expired rows are treated as misses.
func (repo *CacheRepository) Get(ctx context.Context, key string) (CacheEntry, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateKey(key); err != nil {
		return CacheEntry{}, false, err
	}
	row := repo.db.QueryRowContext(ctx, `SELECT status_code, headers, body,
		expires_at, created_at, updated_at FROM http_cache
		WHERE key = ? AND (expires_at = 0 OR expires_at > ?)`, key, time.Now().UnixNano())
	return scanEntry(row, key)
}

// GetStale returns a row without applying its expiry predicate.
func (repo *CacheRepository) GetStale(ctx context.Context, key string) (CacheEntry, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateKey(key); err != nil {
		return CacheEntry{}, false, err
	}
	row := repo.db.QueryRowContext(ctx, `SELECT status_code, headers, body,
		expires_at, created_at, updated_at FROM http_cache WHERE key = ?`, key)
	return scanEntry(row, key)
}

// Set atomically upserts one typed cache entry.
func (repo *CacheRepository) Set(ctx context.Context, entry CacheEntry) error {
	if ctx == nil {
		ctx = context.Background()
	}
	entry = normalizeEntry(entry)
	if err := validateKey(entry.Key); err != nil {
		return err
	}
	headerJSON, err := json.Marshal(map[string][]string(entry.Headers))
	if err != nil {
		return fmt.Errorf("encode cache headers: %w", err)
	}
	repo.writeMu.Lock()
	defer repo.writeMu.Unlock()
	_, err = repo.db.ExecContext(ctx, upsertSQL, entry.Key, entry.StatusCode,
		headerJSON, entry.Body, storedTime(entry.ExpiresAt),
		storedTime(entry.CreatedAt), storedTime(entry.UpdatedAt))
	if err != nil {
		return fmt.Errorf("set cache entry %q: %w", entry.Key, err)
	}
	return nil
}

// Delete removes one durable cache row.
func (repo *CacheRepository) Delete(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateKey(key); err != nil {
		return err
	}
	repo.writeMu.Lock()
	defer repo.writeMu.Unlock()
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM http_cache WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete cache entry %q: %w", key, err)
	}
	return nil
}

// Count returns the total number of durable rows, including expired rows not
// yet swept by the cleaner.
func (repo *CacheRepository) Count(ctx context.Context) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var count int64
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM http_cache`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count cache entries: %w", err)
	}
	return count, nil
}

// DeleteExpired removes rows expired before now and returns the count.
func (repo *CacheRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	repo.writeMu.Lock()
	defer repo.writeMu.Unlock()
	result, err := repo.db.ExecContext(ctx, `DELETE FROM http_cache
		WHERE expires_at > 0 AND expires_at < ?`, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("delete expired cache entries: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired cache entries: %w", err)
	}
	return count, nil
}

// EvictOldest removes up to limit rows ordered by creation time.
func (repo *CacheRepository) EvictOldest(ctx context.Context, limit int) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		return 0, nil
	}
	repo.writeMu.Lock()
	defer repo.writeMu.Unlock()
	result, err := repo.db.ExecContext(ctx, `DELETE FROM http_cache WHERE key IN
		(SELECT key FROM http_cache ORDER BY created_at ASC, key ASC LIMIT ?)`, limit)
	if err != nil {
		return 0, fmt.Errorf("evict oldest cache entries: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count evicted cache entries: %w", err)
	}
	return count, nil
}

// DatabaseSize returns SQLite's page_count * page_size footprint.
func (repo *CacheRepository) DatabaseSize(ctx context.Context) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var pages, pageSize int64
	if err := repo.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, fmt.Errorf("read SQLite page count: %w", err)
	}
	if err := repo.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("read SQLite page size: %w", err)
	}
	return pages * pageSize, nil
}

// FileSize returns the main database file size for diagnostics.
func (repo *CacheRepository) FileSize() (int64, error) {
	return fileSize(repo.path)
}

// Checkpoint runs the required passive WAL checkpoint.
func (repo *CacheRepository) Checkpoint(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := repo.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return fmt.Errorf("WAL passive checkpoint: %w", err)
	}
	return nil
}

func (repo *CacheRepository) writeBatch(ctx context.Context, tasks []cacheWriteTask) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(tasks) == 0 {
		return nil
	}
	repo.writeMu.Lock()
	defer repo.writeMu.Unlock()
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cache batch: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, task := range tasks {
		if task.delete {
			if _, err := tx.ExecContext(ctx, `DELETE FROM http_cache WHERE key = ?`, task.key); err != nil {
				return fmt.Errorf("delete cache entry %q in batch: %w", task.key, err)
			}
			continue
		}
		entry := normalizeEntry(task.entry)
		headerJSON, err := json.Marshal(map[string][]string(entry.Headers))
		if err != nil {
			return fmt.Errorf("encode cache headers: %w", err)
		}
		if _, err := tx.ExecContext(ctx, upsertSQL, entry.Key, entry.StatusCode,
			headerJSON, entry.Body, storedTime(entry.ExpiresAt),
			storedTime(entry.CreatedAt), storedTime(entry.UpdatedAt)); err != nil {
			return fmt.Errorf("set cache entry %q in batch: %w", entry.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cache batch: %w", err)
	}
	committed = true
	return nil
}

func validateKey(key string) error {
	if key == "" {
		return ErrInvalidCacheKey
	}
	return nil
}

func scanEntry(row *sql.Row, key string) (CacheEntry, bool, error) {
	var (
		statusCode                      int
		headerJSON                      string
		body                            []byte
		expiresAt, createdAt, updatedAt int64
	)
	if err := row.Scan(&statusCode, &headerJSON, &body, &expiresAt, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CacheEntry{}, false, nil
		}
		return CacheEntry{}, false, fmt.Errorf("scan cache entry %q: %w", key, err)
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(headerJSON), &headers); err != nil {
		return CacheEntry{}, false, fmt.Errorf("decode cache headers %q: %w", key, err)
	}
	return normalizeEntry(CacheEntry{
		Key:        key,
		StatusCode: statusCode,
		Headers:    http.Header(headers),
		Body:       body,
		ExpiresAt:  readTime(expiresAt),
		CreatedAt:  readTime(createdAt),
		UpdatedAt:  readTime(updatedAt),
	}), true, nil
}
