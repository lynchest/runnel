package circuit

// The circuit package contains the per-domain circuit breaker and the
// Retry-After parsing helpers used by the gateway.  The implementation is
// intentionally independent of the configuration package: callers can pass
// the small Config value below, while registry construction also understands
// config-like values from other packages.
