package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
)

const (
	// DefaultMaxBodyBytes is the documented ingress body ceiling (10 MiB).
	DefaultMaxBodyBytes int64 = 10 * 1024 * 1024
)

var (
	// ErrBodyTooLarge is matched by BodyTooLargeError via errors.Is.
	ErrBodyTooLarge = errors.New("request body is too large")
	// ErrInvalidBodyLimit identifies a negative body limit.
	ErrInvalidBodyLimit = errors.New("request body limit must not be negative")
)

// BodyTooLargeError is returned after at most limit+1 bytes have been read.
// It is intentionally typed so a handler can map it to HTTP 413 without
// matching an implementation-specific error string.
type BodyTooLargeError struct {
	Limit int64
	Size  int64
}

func (e *BodyTooLargeError) Error() string {
	if e == nil {
		return ErrBodyTooLarge.Error()
	}
	if e.Size >= 0 {
		return fmt.Sprintf("request body exceeds limit of %d bytes (read at least %d)", e.Limit, e.Size)
	}
	return fmt.Sprintf("request body exceeds limit of %d bytes", e.Limit)
}

func (e *BodyTooLargeError) Unwrap() error {
	return ErrBodyTooLarge
}

// StatusCode makes the error directly usable by gateway response code.
func (e *BodyTooLargeError) StatusCode() int {
	return http.StatusRequestEntityTooLarge
}

// IsBodyTooLarge reports whether err represents an oversized request body.
func IsBodyTooLarge(err error) bool {
	return errors.Is(err, ErrBodyTooLarge)
}

// BodyLimiter reads a request body once, enforces a hard byte limit, and
// replaces it with an in-memory replayable reader on success.
type BodyLimiter struct {
	MaxBytes int64
}

// NewBodyLimiter creates a body limiter.  The limit is checked when Limit is
// called so constructing one never consumes or mutates a request.
func NewBodyLimiter(maxBytes int64) *BodyLimiter {
	return &BodyLimiter{MaxBytes: maxBytes}
}

// Limit enforces the configured limit and makes r.Body replayable.  On a
// successful read, r.GetBody is also populated and ContentLength is updated
// to the exact buffered size.
func (l *BodyLimiter) Limit(r *http.Request) error {
	if l == nil {
		return errors.New("body limiter is nil")
	}
	return LimitAndReplayBody(r, l.MaxBytes)
}

// LimitAndReplayBody reads at most maxBytes+1 bytes from r.Body.  The extra
// byte distinguishes an exact-limit body from an oversized body without
// allocating unbounded memory.  A successful request gets a fresh reader for
// every replay, suitable for multiple outbound attempts.
func LimitAndReplayBody(r *http.Request, maxBytes int64) error {
	if r == nil {
		return errors.New("request is nil")
	}
	if maxBytes < 0 {
		return ErrInvalidBodyLimit
	}

	if r.Body == nil {
		installReplayBody(r, nil)
		return nil
	}
	if r.ContentLength > maxBytes && r.ContentLength >= 0 {
		// Do not trust ContentLength for acceptance, but rejecting a known
		// oversized body early avoids reading attacker-controlled bytes.
		_ = r.Body.Close()
		r.Body = http.NoBody
		r.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return &BodyTooLargeError{Limit: maxBytes, Size: r.ContentLength}
	}

	original := r.Body
	readLimit := maxBytes
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(original, readLimit))
	closeErr := original.Close()
	if err != nil {
		installReplayBody(r, data)
		return err
	}
	if int64(len(data)) > maxBytes {
		// Keep only the bounded prefix available to code that wants to close or
		// inspect the body after receiving the typed error.
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.GetBody = nil
		return &BodyTooLargeError{Limit: maxBytes, Size: int64(len(data))}
	}
	if closeErr != nil {
		installReplayBody(r, data)
		return closeErr
	}
	installReplayBody(r, data)
	return nil
}

// EnforceBodyLimit maps an oversized body to HTTP 413 and returns false.  Any
// other read/configuration error is mapped to HTTP 400 and also returns false;
// successful requests return true for convenient handler composition.
func EnforceBodyLimit(w http.ResponseWriter, r *http.Request, maxBytes int64) bool {
	if w == nil {
		return false
	}
	if err := LimitAndReplayBody(r, maxBytes); err != nil {
		if IsBodyTooLarge(err) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return false
	}
	return true
}

func installReplayBody(r *http.Request, data []byte) {
	buffer := append([]byte(nil), data...)
	r.Body = io.NopCloser(bytes.NewReader(buffer))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buffer)), nil
	}
	r.ContentLength = int64(len(buffer))
}
