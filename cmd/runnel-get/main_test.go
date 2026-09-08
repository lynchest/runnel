package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestRunMarkdownOutput(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/_healthz":
			return response(http.StatusOK, "application/json", `{}`), nil
		case "/proxy":
			if got := r.URL.Query().Get("url"); got != "https://example.com/page?q=one two" {
				t.Errorf("target URL = %q", got)
			}
			if got := r.Header.Get("Cache-Control"); got != "no-cache" {
				t.Errorf("Cache-Control = %q", got)
			}
			return response(http.StatusOK, "text/html; charset=utf-8", "<main><h1>Title</h1><script>noise</script><p>Body</p></main>"), nil
		default:
			return response(http.StatusNotFound, "text/plain", "not found"), nil
		}
	})}
	t.Setenv("RUNNEL_URL", "http://gateway.test")

	var stdout, stderr bytes.Buffer
	exitCode := runWithClient([]string{"--output", "markdown", "--no-cache", "https://example.com/page?q=one two"}, &stdout, &stderr, client)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got, want := stdout.String(), "# Title\n\nBody\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunLeavesNonHTMLResponseRaw(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/_healthz" {
			return response(http.StatusOK, "application/json", `{}`), nil
		}
		return response(http.StatusOK, "application/json", `{"value":null,"items":[]}`), nil
	})}
	t.Setenv("RUNNEL_URL", "http://gateway.test")

	var stdout, stderr bytes.Buffer
	if exitCode := runWithClient([]string{"--output=markdown", "https://example.com/data"}, &stdout, &stderr, client); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got, want := stdout.String(), `{"value":null,"items":[]}`; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := run([]string{"--help"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
}
