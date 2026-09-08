# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-09-08

### Added
- Per-domain token-bucket rate limiting with configurable anti-bot jitter.
- Three-state circuit breaking (`CLOSED`, `OPEN`, `HALF-OPEN`) with `Retry-After` parsing and exponential cooldowns.
- Prioritized, bounded in-memory request queues for buffering requests during open circuits.
- Lightweight canary probe selection (`headers_only` and `query_param` strategies) during `HALF-OPEN` circuit recovery.
- Durable SQLite response caching (WAL mode) for idempotent GET and HEAD requests.
- Optional stale response serving (`Warning: 110`) while upstream circuits are open.
- In-flight request coalescing (singleflight) for identical cacheable requests.
- Robust SSRF protection blocking loopback, RFC 1918 private, link-local, multicast, and cloud metadata IPs, including DNS rebinding defenses.
- Outbound header sanitization, hop-by-hop stripping, and request body size enforcement.
- Operational endpoints:
  - `GET /_healthz` for process and storage readiness checks.
  - `GET /_metrics` for Prometheus-compatible counters and queue depth gauge.
  - `GET /_circuit` for inspecting per-domain circuit states.
  - `POST /_circuit/reset` for authenticated domain circuit reset.
- Multi-platform CGO-free release builds (Linux AMD64/ARM64, macOS AMD64/ARM64, Windows AMD64).
