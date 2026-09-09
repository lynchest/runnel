package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lynchest/runnel/internal/storage"
)

// responseCacheTTL decides how long an upstream response may be served from
// the shared cache. The configured defaultTTL is always an upper bound.
// Responses carrying no-store, private, or no-cache are never stored: this is
// a shared cache without conditional revalidation, so private and
// must-revalidate content cannot be served correctly. s-maxage takes
// precedence over max-age per RFC 7234 section 5.2.2.9; Expires is honored
// only when neither is present.
//
// Freshness directives are fail-safe: a present-but-unparseable max-age or
// s-maxage, or repeated occurrences with distinct values, refuse the store
// instead of falling back to the default TTL, because the upstream freshness
// intent is ambiguous. The returned lifetime subtracts the response current
// age (Age header and Date skew); already-stale responses are refused.
func responseCacheTTL(header http.Header, defaultTTL time.Duration, now time.Time) (time.Duration, bool) {
	if defaultTTL <= 0 {
		return 0, false
	}
	var combined string
	if header != nil {
		combined = strings.Join(header.Values("Cache-Control"), ", ")
	}
	directives := parseCacheControl(combined)
	if hasDirective(directives, "no-store") {
		return 0, false
	}
	if hasDirective(directives, "private") {
		return 0, false
	}
	if hasDirective(directives, "no-cache") {
		return 0, false
	}
	var lifetime time.Duration
	if _, present := directives["s-maxage"]; present {
		var ok bool
		if lifetime, ok = freshnessFrom(directives["s-maxage"]); !ok {
			return 0, false
		}
	} else if _, present := directives["max-age"]; present {
		var ok bool
		if lifetime, ok = freshnessFrom(directives["max-age"]); !ok {
			return 0, false
		}
	} else if header != nil {
		if raw := strings.TrimSpace(header.Get("Expires")); raw != "" {
			expires, err := http.ParseTime(raw)
			if err != nil {
				// An unparseable Expires carries no usable lifetime; fall
				// back to the operator default rather than guessing.
				return defaultTTL, true
			}
			base := now
			if dateRaw := strings.TrimSpace(header.Get("Date")); dateRaw != "" {
				if date, err := http.ParseTime(dateRaw); err == nil {
					base = date
				}
			}
			lifetime = expires.Sub(base)
		} else {
			return defaultTTL, true
		}
	} else {
		return defaultTTL, true
	}
	// Overflowing delta-seconds are rejected before conversion
	// (see secondsToDuration); a non-positive remainder here also refuses.
	ttl := min(lifetime-currentAge(header, now), defaultTTL)
	if ttl <= 0 {
		return 0, false
	}
	return ttl, true
}

// responseMustRevalidate reports whether the response forbids stale reuse.
// must-revalidate and proxy-revalidate still allow fresh cache hits; only
// the stale-while-open fallback must refuse these entries.
func responseMustRevalidate(header http.Header) bool {
	if header == nil {
		return false
	}
	directives := parseCacheControl(strings.Join(header.Values("Cache-Control"), ", "))
	return hasDirective(directives, "must-revalidate") || hasDirective(directives, "proxy-revalidate")
}

// currentAge estimates RFC 7234 section 4.2.3 current_age at now: the greater
// of the Age header value and the apparent age from a stale Date header.
// Without a fetch-start timestamp the transit-time correction is
// unavailable, so this is a lower bound on age; the residual (transit time,
// typically milliseconds) is documented imprecision, not a policy hole.
func currentAge(header http.Header, now time.Time) time.Duration {
	var age time.Duration
	if header == nil {
		return 0
	}
	if raw := strings.TrimSpace(header.Get("Age")); raw != "" {
		if secs, valid := parseDeltaSeconds(raw); valid {
			// Saturate an overflowing Age to the maximum duration: the
			// response is ancient, so any freshness subtraction refuses
			// the store below.
			if duration, ok := secondsToDuration(secs); ok {
				age = duration
			} else {
				age = time.Duration(math.MaxInt64)
			}
		}
	}
	if dateRaw := strings.TrimSpace(header.Get("Date")); dateRaw != "" {
		if date, err := http.ParseTime(dateRaw); err == nil {
			if apparent := now.Sub(date); apparent > age {
				age = apparent
			}
		}
	}
	return age
}

// freshnessFrom resolves repeated delta-seconds directives fail-safe: every
// occurrence must parse and agree, otherwise the store is refused.
func freshnessFrom(values []string) (time.Duration, bool) {
	var secs int64
	seen := false
	for _, raw := range values {
		value, valid := parseDeltaSeconds(raw)
		if !valid {
			return 0, false
		}
		if !seen {
			secs, seen = value, true
			continue
		}
		if value != secs {
			return 0, false
		}
	}
	if !seen {
		return 0, false
	}
	return secondsToDuration(secs)
}

// maxDeltaSeconds is the largest delta-seconds value representable as a
// time.Duration. Larger values would wrap the nanosecond multiplication
// (sometimes staying positive), so they are refused or saturated instead.
const maxDeltaSeconds = int64(math.MaxInt64 / int64(time.Second))

