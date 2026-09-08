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
	clone.Scheme = strings.ToLower(clone.Scheme)
	clone.User = nil
	clone.Host = canonicalAuthority(clone.Scheme, clone.Host)
	clone.RawQuery = ""
	clone.ForceQuery = false
	clone.Fragment = ""
	clone.RawFragment = ""
	return clone.String()
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
	method, canonicalURL, query, language := requestFingerprintFields(r)
	var names []string
	if len(authCookieNames) > 0 {
		names = authCookieNames[0]
	}
	if names == nil {
		names = defaultAuthCookieNames
	}
	authHash := authFingerprintHash(r, names)
	input := joinFingerprintFields(method, canonicalURL, query, language, authHash)
	return sha256.Sum256([]byte(input))
}

// Fingerprint returns ComputeFingerprint encoded as lowercase hexadecimal.
// With no cookie list, the default selected auth-cookie set is used; one list
// customizes it.
func Fingerprint(r *http.Request, authCookieNames ...[]string) string {
	var names []string
	if len(authCookieNames) > 0 {
		names = authCookieNames[0]
	}
	digest := ComputeFingerprint(r, names)
	return hex.EncodeToString(digest[:])
}

// AuthHash returns the digest of Authorization and selected cookie values.
// It is safe for logging and cache-key composition because no credential value
// is included in the output.
func AuthHash(r *http.Request, cookieNames ...string) string {
	names := cookieNames
	if names == nil {
		names = defaultAuthCookieNames
	}
	return authFingerprintHash(r, names)
}

func requestFingerprintFields(r *http.Request) (string, string, string, string) {
	if r == nil {
		return "", "", "", ""
	}
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if r.URL == nil {
		return method, "", "", normalizeLanguage(r.Header.Values("Accept-Language"))
	}
	return method, CanonicalURL(r.URL), CanonicalQuery(r.URL), normalizeLanguage(r.Header.Values("Accept-Language"))
}

func authFingerprintHash(r *http.Request, cookieNames []string) string {
	if r == nil {
		digest := sha256.Sum256(nil)
		return hex.EncodeToString(digest[:])
	}

	authorization := strings.Join(r.Header.Values("Authorization"), "\x00")
	authDigest := sha256.Sum256([]byte(authorization))
	parts := []string{"authorization=" + hex.EncodeToString(authDigest[:])}

	// Normalize and sort names so config ordering cannot create cache-key
	// aliases.  Duplicate names are collapsed case-insensitively.
	names := normalizeCookieNames(cookieNames)
	cookies := make(map[string][]string)
	for _, cookie := range r.Cookies() {
		key := strings.ToLower(cookie.Name)
		cookies[key] = append(cookies[key], cookie.Value)
	}
	for _, name := range names {
		values := append([]string(nil), cookies[strings.ToLower(name)]...)
		sort.Strings(values)
		if len(values) == 0 {
			parts = append(parts, name+"=<absent>")
			continue
		}
		for _, value := range values {
			digest := sha256.Sum256([]byte(value))
			parts = append(parts, name+"="+hex.EncodeToString(digest[:]))
		}
	}

	combined := joinFingerprintFields(parts...)
	digest := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(digest[:])
}

func joinFingerprintFields(fields ...string) string {
	var builder strings.Builder
	for _, field := range fields {
		builder.WriteString(strconv.Itoa(len(field)))
		builder.WriteByte(':')
		builder.WriteString(field)
		builder.WriteByte('|')
	}
	return builder.String()
}

func canonicalAuthority(scheme, authority string) string {
	if authority == "" {
		return ""
	}
	parsed, err := url.Parse("//" + authority)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(authority)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return strings.ToLower(authority)
	}
	host = strings.TrimSuffix(host, ".")
	port := parsed.Port()
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

func normalizeLanguage(values []string) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.Join(strings.Fields(value), " ")
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, ",")
}

func normalizeCookieNames(names []string) []string {
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
