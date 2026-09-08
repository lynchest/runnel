package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Resolver is the part of net.Resolver needed by the SSRF guard.  Keeping the
// dependency as an interface makes DNS behavior deterministic in tests and
// lets callers provide a resolver with the desired timeout or DNS policy.
type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(context.Context, string) ([]net.IPAddr, error)

// LookupIPAddr implements Resolver.
func (f ResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

var (
	// ErrInvalidTarget identifies a malformed or unsupported target URL.
	ErrInvalidTarget = errors.New("invalid upstream target")
	// ErrSSRFBlocked identifies a target whose address is not safe to contact.
	ErrSSRFBlocked = errors.New("upstream target blocked by SSRF policy")
	// ErrDomainNotAllowed identifies a target outside the configured allowlist.
	ErrDomainNotAllowed = errors.New("upstream domain is not allowed")
	// ErrTargetResolution identifies a target that could not be resolved.
	ErrTargetResolution = errors.New("upstream target could not be resolved")
)

// ValidationError carries a stable reason and the HTTP status a gateway can
// use when turning a validation failure into a response.
type ValidationError struct {
	Kind   error
	Target string
	Err    error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("%v: %s", e.Kind, e.Target)
	}
	return fmt.Sprintf("%v: %s: %v", e.Kind, e.Target, e.Err)
}

func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

// StatusCode returns the most useful HTTP status for an upstream target
// validation error.  Private, link-local, multicast, unspecified, and
// disallowed targets are forbidden; malformed URLs are bad requests.
func (e *ValidationError) StatusCode() int {
	if e == nil {
		return http.StatusBadRequest
	}
	if errors.Is(e.Kind, ErrSSRFBlocked) || errors.Is(e.Kind, ErrDomainNotAllowed) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

// ValidatedTarget is the result of validating a URL and resolving its host.
// IPs contains only addresses that passed the SSRF policy.  A caller should
// retain this value and use DialContext (or a transport made by
// NewSafeTransport) so a second DNS lookup cannot redirect the connection to
// a different address.
type ValidatedTarget struct {
	URL             *url.URL
	Host            string
	Port            string
	IP              net.IP
	IPs             []net.IP
	AllowPrivateIPs bool
}

// Hostname returns the validated hostname without its port.
func (t *ValidatedTarget) Hostname() string {
	if t == nil {
		return ""
	}
	if t.URL != nil {
		return t.URL.Hostname()
	}
	host, _, err := net.SplitHostPort(t.Host)
	if err == nil {
		return host
	}
	return strings.TrimSuffix(t.Host, ".")
}

// Address returns the address of the first validated IP and the target port.
func (t *ValidatedTarget) Address() string {
	if t == nil {
		return ""
	}
	port := t.Port
	if port == "" {
		port = defaultPortForScheme(t.URL)
	}
	if t.IP != nil {
		return net.JoinHostPort(t.IP.String(), port)
	}
	return net.JoinHostPort(t.Hostname(), port)
}

// URLValidator validates HTTP(S) targets, resolves names once, and retains
// the resulting addresses for pinned dialing.
type URLValidator struct {
	Resolver        Resolver
	AllowedDomains  []string
	AllowPrivateIPs bool
}

// NewURLValidator creates a validator.  A nil resolver uses net.DefaultResolver.
// The allowlist is copied so callers may safely reuse and later modify their
// input slice.
func NewURLValidator(resolver Resolver, allowedDomains []string) *URLValidator {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &URLValidator{
		Resolver:       resolver,
		AllowedDomains: append([]string(nil), allowedDomains...),
	}
}

// Validate parses and validates an absolute HTTP(S) URL.
func (v *URLValidator) Validate(ctx context.Context, rawURL string) (*ValidatedTarget, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, validationError(ErrInvalidTarget, "<invalid target>", err)
	}
	return v.ValidateURL(ctx, u)
}

// ValidateURL is the package-level convenience form of URLValidator.Validate.
func ValidateURL(ctx context.Context, rawURL string, resolver Resolver, allowedDomains []string) (*ValidatedTarget, error) {
	return NewURLValidator(resolver, allowedDomains).Validate(ctx, rawURL)
}

