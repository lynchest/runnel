package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type statusRoundTripFunc func(*http.Request) (*http.Response, error)

func (f statusRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRunStatusPrintsHumanSummary(t *testing.T) {
	t.Setenv("RUNNEL_URL", "http://gateway.test")
	client := &http.Client{Transport: statusRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"status":"ok"}`
		if request.URL.Path == "/_circuit" {
			body = `{"circuits":{"a.test":{"state":"CLOSED"},"b.test":{"state":"OPEN"},"c.test":{"state":"HALF_OPEN"}}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	var stdout, stderr bytes.Buffer
	if code := runStatus(&stdout, &stderr, client); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"Gateway: http://gateway.test", "Health: ok", "3 total, 1 closed, 1 open, 1 half-open"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output %q does not contain %q", stdout.String(), want)
		}
	}
}

func TestRunStatusReportsUnreachableGateway(t *testing.T) {
	t.Setenv("RUNNEL_URL", "http://gateway.test")
	client := &http.Client{Transport: statusRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	var stdout, stderr bytes.Buffer
	if code := runStatus(&stdout, &stderr, client); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "cannot reach runnel gateway") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
