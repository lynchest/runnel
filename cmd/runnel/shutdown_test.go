package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/storage"
)

func TestApplicationShutdownDrainsWriterAndTruncatesWAL(t *testing.T) {
	cfg := testConfig(t)
	dbPath := cfg.Storage.DBPath
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := storage.NewCacheEntry("shutdown-key", []byte("durable"), time.Hour)
	if err := app.cache.Set(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal("second shutdown: ", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatal("database was not retained after shutdown: ", err)
	}
	if _, err := os.Stat(dbPath + "-wal"); err == nil {
		t.Fatal("TRUNCATE checkpoint left a WAL sidecar")
	}
	reopened, err := storage.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	}()
	got, hit, err := reopened.Repository().Get(context.Background(), entry.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !hit || string(got.Body) != "durable" {
		t.Fatalf("reopened cache entry = %+v hit=%v, want durable hit", got, hit)
	}
}

func TestApplicationServeAndGracefulShutdownUsesIPv4Listener(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.DBPath = filepath.Join(t.TempDir(), "serve.db")
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			if shutdownErr := app.Shutdown(context.Background()); shutdownErr != nil {
				t.Errorf("shutdown application: %v", shutdownErr)
			}
			t.Skipf("host does not permit IPv4 loopback listeners: %v", err)
		}
		if shutdownErr := app.Shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutdown application: %v", shutdownErr)
		}
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(listener) }()

	client := &http.Client{Timeout: time.Second}
	adminURL := "http://" + listener.Addr().String() + "/_healthz"
	var response *http.Response
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get(adminURL)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		if shutdownErr := app.Shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutdown application: %v", shutdownErr)
		}
		t.Fatal("health request: ", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close health response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", response.StatusCode)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := app.Shutdown(shutdownCtx); err != nil {
		t.Fatal("shutdown: ", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal("serve: ", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	_, err = client.Get(adminURL)
	if err == nil {
		t.Fatal("health endpoint still accepted requests after shutdown")
	}
}