// ValidateURL validates an already parsed absolute HTTP(S) URL.
func (v *URLValidator) ValidateURL(ctx context.Context, u *url.URL) (*ValidatedTarget, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if u == nil {
		return nil, validationError(ErrInvalidTarget, "", errors.New("URL is nil"))
	}
	displayURL := redactedURL(u)

	clone := *u
	if !isHTTPOrHTTPS(clone.Scheme) {
		return nil, validationError(ErrInvalidTarget, displayURL, fmt.Errorf("scheme %q is not allowed", clone.Scheme))
	}
	if clone.Host == "" || clone.Hostname() == "" {
		return nil, validationError(ErrInvalidTarget, displayURL, errors.New("host is required"))
	}
	if clone.User != nil {
		// Userinfo is not sent by the gateway and accepting it makes URL review
		// needlessly error-prone.  It can also hide the actual host from humans.
		return nil, validationError(ErrInvalidTarget, displayURL, errors.New("userinfo is not allowed"))
	}
	if strings.Contains(clone.Hostname(), "%") {
		// Zone identifiers are meaningful only on a local interface and should
		// never be accepted as a public upstream target.
		return nil, validationError(ErrSSRFBlocked, displayURL, errors.New("IPv6 zone identifiers are not allowed"))
	}
	if _, err := portForURL(&clone); err != nil {
		return nil, validationError(ErrInvalidTarget, displayURL, err)
	}

	host := normalizeHostname(clone.Hostname())
	if !IsAllowedDomain(host, v.allowedDomains()) {
		return nil, validationError(ErrDomainNotAllowed, displayURL, nil)
	}

	addresses, err := v.resolve(ctx, host)
	if err != nil {
		return nil, validationError(ErrTargetResolution, displayURL, err)
	}
	if len(addresses) == 0 {
		return nil, validationError(ErrTargetResolution, displayURL, errors.New("resolver returned no addresses"))
	}

	validated := make([]net.IP, 0, len(addresses))
	for _, ip := range addresses {
		ip = canonicalIP(ip)
		if ip == nil {
			return nil, validationError(ErrTargetResolution, displayURL, errors.New("resolver returned an invalid address"))
		}
		if !v.AllowPrivateIPs && IsBlockedIP(ip) {
			return nil, validationError(ErrSSRFBlocked, displayURL, fmt.Errorf("resolved address %s is not allowed", ip))
		}
		validated = appendUniqueIP(validated, ip)
	}
	if len(validated) == 0 {
		return nil, validationError(ErrTargetResolution, displayURL, errors.New("resolver returned no usable addresses"))
	}

	clone.Host = normalizedHostWithPort(&clone, host)
	clone.RawPath = ""
	clone.ForceQuery = u.ForceQuery
	port, _ := portForURL(&clone)
	return &ValidatedTarget{
		URL:             &clone,
		Host:            clone.Host,
		Port:            port,
		IP:              append(net.IP(nil), validated[0]...),
		IPs:             cloneIPs(validated),
		AllowPrivateIPs: v.AllowPrivateIPs,
	}, nil
}

// ValidateRedirect validates a request URL and returns the pinned target for
// that hop.  The request URL is not rewritten.
func (v *URLValidator) ValidateRedirect(req *http.Request) (*ValidatedTarget, error) {
	if req == nil {
		return nil, validationError(ErrInvalidTarget, "", errors.New("request is nil"))
	}
	return v.ValidateURL(req.Context(), req.URL)
}

// CheckRedirect returns a net/http-compatible redirect policy.  Every hop is
// resolved and checked independently, then its validated IP is attached to the
// request context for the transport's DialContext.  No Location rewriting is
// performed here; redirect presentation belongs to the gateway layer.
func (v *URLValidator) CheckRedirect(req *http.Request, _ []*http.Request) error {
	target, err := v.ValidateRedirect(req)
	if err != nil {
		return err
	}
	return AttachValidatedTarget(req, target)
}

// AttachValidatedTarget associates a validated target with a request.  The
// request's URL is left intact while Host is synchronized for outbound HTTP.
func AttachValidatedTarget(req *http.Request, target *ValidatedTarget) error {
	if req == nil {
		return errors.New("request is nil")
	}
	if target == nil || target.URL == nil || len(target.IPs) == 0 {
		return errors.New("validated target is nil or incomplete")
	}
	if err := SynchronizeOutboundHost(req, target.URL); err != nil {
		return err
	}
	*req = *req.WithContext(context.WithValue(req.Context(), validatedTargetContextKey{}, target))
	return nil
}

// WithValidatedTarget returns a context carrying a pinned target.  It is
// useful when a caller builds requests without using AttachValidatedTarget.
func WithValidatedTarget(ctx context.Context, target *ValidatedTarget) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, validatedTargetContextKey{}, target)
}

// ValidatedTargetFromContext retrieves a target attached by
// WithValidatedTarget or AttachValidatedTarget.
func ValidatedTargetFromContext(ctx context.Context) (*ValidatedTarget, bool) {
	if ctx == nil {
		return nil, false
	}
	target, ok := ctx.Value(validatedTargetContextKey{}).(*ValidatedTarget)
	return target, ok && target != nil
}

