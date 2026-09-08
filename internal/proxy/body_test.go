package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodyLimiter_ReplayableAtLimit(t *testing.T) {
	const limit = int64(8)
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/proxy", strings.NewReader("12345678"))
	if err := LimitAndReplayBody(req, limit); err != nil {
		t.Fatal(err)
	}
	first, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	second, err := io.ReadAll(secondBody)
	if closeErr := secondBody.Close(); closeErr != nil {
		t.Errorf("close replay body: %v", closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "12345678" || string(second) != "12345678" {
		t.Fatalf("body was not replayable: %q / %q", first, second)
	}
	if req.ContentLength != limit {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, limit)
	}
}

func TestBodyLimiter_TooLarge(t *testing.T) {
	const limit = int64(10 * 1024 * 1024)
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/proxy", strings.NewReader(strings.Repeat("x", 11*1024*1024)))
	err := LimitAndReplayBody(req, limit)
	if err == nil || !IsBodyTooLarge(err) {
		t.Fatalf("expected typed too-large error, got %v", err)
	}
	var typed *BodyTooLargeError
	if !errors.As(err, &typed) || typed.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("error is not suitable for 413: %T %v", err, err)
	}
	if typed.Size <= limit {
		t.Fatalf("reported size = %d, want more than %d", typed.Size, limit)
	}
}

func TestBodyLimiter_KnownLengthTooLarge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/proxy", strings.NewReader("0123456789"))
	req.ContentLength = 4
	if err := LimitAndReplayBody(req, 3); !IsBodyTooLarge(err) {
		t.Fatalf("expected known-length too-large error, got %v", err)
	}
}

func TestBodyLimiter_NilBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/proxy", nil)
	req.Body = nil
	if err := LimitAndReplayBody(req, 0); err != nil {
		t.Fatal(err)
	}
	if req.Body == nil || req.GetBody == nil {
		t.Fatal("nil body was not replaced with a replayable empty body")
	}
}

func TestBodyLimiter_EnforceWrites413(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/proxy", strings.NewReader("12345"))
	recorder := httptest.NewRecorder()
	if EnforceBodyLimit(recorder, req, 4) {
		t.Fatal("oversized request was accepted")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
}
