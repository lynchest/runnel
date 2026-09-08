package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSSRF_BlockPrivateIPs(t *testing.T) {
	resolver := ResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "localhost" {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})
	validator := NewURLValidator(resolver, nil)

	for _, rawURL := range []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://192.168.1.1/",
		"http://169.254.169.254/",
		"http://[::1]/",
		"http://[fc00::1]/",
		"http://[fe80::1]/",
		"http://[ff02::1]/",
		"http://[::]/",
		"http://[::ffff:127.0.0.1]/",
		"http://[::7f00:1]/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, err := validator.Validate(context.Background(), rawURL)
			if err == nil {
				t.Fatal("expected target to be blocked")
			}
			if !errors.Is(err, ErrSSRFBlocked) {
				t.Fatalf("expected SSRF error, got %v", err)
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) || validationErr.StatusCode() != http.StatusForbidden {
				t.Fatalf("expected HTTP 403 validation error, got %v", err)
			}
		})
	}
}

func TestSSRF_ResolverAndIPv6(t *testing.T) {
	called := 0
	validator := NewURLValidator(ResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		called++
		if host != "public.example" {
			t.Fatalf("resolver called with %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("2001:db8::10")}}, nil
	}), nil)

	target, err := validator.Validate(context.Background(), "https://PUBLIC.EXAMPLE/v1")
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("resolver called %d times, want 1", called)
	}
	if !target.IP.Equal(net.ParseIP("2001:db8::10")) || target.Host != "public.example" {
		t.Fatalf("unexpected validated target: %+v", target)
	}

	// Reject the whole resolution result when one answer is unsafe.  Pinning a
	// merely safe answer would still leave a DNS round-robin escape hatch.
	mixed := NewURLValidator(ResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2001:db8::10")}, {IP: net.ParseIP("::1")}}, nil
	}), nil)
	if _, err := mixed.Validate(context.Background(), "https://mixed.example/"); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("mixed safe/private answers not blocked: %v", err)
	}
}

func TestSSRF_AllowedDomainMatching(t *testing.T) {
	allowed := []string{"*.example.com", "api.other.test"}
	for host, want := range map[string]bool{
		"api.example.com":      true,
		"deep.api.example.com": true,
		"example.com":          false,
		"evil-example.com":     false,
		"api.other.test":       true,
		"other.test":           false,
	} {
		if got := IsAllowedDomain(host, allowed); got != want {
			t.Errorf("IsAllowedDomain(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSSRF_CheckRedirectBlocksPrivateTarget(t *testing.T) {
	validator := NewURLValidator(ResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	}), nil)

	redirect := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/private", nil)
	if err := validator.CheckRedirect(redirect, nil); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("redirect to private IPv4 was not blocked: %v", err)
	}
	redirect = httptest.NewRequest(http.MethodGet, "http://[::1]/private", nil)
	if err := validator.CheckRedirect(redirect, nil); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("redirect to private IPv6 was not blocked: %v", err)
	}

	safe := httptest.NewRequest(http.MethodGet, "https://safe.example/next", nil)
	if err := validator.CheckRedirect(safe, nil); err != nil {
		t.Fatalf("safe redirect rejected: %v", err)
	}
	if _, ok := ValidatedTargetFromContext(safe.Context()); !ok {
		t.Fatal("safe redirect did not carry pinned target")
	}
}

func TestSSRF_PinnedDialRejectsDifferentHost(t *testing.T) {
	validator := NewURLValidator(ResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	}), nil)
	target, err := validator.Validate(context.Background(), "https://safe.example/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.DialContext(context.Background(), "tcp", "other.example:443")
	if err == nil {
		t.Fatal("pinned dial accepted a different host")
	}
}

func TestSSRF_InvalidSchemes(t *testing.T) {
	validator := NewURLValidator(nil, nil)
	for _, rawURL := range []string{"file:///etc/passwd", "ftp://example.com/a", "://bad", "/relative"} {
		if _, err := validator.Validate(context.Background(), rawURL); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("Validate(%q) error = %v, want invalid target", rawURL, err)
		}
	}
	if _, err := validator.ValidateURL(context.Background(), &url.URL{Scheme: "http"}); !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("missing host error = %v, want invalid target", err)
	}
}

func TestSSRF_ValidationErrorRedactsCredentialsAndQuery(t *testing.T) {
	validator := NewURLValidator(nil, nil)
	_, err := validator.Validate(context.Background(), "https://user:password@example.test/path?token=secret")
	if err == nil {
		t.Fatal("expected userinfo to be rejected")
	}
	if containsAny(err.Error(), "password", "secret") {
		t.Fatalf("validation error leaked URL secret: %v", err)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