// secondsToDuration converts delta-seconds fail-safe.
func secondsToDuration(secs int64) (time.Duration, bool) {
	if secs < 0 || secs > maxDeltaSeconds {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// parseCacheControl splits Cache-Control values into lowercase directive
// names with every occurrence's unquoted argument preserved in order, so
// callers can detect conflicting repeats.
func parseCacheControl(values ...string) map[string][]string {
	directives := make(map[string][]string)
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, argument, _ := strings.Cut(part, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			argument = strings.TrimSpace(argument)
			if len(argument) >= 2 && strings.HasPrefix(argument, "\"") && strings.HasSuffix(argument, "\"") {
				argument = argument[1 : len(argument)-1]
			}
			directives[name] = append(directives[name], argument)
		}
	}
	return directives
}

// hasDirective reports whether a directive occurs at least once.
func hasDirective(directives map[string][]string, name string) bool {
	values, ok := directives[name]
	return ok && len(values) > 0
}

// parseDeltaSeconds parses an HTTP delta-seconds value: one or more ASCII
// digits. Anything else (empty, signs, garbage, overflow) is invalid.
func parseDeltaSeconds(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	secs, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return secs, true
}

// singleflightKey returns the coalescing key for one upstream fetch: the
// cache fingerprint plus a digest of the request headers the fingerprint
// does not cover. Upstream Vary is unknown before the fetch, so any header
// difference splits the flight; sharing one upstream response between
// header-differing requests would leak one variant's body to the other,
// bypassing the Vary check that only runs on cache hits.
func (g *Gateway) singleflightKey(r *http.Request, target *url.URL) string {
	return g.requestKey(r, target) + "|" + headerVariantDigest(r)
}

// headerVariantDigest hashes the request headers that can select an upstream
// variant but are not part of the cache fingerprint. Hop-by-hop headers are
// stripped before sending and cannot affect the response; Host is
// synchronized to the target; Content-Length is framing; X-Request-ID is our
// own per-request unique value (responses varying on it are never cached;
// see varyIncludesRequestID). Everything else, including Cookie and
// User-Agent, splits the flight in the safe direction.
func headerVariantDigest(r *http.Request) string {
	if r == nil {
		return ""
	}
	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		lower := strings.ToLower(name)
		if _, skip := hopByHopHeadersMap[lower]; skip {
			continue
		}
		switch lower {
		case "host", "content-length", "x-request-id":
			continue
		}
		names = append(names, lower)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		builder.WriteString(name)
		builder.WriteByte(':')
		for _, value := range r.Header.Values(name) {
			builder.WriteString(strconv.Itoa(len(value)))
			builder.WriteByte(':')
			builder.WriteString(value)
			builder.WriteByte('|')
		}
		builder.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// varyIncludesRequestID reports whether the response varies on X-Request-ID.
// Request IDs are excluded from the singleflight digest to preserve
// coalescing (every generated ID is unique), so such responses are never
// cached: concurrent flights would otherwise share the leader's variant
// while the follower's own ID never reaches upstream.
func varyIncludesRequestID(fields []string) bool {
	for _, field := range fields {
		if field == "x-request-id" {
			return true
		}
	}
	return false
}

// parseVaryFields returns the normalized response Vary field names with
// duplicates removed and sorted. star reports "Vary: *", which must never be
// cached because the response varies on unspecified request aspects.
func parseVaryFields(header http.Header) (fields []string, star bool) {
	if header == nil {
		return nil, false
	}
	seen := make(map[string]struct{})
	for _, value := range header.Values("Vary") {
		for _, part := range strings.Split(value, ",") {
			field := strings.ToLower(strings.TrimSpace(part))
			if field == "" {
				continue
			}
			if field == "*" {
				return nil, true
			}
			if _, exists := seen[field]; exists {
				continue
			}
			seen[field] = struct{}{}
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	return fields, false
}

// varyRequestSnapshot records the current request values for fields. Values
// are joined with a control-character separator that cannot appear in HTTP
// field values, so comparison is exact without parsing structured headers.
func varyRequestSnapshot(r *http.Request, fields []string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	snapshot := make(map[string]string, len(fields))
	for _, field := range fields {
		snapshot[field] = requestHeaderValue(r, field)
	}
	return snapshot
}

func requestHeaderValue(r *http.Request, field string) string {
	if r == nil {
		return ""
	}
	return strings.Join(r.Header.Values(field), "\x1f")
}

// cacheVaryMatches reports whether a cached entry may serve r. Legacy entries
// without Vary metadata never match, so rows written before Vary tracking
// degrade to safe misses. Entries stored for responses without Vary always
// match.
func cacheVaryMatches(entry storage.CacheEntry, r *http.Request) bool {
	if !entry.VaryKnown() {
		return false
	}
	vary := entry.Vary
	if len(vary) == 0 {
		return true
	}
	for field, value := range vary {
		if requestHeaderValue(r, field) != value {
			return false
		}
	}
	return true
}
