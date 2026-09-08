package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedirectRelativeLocationRewritten(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://api.test.com/v1/items?page=1", nil)
	rewritten, err := RewriteRedirect(request, "/v2/games")
	if err != nil {
		t.Fatal(err)
	}
	want := "/proxy?url=https%3A%2F%2Fapi.test.com%2Fv2%2Fgames"
	if rewritten != want {
		t.Fatalf("rewritten Location = %q, want %q", rewritten, want)
	}
}

func TestRedirectRelativePathUsesRequestDirectory(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://api.test.com/v1/items", nil)
	rewritten, err := RewriteRedirect(request, "next")
	if err != nil {
		t.Fatal(err)
	}
	want := "/proxy?url=https%3A%2F%2Fapi.test.com%2Fv1%2Fnext"
	if rewritten != want {
		t.Fatalf("rewritten Location = %q, want %q", rewritten, want)
	}
}

func TestRedirectAbsoluteLocationAndPolicy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://api.test.com/v1/items", nil)
	rewritten, err := RewriteRedirect(request, "https://other.test/v2")
	if err != nil {
		t.Fatal(err)
	}
	if rewritten != "/proxy?url=https%3A%2F%2Fother.test%2Fv2" {
		t.Fatalf("absolute Location rewrite = %q", rewritten)
	}
	if err := StopRedirects(request, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error = %v, want ErrUseLastResponse", err)
	}
}

func TestRedirectRejectsUnsupportedLocation(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://api.test.com/v1/items", nil)
	for _, location := range []string{"javascript:alert(1)", "file:///etc/passwd", "//", ""} {
		if _, err := RewriteRedirect(request, location); err == nil {
			t.Fatalf("RewriteRedirect(%q) unexpectedly succeeded", location)
		}
	}
}

func TestRedirectResponseLocationRewrite(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://api.test.com/v1/items", nil)
	response := &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": {"/v2/games"}},
		Request:    request,
	}
	if err := RewriteResponseLocation(response); err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Location"); got != "/proxy?url=https%3A%2F%2Fapi.test.com%2Fv2%2Fgames" {
		t.Fatalf("response Location = %q", got)
	}
}
