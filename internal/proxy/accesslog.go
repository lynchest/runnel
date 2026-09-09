package proxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// gatewayRequestIDKey carries the validated request ID through contexts.
type gatewayRequestIDKey struct{}

// maxRequestIDLen bounds accepted inbound X-Request-ID values.
const maxRequestIDLen = 128

var requestIDFallback atomic.Uint64

// validatedRequestID returns the inbound ID when it is safe to reuse:
// 1-128 chars of letters, digits, dot, dash, or underscore. Anything else
// yields "" so the caller generates a fresh value.
func validatedRequestID(inbound string) string {
	if len(inbound) == 0 || len(inbound) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(inbound); i++ {
		c := inbound[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '_' {
			continue
		}
		return ""
	}
	return inbound
}

// statusClass maps a response status to a short error class for access logs.
// Success carries no class; unwritten responses mean the client went away.
func statusClass(status int) string {
	switch status {
	case 0:
		return "client_canceled"
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusRequestEntityTooLarge:
		return "body_too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusServiceUnavailable:
		return "unavailable"
	case http.StatusGatewayTimeout:
		return "gateway_timeout"
	default:
		if status >= 500 {
			return "server_error"
		}
		if status >= 400 {
			return "client_error"
		}
		return ""
	}
}

// newRequestID generates a random 128-bit hex identifier. A monotonic
// fallback covers the practically impossible CSPRNG failure without blocking.
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return "fallback-" + strconv.FormatUint(requestIDFallback.Add(1), 16) +
		"-" + strconv.FormatInt(time.Now().UnixNano(), 16)
}

// requestIDFromContext retrieves the validated request ID carried by ctx.
func requestIDFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	id, ok := ctx.Value(gatewayRequestIDKey{}).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// sanitizeLogToken keeps one access-log field on a single line without
// control characters or spaces. Out-of-alphabet bytes become '?'.
func sanitizeLogToken(value string) string {
	if value == "" {
		return "-"
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '_' {
			builder.WriteByte(c)
		} else {
			builder.WriteByte('?')
		}
	}
	return builder.String()
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status before delegating.
func (w *statusRecorder) WriteHeader(status int) {
	if w != nil && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write records an implicit 200 before delegating.
func (w *statusRecorder) Write(data []byte) (int, error) {
	if w != nil && w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

// logProxyAccess writes one structured line per /proxy request. Only the
// domain is logged from the target: never the full URL, query values,
// headers, or bodies, so credentials cannot leak through logs.
func (g *Gateway) logProxyAccess(method, domain string, status int, duration time.Duration, cacheResult, errClass, requestID string) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger.Printf("proxy method=%s domain=%s status=%d duration_ms=%d cache=%s err=%s request_id=%s",
		sanitizeLogToken(method),
		sanitizeLogToken(domain),
		status,
		duration.Milliseconds(),
		sanitizeLogToken(cacheResult),
		sanitizeLogToken(errClass),
		sanitizeLogToken(requestID),
	)
}

// upstreamErrClass annotates a fetch error with a short machine-readable
// class for access logs. It unwraps to the original error so errors.Is/As
// matching (context deadlines, body limits) keeps working. Only the class
// is ever logged, never the message, because transport errors can embed
// target addresses.
type upstreamErrClass struct {
	class string
	err   error
}

func (e *upstreamErrClass) Error() string {
	if e == nil || e.err == nil {
		return "upstream error"
	}
	return e.err.Error()
}

func (e *upstreamErrClass) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// withUpstreamClass wraps err for log classification.
func withUpstreamClass(err error, class string) error {
	if err == nil || class == "" {
		return err
	}
	return &upstreamErrClass{class: class, err: err}
}

// upstreamClassOf extracts the annotated class from err.
func upstreamClassOf(err error) (string, bool) {
	var annotated *upstreamErrClass
	if errors.As(err, &annotated) && annotated != nil && annotated.class != "" {
		return annotated.class, true
	}
	return "", false
}

// classifyTransportError maps a client.Do failure to a log class. DNS is
// checked before timeout because DNS timeouts are more usefully reported as
// DNS failures. TLS matches typed x509 failures, the record-header probe
// error, and the "tls:"/"x509:" message families, whose concrete types are
// unexported.
func classifyTransportError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	if os.IsTimeout(err) {
		return "timeout"
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return "tls_error"
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return "tls_error"
	}
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &invalidErr) {
		return "tls_error"
	}
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return "tls_error"
	}
	message := err.Error()
	if strings.HasPrefix(message, "tls:") || strings.HasPrefix(message, "x509:") {
		return "tls_error"
	}
	return "upstream_error"
}
