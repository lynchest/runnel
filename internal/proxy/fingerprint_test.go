package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFingerprint_QuerySorting(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "https://api.example.test/items?b=2&a=1&a=3", nil)
	second := httptest.NewRequest(http.MethodGet, "https://API.EXAMPLE.TEST/items?a=3&b=2&a=1", nil)
	if got, want := Fingerprint(first), Fingerprint(second); got != want {
		t.Fatalf("query order changed fingerprint: %s != %s", got, want)
	}
	if CanonicalQuery(first.URL) != "a=1&a=3&b=2" {
		t.Fatalf("unexpected canonical query: %q", CanonicalQuery(first.URL))
	}
}

func TestFingerprint_AuthCookieIsolation(t *testing.T) {
	anonymous := httptest.NewRequest(http.MethodGet, "https://api.example.test/items?a=1", nil)
	authenticated := httptest.NewRequest(http.MethodGet, "https://api.example.test/items?a=1", nil)
	authenticated.AddCookie(&http.Cookie{Name: "steamLoginSecure", Value: "xyz"})
	if Fingerprint(anonymous) == Fingerprint(authenticated) {
		t.Fatal("selected auth cookie did not isolate fingerprints")
	}
	if strings.Contains(Fingerprint(authenticated), "xyz") || strings.Contains(AuthHash(authenticated), "xyz") {
		t.Fatal("credential value leaked into fingerprint output")
	}
}

func TestFingerprint_AuthorizationIsolation(t *testing.T) {
	first := httptest.NewRequest(http.MethodPost, "https://api.example.test/items", nil)
	second := httptest.NewRequest(http.MethodPost, "https://api.example.test/items", nil)
	first.Header.Set("Authorization", "Bearer first-secret")
	second.Header.Set("Authorization", "Bearer second-secret")
	if Fingerprint(first) == Fingerprint(second) {
		t.Fatal("Authorization value did not isolate fingerprints")
	}
	if strings.Contains(AuthHash(first), "first-secret") {
		t.Fatal("Authorization value leaked into auth hash")
	}
}

func TestFingerprint_CustomCookieSelection(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "https://api.example.test/items", nil)
	second := httptest.NewRequest(http.MethodGet, "https://api.example.test/items", nil)
	first.AddCookie(&http.Cookie{Name: "nonAuth", Value: "one"})
	second.AddCookie(&http.Cookie{Name: "nonAuth", Value: "two"})
	if Fingerprint(first, []string{"session"}) != Fingerprint(second, []string{"session"}) {
		t.Fatal("unselected cookie changed fingerprint")
	}
	second.AddCookie(&http.Cookie{Name: "session", Value: "two"})
	if Fingerprint(first, []string{"session"}) == Fingerprint(second, []string{"session"}) {
		t.Fatal("selected cookie did not change fingerprint")
	}
}

func TestFingerprint_AcceptLanguageIsolation(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "https://api.example.test/items", nil)
	second := httptest.NewRequest(http.MethodGet, "https://api.example.test/items", nil)
	first.Header.Set("Accept-Language", "en-US")
	second.Header.Set("Accept-Language", "tr-TR")
	if Fingerprint(first) == Fingerprint(second) {
		t.Fatal("Accept-Language did not change fingerprint")
	}
}

func BenchmarkGatewayRequestKey(b *testing.B) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/proxy?url=https://api.example.test/v1/items?page=1", nil)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Authorization", "Bearer token123")
	req.AddCookie(&http.Cookie{Name: "sessionid", Value: "sess456"})
	target, _ := req.URL.Parse("https://api.example.test/v1/items?page=1")

	g := &Gateway{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.requestKey(req, target)
	}
}