// DialContext pins a connection to the target attached to ctx.  If no target
// is attached, it validates and resolves the host from address exactly once
// before dialing it.  In both cases the dial never asks net.Dialer to resolve
// a hostname a second time.
func (v *URLValidator) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if target, ok := ValidatedTargetFromContext(ctx); ok {
		return target.dialContext(ctx, network, address)
	}

	host, port, err := splitDialAddress(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		validated, err := v.ValidateURL(ctx, &url.URL{Scheme: schemeForPort(port), Host: net.JoinHostPort(host, port)})
		if err != nil {
			return nil, err
		}
		return validated.dialContext(ctx, network, address)
	}
	validated, err := v.validateHost(ctx, host, port, schemeForPort(port))
	if err != nil {
		return nil, err
	}
	return validated.dialContext(ctx, network, address)
}

// PinnedDialContext returns a transport-compatible dial function bound to a
// single validated target.  It is the simplest safe option for a one-request
// client and cannot be affected by DNS rebinding.
func (v *URLValidator) PinnedDialContext(target *ValidatedTarget) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if target == nil {
			return nil, errors.New("validated target is nil")
		}
		return target.dialContext(ctx, network, address)
	}
}

// NewSafeTransport returns a transport whose proxy is disabled and whose
// DialContext validates/pins each request target supplied in its context.
// A caller should attach the initial target before sending and use
// CheckRedirect as the client's redirect policy so each hop gets a new pin.
func (v *URLValidator) NewSafeTransport(initial *ValidatedTarget) *http.Transport {
	transport := cloneDefaultTransport()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, ok := ValidatedTargetFromContext(ctx); ok || initial == nil {
			return v.DialContext(ctx, network, address)
		}
		host, _, err := splitDialAddress(address)
		if err == nil && sameHostname(host, initial.Hostname()) {
			return initial.dialContext(ctx, network, address)
		}
		// A caller may omit CheckRedirect. In that case do not reuse the
		// initial pin for a changed authority; validate the new dial address
		// independently so a redirect cannot bypass the guard.
		return v.DialContext(ctx, network, address)
	}
	return transport
}

func cloneDefaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{}
}

// IsBlockedIP reports whether ip is loopback, private, link-local, multicast,
// unspecified, or otherwise a non-global unicast address.  The explicit
// checks cover both IPv4 and IPv6, including IPv4-mapped IPv6 forms.
func IsBlockedIP(ip net.IP) bool {
	ip = canonicalIP(ip)
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if !ip.IsGlobalUnicast() {
		return true
	}
	// IPv4-compatible IPv6 addresses (::x.y.z.w) are deprecated but can still
	// be interpreted as local IPv4 destinations by operating systems. Treat
	// the whole ::/96 form as non-public rather than relying on net.IP.To4,
	// which intentionally does not classify this obsolete representation.
	if len(ip) == net.IPv6len && isAllZero(ip[:12]) {
		return true
	}
	// The entire IPv4 0/8 block is reserved for this network and should not be
	// treated as an external upstream even though only 0.0.0.0 is reported by
	// net.IP.IsUnspecified.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return true
	}
	// 100.64.0.0/10 is shared address space rather than a private range, but
	// it is not a routable public upstream target and is a common SSRF escape.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 0x40 {
		return true
	}
	return false
}

// IsAllowedDomain applies the configured domain allowlist.  An empty list
// allows any syntactically valid hostname.  Entries may be exact names or
// wildcard names such as "*.example.com"; wildcard matches require at least
// one subdomain label and respect label boundaries.
func IsAllowedDomain(host string, allowedDomains []string) bool {
	if len(allowedDomains) == 0 {
		return true
	}
	host = normalizeHostname(host)
	if host == "" {
		return false
	}
	for _, entry := range allowedDomains {
		pattern := normalizeAllowedDomain(entry)
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*.")
			if host != suffix && strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

type validatedTargetContextKey struct{}

func (v *URLValidator) allowedDomains() []string {
	if v == nil {
		return nil
	}
	return v.AllowedDomains
}

func (v *URLValidator) resolver() Resolver {
	if v == nil || v.Resolver == nil {
		return net.DefaultResolver
	}
	return v.Resolver
}

func (v *URLValidator) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addresses, err := v.resolver().LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.IP)
	}
	return result, nil
}

func (v *URLValidator) validateHost(ctx context.Context, host, port, scheme string) (*ValidatedTarget, error) {
	return v.ValidateURL(ctx, &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)})
}

