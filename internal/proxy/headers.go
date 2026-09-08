package proxy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// RFC 7230 section 6.1 names the fixed hop-by-hop fields.  Trailer and
// Trailers are both included because net/http and older upstreams use both
// spellings in practice.
var hopByHopHeaders = [...]string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// RemoveHopByHopHeaders removes RFC hop-by-hop headers from h.  It also
// removes every header named by a Connection token, as required by RFC 7230;
// token matching is case-insensitive and accepts comma-separated values.
func RemoveHopByHopHeaders(h http.Header) {
	if h == nil {
		return
	}

	connectionTokens := make(map[string]struct{})
	for key, values := range h {
		if !strings.EqualFold(key, "Connection") {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				token = strings.TrimSpace(token)
				if token != "" {
					connectionTokens[strings.ToLower(token)] = struct{}{}
				}
			}
		}
	}

	for key := range h {
		lowerKey := strings.ToLower(key)
		remove := false
		for _, standard := range hopByHopHeaders {
			if lowerKey == strings.ToLower(standard) {
				remove = true
				break
			}
		}
		if !remove {
			_, remove = connectionTokens[lowerKey]
		}
		if remove {
			delete(h, key)
		}
	}
}

// StripHopByHopRequest removes hop-by-hop request headers and transport-only
// fields from req.
func StripHopByHopRequest(req *http.Request) {
	if req == nil {
		return
	}
	RemoveHopByHopHeaders(req.Header)
	req.TransferEncoding = nil
	if req.Trailer != nil {
		req.Trailer = nil
	}
}

// StripHopByHopResponse removes hop-by-hop response headers and transport-only
// fields from resp.
func StripHopByHopResponse(resp *http.Response) {
	if resp == nil {
		return
	}
	RemoveHopByHopHeaders(resp.Header)
	resp.TransferEncoding = nil
	if resp.Trailer != nil {
		resp.Trailer = nil
	}
}

const (
	corsAllowOrigin  = "*"
	corsAllowMethods = "GET, POST, PUT, DELETE, OPTIONS"
	corsAllowHeaders = "Authorization, Content-Type, Accept-Language, Cookie"
	corsMaxAge       = "86400"
)

// WriteCORSPreflight responds to an OPTIONS request without contacting an
// upstream.  It returns true when it wrote the response and false for other
// methods, allowing it to be used as a small middleware primitive.
func WriteCORSPreflight(w http.ResponseWriter, r *http.Request) bool {
	if w == nil || r == nil || !strings.EqualFold(r.Method, http.MethodOptions) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
	w.Header().Set("Access-Control-Max-Age", corsMaxAge)
	w.WriteHeader(http.StatusNoContent)
	return true
}

// CORSPreflightHandler returns a handler that answers preflight requests.
// Non-OPTIONS requests receive 405, making accidental direct use visible.
func CORSPreflightHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if WriteCORSPreflight(w, r) {
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

// WithCORSPreflight wraps next and short-circuits OPTIONS requests.  It does
// not add permissive CORS headers to ordinary responses; callers can choose
// that policy separately.
func WithCORSPreflight(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if WriteCORSPreflight(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SynchronizeOutboundHost sets the outbound request's URL and Host field to
// target's authority.  Keeping URL.Host and Request.Host together preserves
// virtual-host routing while the transport dials a pinned IP; TLS SNI still
// comes from the URL hostname in the standard net/http transport.
func SynchronizeOutboundHost(req *http.Request, target *url.URL) error {
	if req == nil {
		return errors.New("request is nil")
	}
	if target == nil || target.Host == "" {
		return errors.New("target URL has no host")
	}
	req.Host = target.Host
	if req.URL != nil {
		if target.Scheme != "" {
			req.URL.Scheme = target.Scheme
		}
		req.URL.Host = target.Host
	}
	return nil
}

// SynchronizeTargetHost applies Host synchronization from a validated target.
func SynchronizeTargetHost(req *http.Request, target *ValidatedTarget) error {
	if target == nil {
		return errors.New("validated target is nil")
	}
	return SynchronizeOutboundHost(req, target.URL)
}
