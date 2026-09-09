package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestResponseCacheTTL(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	def := time.Hour
	header := func(pairs ...string) http.Header {
		h := make(http.Header)
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}
	cases := []struct {
		name      string
		header    http.Header
		wantTTL   time.Duration
		wantCache bool
	}{
		{"no headers uses default", header(), def, true},
		{"nil headers use default", nil, def, true},
		{"no-store refuses", header("Cache-Control", "max-age=60, no-store"), 0, false},
		{"no-store alone refuses", header("Cache-Control", "no-store"), 0, false},
		{"private refuses shared cache", header("Cache-Control", "max-age=60, private"), 0, false},
		{"private with field refuses", header("Cache-Control", `private="Set-Cookie"`), 0, false},
		{"response no-cache refuses without revalidation", header("Cache-Control", "no-cache"), 0, false},
		{"max-age below default", header("Cache-Control", "max-age=60"), time.Minute, true},
		{"max-age capped by default", header("Cache-Control", "max-age=7200"), def, true},
		{"max-age zero refuses", header("Cache-Control", "max-age=0"), 0, false},
		{"s-maxage wins over max-age", header("Cache-Control", "max-age=600, s-maxage=30"), 30 * time.Second, true},
		{"s-maxage zero refuses", header("Cache-Control", "max-age=600, s-maxage=0"), 0, false},
		{"invalid max-age refuses fail-safe", header("Cache-Control", "max-age=nonsense"), 0, false},
		{"invalid s-maxage refuses even with valid max-age", header("Cache-Control", "s-maxage=abc, max-age=60"), 0, false},
		{"negative max-age refuses", header("Cache-Control", "max-age=-5"), 0, false},
		{"conflicting max-age repeats refuse", header("Cache-Control", "max-age=60, max-age=600"), 0, false},
		{"identical max-age repeats agree", header("Cache-Control", "max-age=60, max-age=60"), time.Minute, true},
		{"conflicting s-maxage repeats refuse", header("Cache-Control", "s-maxage=30, s-maxage=60"), 0, false},
		{"overflowing max-age refuses", header("Cache-Control", "max-age=99999999999999999999"), 0, false},
		{"case insensitive directives", header("Cache-Control", "Max-Age=60, No-Store"), 0, false},
		{"quoted max-age", header("Cache-Control", `max-age="60"`), time.Minute, true},
		{
			"expires used without max-age",
			header("Expires", "Thu, 10 Sep 2026 12:30:00 GMT", "Date", "Thu, 10 Sep 2026 12:00:00 GMT"),
			30 * time.Minute, true,
		},
		{
			"expires capped by default",
			header("Expires", "Thu, 10 Sep 2026 15:00:00 GMT", "Date", "Thu, 10 Sep 2026 12:00:00 GMT"),
			def, true,
		},
		{
			"past expires refuses",
			header("Expires", "Thu, 10 Sep 2026 11:00:00 GMT", "Date", "Thu, 10 Sep 2026 12:00:00 GMT"),
			0, false,
		},
		{
			"invalid expires falls back to default",
			header("Expires", "not-a-date"),
			def, true,
		},
		{
			"age reduces max-age lifetime",
			header("Cache-Control", "max-age=60", "Age", "59"),
			time.Second, true,
		},
		{
			"age exceeding max-age refuses",
			header("Cache-Control", "max-age=60", "Age", "60"),
			0, false,
		},
		{
			"stale date reduces lifetime",
			header("Cache-Control", "max-age=3600", "Date", "Thu, 10 Sep 2026 11:30:00 GMT"),
			30 * time.Minute, true,
		},
		{
			"already-stale date refuses",
			header("Cache-Control", "max-age=60", "Date", "Thu, 10 Sep 2026 11:00:00 GMT"),
			0, false,
		},
		{"unknown directives ignored", header("Cache-Control", "stale-while-revalidate=30"), def, true},
		{"must-revalidate still stored fresh", header("Cache-Control", "max-age=60, must-revalidate"), time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ttl, ok := responseCacheTTL(tc.header, def, now)
			if ok != tc.wantCache {
				t.Fatalf("cacheable = %v, want %v", ok, tc.wantCache)
			}
			if ok && ttl != tc.wantTTL {
				t.Fatalf("ttl = %s, want %s", ttl, tc.wantTTL)
			}
		})
	}
}

func TestResponseCacheTTLNonPositiveDefault(t *testing.T) {
	if _, ok := responseCacheTTL(http.Header{"Cache-Control": {"max-age=60"}}, 0, time.Now()); ok {
		t.Fatal("zero default TTL must refuse caching")
	}
}