func (t *ValidatedTarget) dialContext(ctx context.Context, network, requestedAddress string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil || len(t.IPs) == 0 {
		return nil, errors.New("validated target has no pinned addresses")
	}
	host, requestedPort, err := splitDialAddress(requestedAddress)
	if err != nil {
		return nil, err
	}
	if !sameHostname(host, t.Hostname()) {
		return nil, fmt.Errorf("dial host %q does not match validated target %q", host, t.Hostname())
	}
	port := requestedPort
	if port == "" {
		port = t.Port
	}
	if port == "" {
		port = defaultPortForScheme(t.URL)
	}
	if t.Port != "" && port != t.Port {
		return nil, fmt.Errorf("dial port %q does not match validated target port %q", port, t.Port)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("invalid dial port %q: %w", port, err)
	}

	dialer := net.Dialer{}
	var lastErr error
	for _, ip := range t.IPs {
		ip = canonicalIP(ip)
		if ip == nil || (!t.AllowPrivateIPs && IsBlockedIP(ip)) {
			lastErr = errors.New("validated target contains a blocked address")
			continue
		}
		if !ipMatchesNetwork(ip, network) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no pinned address supports network %q", network)
}

// DialContext is the transport-compatible pinned dial method on a validated
// target.
func (t *ValidatedTarget) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.dialContext(ctx, network, address)
}

func validationError(kind error, target string, err error) *ValidationError {
	return &ValidationError{Kind: kind, Target: target, Err: err}
}

func redactedURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clone := *u
	clone.User = nil
	clone.RawQuery = ""
	clone.ForceQuery = false
	clone.Fragment = ""
	clone.RawFragment = ""
	return clone.String()
}

func isHTTPOrHTTPS(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}

func defaultPortForScheme(u *url.URL) string {
	if u != nil && strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func portForURL(u *url.URL) (string, error) {
	if u == nil {
		return "", errors.New("URL is nil")
	}
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(u)
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return "", fmt.Errorf("invalid port %q", port)
	}
	return port, nil
}

func normalizedHostWithPort(u *url.URL, host string) string {
	port := u.Port()
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func normalizeHostname(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	host = strings.TrimSuffix(host, ".")
	return host
}

func normalizeAllowedDomain(entry string) string {
	entry = strings.TrimSpace(strings.ToLower(entry))
	if entry == "" {
		return ""
	}
	if parsed, err := url.Parse(entry); err == nil && parsed.Hostname() != "" {
		if parsed.Path != "" && parsed.Path != "/" {
			return ""
		}
		entry = parsed.Host
	}
	entry = strings.TrimSuffix(entry, ".")
	if strings.HasPrefix(entry, "*.") {
		return "*." + normalizeHostname(strings.TrimPrefix(entry, "*."))
	}
	if host, port, err := net.SplitHostPort(entry); err == nil && port != "" {
		entry = host
	}
	return normalizeHostname(entry)
}

func canonicalIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if ip4 := ip.To4(); ip4 != nil {
		return append(net.IP(nil), ip4...)
	}
	if ip16 := ip.To16(); ip16 != nil {
		return append(net.IP(nil), ip16...)
	}
	return nil
}

func appendUniqueIP(ips []net.IP, ip net.IP) []net.IP {
	for _, existing := range ips {
		if existing.Equal(ip) {
			return ips
		}
	}
	return append(ips, append(net.IP(nil), ip...))
}

func cloneIPs(ips []net.IP) []net.IP {
	result := make([]net.IP, len(ips))
	for i, ip := range ips {
		result[i] = append(net.IP(nil), ip...)
	}
	return result
}

func splitDialAddress(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err == nil {
		return host, port, nil
	}
	if strings.Count(address, ":") == 0 {
		return address, "", nil
	}
	return "", "", fmt.Errorf("invalid dial address %q: %w", address, err)
}

func schemeForPort(port string) string {
	if port == "443" {
		return "https"
	}
	return "http"
}

func sameHostname(a, b string) bool {
	left := normalizeHostname(strings.Trim(a, "[]"))
	right := normalizeHostname(strings.Trim(b, "[]"))
	leftIP, rightIP := net.ParseIP(left), net.ParseIP(right)
	if leftIP != nil && rightIP != nil {
		return leftIP.Equal(rightIP)
	}
	return left == right
}

func ipMatchesNetwork(ip net.IP, network string) bool {
	if strings.HasSuffix(network, "4") {
		return ip.To4() != nil
	}
	if strings.HasSuffix(network, "6") {
		return ip.To4() == nil
	}
	return true
}

func isAllZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
