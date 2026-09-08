package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAsyncWriterHighConcurrencyNoLockErrors(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	dirty := NewDirtyBuffer(DefaultDirtyTTL, time.Now)
	writer, err := NewAsyncWriter(store.Repository(), dirty, WriterOptions{
		QueueSize:     DefaultWriterBufferSize,
		BatchSize:     DefaultWriterBatchSize,
		FlushInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	const total = 500
	var group sync.WaitGroup
	group.Add(total)
	accepted := make(chan struct{}, total)
	for index := 0; index < total; index++ {
		index := index
		go func() {
			defer group.Done()
			if writer.Enqueue(CacheEntry{Key: fmt.Sprintf("concurrent:%03d", index), Body: []byte("body"), ExpiresAt: time.Now().Add(time.Hour)}) {
				accepted <- struct{}{}
			}
		}()
	}
	group.Wait()
	close(accepted)
	if len(accepted) != total {
		t.Fatalf("accepted %d/%d tasks", len(accepted), total)
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := writer.Stats()
	if stats.Failed != 0 || stats.Dropped != 0 || stats.Written != total {
		t.Fatalf("writer stats = %#v", stats)
	}
	count, err := store.Repository().Count(context.Background())
	if err != nil || count != total {
		t.Fatalf("durable count = %d, %v; want %d", count, err, total)
	}
}

func TestAsyncWriterNonBlockingDropMetric(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	dirty := NewDirtyBuffer(DefaultDirtyTTL, time.Now)
	writer, err := NewAsyncWriter(store.Repository(), dirty, WriterOptions{
		QueueSize:     1,
		BatchSize:     100000,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writer.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown writer: %v", err)
		}
	}()
	var group sync.WaitGroup
	for index := 0; index < 256; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			writer.Enqueue(CacheEntry{Key: fmt.Sprintf("drop:%d", index), Body: []byte("x")})
		}(index)
	}
	group.Wait()
	if writer.Stats().Dropped == 0 {
		t.Fatal("expected at least one nonblocking queue drop")
	}
}

func TestAsyncWriterShutdownDrainsQueue(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	dirty := NewDirtyBuffer(DefaultDirtyTTL, time.Now)
	writer, err := NewAsyncWriter(store.Repository(), dirty, WriterOptions{QueueSize: 32, BatchSize: 8, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 17; index++ {
		if !writer.Enqueue(CacheEntry{Key: fmt.Sprintf("drain:%d", index), Body: []byte("ok")}) {
			t.Fatalf("enqueue %d failed", index)
		}
	}
	if err := writer.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	count, err := store.Repository().Count(context.Background())
	if err != nil || count != 17 {
		t.Fatalf("drained count = %d, %v; want 17", count, err)
	}
}

func TestCacheStoreReadYourOwnWrites(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	dirty := NewDirtyBuffer(DefaultDirtyTTL, time.Now)
	writer, err := NewAsyncWriter(store.Repository(), dirty, WriterOptions{FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewCacheStore(store.Repository(), dirty, writer)
	if err != nil {
		t.Fatal(err)
	}
	entry := CacheEntry{Key: "own-write", Body: []byte("memory-first"), ExpiresAt: time.Now().Add(time.Hour)}
	if err := cache.Set(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := cache.Get(context.Background(), entry.Key)
	if err != nil || !ok || string(got.Body) != "memory-first" {
		t.Fatalf("cache read = %#v, hit=%v, err=%v", got, ok, err)
	}
	if err := cache.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
