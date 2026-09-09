package storage

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestVaryMetadataRoundTrip(t *testing.T) {
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
	repo := store.Repository()
	entry := CacheEntry{
		Key:        "vary:roundtrip",
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": {"text/plain"}, "Vary": {"Accept-Encoding"}},
		Body:       []byte("body"),
		ExpiresAt:  time.Now().Add(time.Hour),
	}.WithVary(map[string]string{"accept-encoding": "gzip"})
	if err := repo.Set(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := repo.Get(context.Background(), entry.Key)
	if err != nil || !ok {
		t.Fatalf("get = hit %v err %v", ok, err)
	}
	if !got.VaryKnown() {
		t.Fatal("round-tripped entry lost Vary metadata")
	}
	if len(got.Vary) != 1 || got.Vary["accept-encoding"] != "gzip" {
		t.Fatalf("vary = %v, want accept-encoding=gzip", got.Vary)
	}
	if got.Headers.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("response Vary header = %q, want preserved", got.Headers.Get("Vary"))
	}
}

func TestLegacyRowWithoutVaryIsUnsealed(t *testing.T) {
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
	// Rows written before Vary tracking store a bare header map.
	if _, err := store.DB().Exec(`INSERT INTO http_cache
		(key, status_code, headers, body, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"vary:legacy", 200, `{"Content-Type":["text/plain"]}`, []byte("old"),
		time.Now().Add(time.Hour).UnixNano(), time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Repository().Get(context.Background(), "vary:legacy")
	if err != nil || !ok {
		t.Fatalf("get = hit %v err %v", ok, err)
	}
	if got.VaryKnown() {
		t.Fatal("legacy row must not report Vary metadata")
	}
	if string(got.Body) != "old" {
		t.Fatalf("body = %q, want old", got.Body)
	}
}

func TestNoStaleRoundTrip(t *testing.T) {
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
	repo := store.Repository()
	entry := CacheEntry{
		Key:        "nostale:roundtrip",
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Cache-Control": {"max-age=60, must-revalidate"}},
		Body:       []byte("body"),
		ExpiresAt:  time.Now().Add(time.Hour),
		NoStale:    true,
	}.WithVary(nil)
	if err := repo.Set(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := repo.Get(context.Background(), entry.Key)
	if err != nil || !ok {
		t.Fatalf("get = hit %v err %v", ok, err)
	}
	if !got.NoStale {
		t.Fatal("NoStale flag lost in round-trip")
	}
	if !got.VaryKnown() {
		t.Fatal("round-tripped entry lost Vary metadata")
	}
}
