package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// defaultAuthCookieNames are common session/authentication cookie names.  A
// domain-specific list should normally be supplied to Fingerprint;
// these defaults keep an unconfigured gateway from sharing authenticated and
// anonymous responses.
var defaultAuthCookieNames = []string{
	"steamLoginSecure",
	"sessionid",
	"access_token",
	"auth_token",
	"refresh_token",
	"connect.sid",
}

// defaultNormalizedAuthCookieNames pre-normalizes default auth cookie names once
// at package initialization to eliminate map allocations and sorting per request.
var defaultNormalizedAuthCookieNames = normalizeCookieNames(defaultAuthCookieNames)

// CanonicalQuery returns a deterministic query encoding.  Keys and values are
// sorted, duplicate keys are preserved, and URL encoding follows
// url.Values.Encode.  Empty queries remain empty.
func CanonicalQuery(u *url.URL) string {
	if u == nil || u.RawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		// Keep malformed bytes in the key instead of silently collapsing them
		// with an empty query.  Sorting raw pairs still makes ordering stable for
		// otherwise equivalent malformed inputs.
		return canonicalRawQuery(u.RawQuery)
	}
	for key := range values {
		sort.Strings(values[key])
	}
	return values.Encode()
}

// CanonicalURL returns scheme, host, path, and escaped user-visible URL
// components in canonical form, excluding query and fragment.  Query data is
// exposed separately by CanonicalQuery so callers can inspect the exact cache
// key inputs without ambiguity.
func CanonicalURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clone := *u
	// Optimization: Skip strings.ToLower allocation when scheme is already lowercase.
	if containsUpper(clone.Scheme) {
		clone.Scheme = strings.ToLower(clone.Scheme)
	}
	clone.User = nil
	clone.Host = canonicalAuthority(clone.Scheme, clone.Host)
	clone.RawQuery = ""
	clone.ForceQuery = false
	clone.Fragment = ""
	clone.RawFragment = ""
	return clone.String()
}

func containsUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// CanonicalTargetURL is a string helper for callers that have not parsed a
// URL yet.  Invalid URLs are represented by the original string, while normal
// HTTP request paths should use CanonicalURL directly for error handling.
func CanonicalTargetURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return CanonicalURL(u)
}

// ComputeFingerprint calculates the binary SHA-256 fingerprint.  The input
// fields are length-delimited to avoid concatenation ambiguity:
//
//	method | canonical URL | canonical query | Accept-Language | auth hash
//
// Authorization and selected cookie values are represented only by their
// SHA-256 digests, so secrets never appear in the returned value or in the
// intermediate string passed to SHA-256.
func ComputeFingerprint(r *http.Request, authCookieNames ...[]string) [32]byte {
	return ComputeFingerprintWithTarget(r, nil, authCookieNames...)
}

// ComputeFingerprintWithTarget calculates the SHA-256 fingerprint using target as the URL if provided.
// This allows computing fingerprints without cloning http.Request to set URL.
func ComputeFingerprintWithTarget(r *http.Request, target *url.URL, authCookieNames ...[]string) [32]byte {
	method, canonicalURL, query, language := requestFingerprintFieldsWithTarget(r, target)
	var names []string
	if len(authCookieNames) > 0 {
		names = authCookieNames[0]
	}
	authHash := authFingerprintHash(r, names)
	input := joinFingerprintFields(method, canonicalURL, query, language, authHash)
	return sha256.Sum256([]byte(input))
}

// Fingerprint returns ComputeFingerprint encoded as lowercase hexadecimal.
// With no cookie list, the default selected auth-cookie set is used; one list
// customizes it.
func Fingerprint(r *http.Request, authCookieNames ...[]string) string {
	return FingerprintWithTarget(r, nil, authCookieNames...)
}

// FingerprintWithTarget returns Fingerprint encoded as lowercase hexadecimal using target URL if provided.
func FingerprintWithTarget(r *http.Request, target *url.URL, authCookieNames ...[]string) string {
	var names []string
	if len(authCookieNames) > 0 {
		names = authCookieNames[0]
	}
	digest := ComputeFingerprintWithTarget(r, target, names)
	return hex.EncodeToString(digest[:])
}

// AuthHash returns the digest of Authorization and selected cookie values.
// It is safe for logging and cache-key composition because no credential value
// is included in the output.
func AuthHash(r *http.Request, cookieNames ...string) string {
	return authFingerprintHash(r, cookieNames)
}

func requestFingerprintFieldsWithTarget(r *http.Request, target *url.URL) (string, string, string, string) {
	if r == nil {
		return "", "", "", ""
	}
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	u := target
	if u == nil {
		u = r.URL
	}
	if u == nil {
		return method, "", "", normalizeLanguage(r.Header.Values("Accept-Language"))
	}
	return method, CanonicalURL(u), CanonicalQuery(u), normalizeLanguage(r.Header.Values("Accept-Language"))
}