func TestParseVaryFields(t *testing.T) {
	header := func(values ...string) http.Header {
		h := make(http.Header)
		for _, v := range values {
			h.Add("Vary", v)
		}
		return h
	}
	fields, star := parseVaryFields(header("Accept-Encoding", "accept-language"))
	if star || len(fields) != 2 || fields[0] != "accept-encoding" || fields[1] != "accept-language" {
		t.Fatalf("fields = %v star = %v, want sorted pair without star", fields, star)
	}
	if _, star := parseVaryFields(header("Accept-Encoding, *")); !star {
		t.Fatal("Vary with * must report star")
	}
	if _, star := parseVaryFields(header("*")); !star {
		t.Fatal("Vary: * must report star")
	}
	fields, star = parseVaryFields(header("Accept-Encoding, accept-encoding , ,"))
	if star || len(fields) != 1 || fields[0] != "accept-encoding" {
		t.Fatalf("fields = %v star = %v, want deduplicated single field", fields, star)
	}
	if fields, star := parseVaryFields(nil); star || len(fields) != 0 {
		t.Fatal("nil headers must yield no fields")
	}
}

func TestResponseMustRevalidate(t *testing.T) {
	header := func(values ...string) http.Header {
		h := make(http.Header)
		for _, v := range values {
			h.Add("Cache-Control", v)
		}
		return h
	}
	if responseMustRevalidate(nil) {
		t.Fatal("nil headers must not forbid stale")
	}
	if responseMustRevalidate(header("max-age=60")) {
		t.Fatal("plain max-age must allow stale fallback")
	}
	if !responseMustRevalidate(header("max-age=60, must-revalidate")) {
		t.Fatal("must-revalidate must forbid stale")
	}
	if !responseMustRevalidate(header("s-maxage=30, proxy-revalidate")) {
		t.Fatal("proxy-revalidate must forbid stale")
	}
	if !responseMustRevalidate(header("Must-Revalidate")) {
		t.Fatal("directive matching must be case-insensitive")
	}
}

func TestHeaderVariantDigest(t *testing.T) {
	request := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape("https://api.test/items"), nil)
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		return r
	}
	base := headerVariantDigest(request(nil))
	if headerVariantDigest(request(map[string]string{"X-Request-ID": "unique-per-request-1"})) != base {
		t.Fatal("X-Request-ID must not split the flight")
	}
	if headerVariantDigest(request(map[string]string{"Connection": "close"})) != base {
		t.Fatal("hop-by-hop headers must not split the flight")
	}
	if headerVariantDigest(request(map[string]string{"X-Tier": "pro"})) == base {
		t.Fatal("variant headers must split the flight")
	}
	if headerVariantDigest(request(map[string]string{"User-Agent": "a"})) ==
		headerVariantDigest(request(map[string]string{"User-Agent": "b"})) {
		t.Fatal("different User-Agent values must split the flight")
	}
	if headerVariantDigest(nil) != "" {
		t.Fatal("nil request digest must be empty")
	}
}

func TestSingleflightKeySplitsVariants(t *testing.T) {
	gateway := NewGateway(GatewayConfig{CacheTTL: time.Hour})
	target, err := url.Parse("https://api.test/items")
	if err != nil {
		t.Fatal(err)
	}
	free := httptest.NewRequest(http.MethodGet, "/proxy?url=x", nil)
	free.Header.Set("X-Tier", "free")
	pro := httptest.NewRequest(http.MethodGet, "/proxy?url=x", nil)
	pro.Header.Set("X-Tier", "pro")
	if gateway.singleflightKey(free, target) == gateway.singleflightKey(pro, target) {
		t.Fatal("singleflight key must differ across variant headers")
	}
	clone := httptest.NewRequest(http.MethodGet, "/proxy?url=x", nil)
	clone.Header.Set("X-Tier", "free")
	clone.Header.Set("X-Request-ID", "different-id")
	if gateway.singleflightKey(free, target) != gateway.singleflightKey(clone, target) {
		t.Fatal("singleflight key must ignore X-Request-ID")
	}
}

func TestDeltaSecondsDurationOverflowRefused(t *testing.T) {
	// 20e9 seconds fits int64 but wraps time.Duration positive without a
	// pre-multiplication guard; it must refuse the store.
	header := http.Header{"Cache-Control": {"max-age=20000000000"}}
	if _, ok := responseCacheTTL(header, time.Hour, time.Now()); ok {
		t.Fatal("overflowing max-age must refuse caching")
	}
	aged := http.Header{"Cache-Control": {"max-age=60"}, "Age": {"20000000000"}}
	if _, ok := responseCacheTTL(aged, time.Hour, time.Now()); ok {
		t.Fatal("overflowing Age must refuse caching")
	}
	boundary := http.Header{"Cache-Control": {"max-age=" + strconv.FormatInt(int64(1<<63-1)/int64(time.Second), 10)}}
	ttl, ok := responseCacheTTL(boundary, time.Hour, time.Now())
	if !ok || ttl != time.Hour {
		t.Fatalf("boundary max-age = %s %v, want capped default TTL", ttl, ok)
	}
}
