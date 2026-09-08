package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const cacheSchema = `
CREATE TABLE IF NOT EXISTS http_cache (
	key         TEXT PRIMARY KEY NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 200,
	headers     TEXT NOT NULL DEFAULT '{}',
	body        BLOB NOT NULL DEFAULT X'',
	expires_at  INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_http_cache_expires_at ON http_cache (expires_at);
CREATE INDEX IF NOT EXISTS idx_http_cache_created_at ON http_cache (created_at);
`

const upsertSQL = `INSERT INTO http_cache
	(key, status_code, headers, body, expires_at, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET
		status_code = excluded.status_code,
		headers = excluded.headers,
		body = excluded.body,
		expires_at = excluded.expires_at,
		created_at = excluded.created_at,
		updated_at = excluded.updated_at`

// SQLiteStore owns the opened pure-Go SQLite database and its typed
// repository.
type SQLiteStore struct {
	db   *sql.DB
	path string

	closeOnce sync.Once
	closeErr  error
	repo      *CacheRepository
}

// OpenSQLite opens path, configures WAL/NORMAL/5000ms busy_timeout, checks
// integrity, and creates the cache schema. A damaged existing file is moved
// to path.corrupt.<timestamp> before a fresh database is created.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("SQLite database path cannot be empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create SQLite database directory: %w", err)
		}
	}
	db, err := openSQLite(path)
	if err != nil {
		if !corruptFile(path) {
			return nil, err
		}
		if quarantineErr := quarantine(path); quarantineErr != nil {
			return nil, fmt.Errorf("open SQLite database: %w; quarantine corrupt database: %v", err, quarantineErr)
		}
		db, err = openSQLite(path)
		if err != nil {
			return nil, fmt.Errorf("open fresh SQLite database after quarantine: %w", err)
		}
	}
	return &SQLiteStore{
		db:   db,
		path: path,
		repo: &CacheRepository{db: db, path: path},
	}, nil
}

func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure SQLite busy_timeout: %w", err)
	}
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure SQLite WAL journal: %w", err)
	}
	if journalMode != "wal" && journalMode != "WAL" {
		_ = db.Close()
		return nil, fmt.Errorf("configure SQLite WAL journal: got %q", journalMode)
	}
	if _, err := db.Exec(`PRAGMA synchronous = NORMAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure SQLite synchronous=NORMAL: %w", err)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check(1)`).Scan(&integrity); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("SQLite integrity_check: %w", err)
	}
	if integrity != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("SQLite integrity_check failed: %s", integrity)
	}
	if _, err := db.ExecContext(context.Background(), cacheSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create cache schema: %w", err)
	}
	return db, nil
}

// Repository returns the typed cache repository. It is safe to retain for the
// lifetime of the store.
func (store *SQLiteStore) Repository() *CacheRepository {
	if store == nil {
		return nil
	}
	return store.repo
}

// DB returns the underlying database for read-only diagnostics and PRAGMA
// assertions.
func (store *SQLiteStore) DB() *sql.DB {
	if store == nil {
		return nil
	}
	return store.db
}

// Path returns the opened path.
func (store *SQLiteStore) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

// FileSize returns the main database file size, excluding WAL sidecars.
func (store *SQLiteStore) FileSize() (int64, error) {
	if store == nil {
		return 0, errors.New("SQLite store is nil")
	}
	return fileSize(store.path)
}

// Close checkpoints the final WAL and closes the database. It is idempotent.
func (store *SQLiteStore) Close() error {
	if store == nil {
		return nil
	}
	store.closeOnce.Do(func() {
		if store.db == nil {
			return
		}
		if _, err := store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			store.closeErr = fmt.Errorf("final WAL checkpoint: %w", err)
		}
		if err := store.db.Close(); err != nil && store.closeErr == nil {
			store.closeErr = err
		}
	})
	return store.closeErr
}

func corruptFile(path string) bool {
	if path == ":memory:" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func quarantine(path string) error {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := path + ".corrupt." + stamp
	destination := base
	for suffix := 1; ; suffix++ {
		if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
			break
		}
		destination = fmt.Sprintf("%s.%d", base, suffix)
	}
	if err := os.Rename(path, destination); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		if _, err := os.Stat(sidecar); err != nil {
			continue
		}
		if err := os.Rename(sidecar, destination+suffix); err != nil {
			return fmt.Errorf("quarantine %s: %w", suffix, err)
		}
	}
	return nil
}

func fileSize(path string) (int64, error) {
	if path == "" || path == ":memory:" {
		return 0, errors.New("SQLite store has no file path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat SQLite database: %w", err)
	}
	return info.Size(), nil
}

func storedTime(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func readTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
}
