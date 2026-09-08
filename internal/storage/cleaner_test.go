package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanerDeletesExpiredRowsAndCheckpointsWAL(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	repo := store.Repository()
	if err := repo.Set(context.Background(), CacheEntry{Key: "expired", Body: []byte("old"), ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cleaner := NewCleaner(repo, CleanerOptions{})
	stats, err := cleaner.RunOnce(context.Background(), time.Now())
	if err != nil || stats.ExpiredDeleted != 1 || !stats.Checkpointed {
		t.Fatalf("RunOnce = %#v, %v", stats, err)
	}
}

func TestCleanerQuotaEvictsOldest500(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	repo := store.Repository()
	base := time.Now().Add(-time.Hour)
	for index := 0; index < DefaultEvictionBatchSize+1; index++ {
		if err := repo.Set(context.Background(), CacheEntry{
			Key:       fmt.Sprintf("quota:%04d", index),
			Body:      []byte("payload"),
			ExpiresAt: time.Now().Add(time.Hour),
			CreatedAt: base.Add(time.Duration(index) * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cleaner := NewCleaner(repo, CleanerOptions{MaxBytes: 1})
	stats, err := cleaner.RunOnce(context.Background(), time.Now())
	if err != nil || stats.Evicted != DefaultEvictionBatchSize {
		t.Fatalf("RunOnce = %#v, %v", stats, err)
	}
	count, err := repo.Count(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("remaining count = %d, %v; want 1", count, err)
	}
	if _, ok, err := repo.Get(context.Background(), "quota:0000"); err != nil || ok {
		t.Fatalf("oldest row remains: ok=%v err=%v", ok, err)
	}
	if _, ok, err := repo.Get(context.Background(), "quota:0500"); err != nil || !ok {
		t.Fatalf("newest row missing: ok=%v err=%v", ok, err)
	}
}

func TestCleanerStartShutdownStopsGoroutine(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	cleaner := NewCleaner(store.Repository(), CleanerOptions{Interval: time.Millisecond})
	cleaner.Start(context.Background())
	time.Sleep(10 * time.Millisecond)
	if err := cleaner.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaner.Done():
	case <-time.After(time.Second):
		t.Fatal("cleaner goroutine did not exit")
	}
}