func authFingerprintHash(r *http.Request, cookieNames []string) string {
	// Optimization: Stack-allocate fixed 64-byte buffer for hex encoding digests.
	var hexBuf [64]byte
	if r == nil {
		digest := sha256.Sum256(nil)
		hex.Encode(hexBuf[:], digest[:])
		return string(hexBuf[:])
	}

	// Optimization: Avoid strings.Join allocation when 0 or 1 Authorization header exists.
	var authDigest [32]byte
	auths := r.Header.Values("Authorization")
	if len(auths) == 0 {
		authDigest = sha256.Sum256(nil)
	} else if len(auths) == 1 {
		authDigest = sha256.Sum256([]byte(auths[0]))
	} else {
		authorization := strings.Join(auths, "\x00")
		authDigest = sha256.Sum256([]byte(authorization))
	}

	names := cookieNames
	if names == nil {
		names = defaultNormalizedAuthCookieNames
	} else {
		names = normalizeCookieNames(names)
	}

	// Pre-allocate parts slice: 1 for authorization + 1 per cookie name.
	parts := make([]string, 0, 1+len(names))
	hex.Encode(hexBuf[:], authDigest[:])
	parts = append(parts, "authorization="+string(hexBuf[:]))

	cookieHeaders := r.Header["Cookie"]
	// Optimization: Bypass cookie parsing entirely when no Cookie header is present.
	if len(cookieHeaders) == 0 {
		for _, name := range names {
			parts = append(parts, name+"=<absent>")
		}
	} else {
		// Optimization: Parse cookie lines directly to avoid r.Cookies() heap allocations.
		cookies := make(map[string][]string)
		for _, line := range cookieHeaders {
			for len(line) > 0 {
				var part string
				if i := strings.IndexByte(line, ';'); i >= 0 {
					part, line = line[:i], line[i+1:]
				} else {
					part, line = line, ""
				}
				part = strings.TrimSpace(part)
				if len(part) == 0 {
					continue
				}
				name, val, ok := strings.Cut(part, "=")
				if !ok {
					continue
				}
				name = strings.TrimSpace(name)
				val = strings.TrimSpace(val)
				if len(val) > 1 && val[0] == '"' && val[len(val)-1] == '"' {
					val = val[1 : len(val)-1]
				}
				key := strings.ToLower(name)
				cookies[key] = append(cookies[key], val)
			}
		}
		for _, name := range names {
			vals := cookies[strings.ToLower(name)]
			if len(vals) == 0 {
				parts = append(parts, name+"=<absent>")
				continue
			}
			values := append([]string(nil), vals...)
			sort.Strings(values)
			for _, value := range values {
				digest := sha256.Sum256([]byte(value))
				hex.Encode(hexBuf[:], digest[:])
				parts = append(parts, name+"="+string(hexBuf[:]))
			}
		}
	}

	combined := joinFingerprintFields(parts...)
	digest := sha256.Sum256([]byte(combined))
	hex.Encode(hexBuf[:], digest[:])
	return string(hexBuf[:])
}

// joinFingerprintFields formats length-delimited fields into a single builder string.
// Optimization: Buffer capacity is pre-calculated to avoid strings.Builder reallocations.
func joinFingerprintFields(fields ...string) string {
	totalLen := 0
	for _, field := range fields {
		totalLen += lenIntStr(len(field)) + 1 + len(field) + 1
	}
	var builder strings.Builder
	builder.Grow(totalLen)
	for _, field := range fields {
		builder.WriteString(strconv.Itoa(len(field)))
		builder.WriteByte(':')
		builder.WriteString(field)
		builder.WriteByte('|')
	}
	return builder.String()
}

func lenIntStr(n int) int {
	if n < 10 {
		return 1
	}
	if n < 100 {
		return 2
	}
	if n < 1000 {
		return 3
	}
	if n < 10000 {
		return 4
	}
	return len(strconv.Itoa(n))
}

// canonicalAuthority returns standard lowercased authority without default ports (80/443).
// Optimization: Uses string slicing instead of url.Parse("//" + authority) to avoid heap allocation.
func canonicalAuthority(scheme, authority string) string {
	if authority == "" {
		return ""
	}
	host, port := parseAuthority(authority)
	if port != "" && !isDigits(port) {
		return strings.ToLower(authority)
	}
	host = strings.ToLower(host)
	if host == "" {
		return strings.ToLower(authority)
	}
	host = strings.TrimSuffix(host, ".")
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		if strings.Contains(host, ":") {
			return "[" + host + "]:" + port
		}
		return host + ":" + port
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func parseAuthority(authority string) (host, port string) {
	if strings.HasPrefix(authority, "[") {
		i := strings.LastIndexByte(authority, ']')
		if i < 0 {
			return authority, ""
		}
		host = authority[1:i]
		if len(authority) > i+1 && authority[i+1] == ':' {
			port = authority[i+2:]
		}
		return host, port
	}
	if i := strings.LastIndexByte(authority, ':'); i >= 0 {
		return authority[:i], authority[i+1:]
	}
	return authority, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func normalizeLanguage(values []string) string {
	if len(values) == 0 {
		return ""
	}
	var b strings.Builder
	for _, value := range values {
		v := compactWhitespace(value)
		if v != "" {
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			b.WriteString(v)
		}
	}
	return b.String()
}

// compactWhitespace collapses internal whitespace in s.
// Optimization: Fast-path returns s unchanged if no whitespace is found, eliminating strings.Fields allocation.
func compactWhitespace(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || !containsWhitespace(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteByte(c)
			inSpace = false
		}
	}
	return b.String()
}

func containsWhitespace(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return true
		}
	}
	return false
}

func normalizeCookieNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return strings.ToLower(result[i]) < strings.ToLower(result[j])
	})
	return result
}

func canonicalRawQuery(rawQuery string) string {
	pairs := strings.Split(rawQuery, "&")
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}
