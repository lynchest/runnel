package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteWALSchemaAndTypedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	var journalMode string
	if err := store.DB().QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal mode = %q, want WAL", journalMode)
	}
	var synchronous, busyTimeout int
	if err := store.DB().QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if synchronous != 1 || busyTimeout != 5000 {
		t.Fatalf("SQLite pragmas = synchronous %d, busy_timeout %d", synchronous, busyTimeout)
	}

	expires := time.Now().Add(time.Hour)
	entry := CacheEntry{
		Key:        "steam:games:1",
		StatusCode: 206,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"ok":true}`),
		ExpiresAt:  expires,
	}
	repo := store.Repository()
	if err := repo.Set(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := repo.Get(context.Background(), entry.Key)
	if err != nil || !ok || got.StatusCode != 206 || string(got.Body) != string(entry.Body) {
		t.Fatalf("round-trip = %#v, hit=%v, err=%v", got, ok, err)
	}
	if got.Headers.Get("Content-Type") != "application/json" || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("headers/expiry mismatch: %#v", got)
	}
}

func TestSQLiteCorruptFileIsQuarantined(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "cache.db")
	if err := os.WriteFile(path, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "cache.db.corrupt.") {
			return
		}
	}
	t.Fatalf("no corrupt quarantine in %s", directory)
}

func TestSQLiteExpiredRowsCanBePurged(t *testing.T) {
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
	if err := repo.Set(context.Background(), CacheEntry{Key: "expired", Body: []byte("old"), ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Set(context.Background(), CacheEntry{Key: "live", Body: []byte("new"), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	removed, err := repo.DeleteExpired(context.Background(), time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("DeleteExpired = %d, %v; want 1", removed, err)
	}
}
