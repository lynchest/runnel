# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security
- Refuse startup on a non-loopback bind (`server.host` / `RUNNEL_HOST`) with
  empty `security.allowed_domains`. The loopback default is unchanged.
- Honor upstream `Cache-Control` (`no-store`, `private`, `no-cache` are never
  stored; `s-maxage`/`max-age`/`Expires` shorten freshness under the
  configured TTL cap) and `Vary` (`Vary: *` is never stored; other fields
  must match the request for a HIT). Concurrent requests with different
  headers never share one singleflight upstream fetch, except per-request
  unique `X-Request-ID` (responses varying on it are never cached).
  Response age (`Age` header and `Date` skew) is subtracted from the
  lifetime; ambiguous freshness directives (unparseable or conflicting
  repeats) refuse the store. Rows written before Vary tracking safely
  miss until rewritten.
- `must-revalidate`/`proxy-revalidate` responses may serve fresh hits but
  are never served stale while a circuit is open.

### Added
- Per-domain metrics with `domain` labels (upstream requests/errors,
  upstream 429/503, circuit and queue rejections, queue depth) plus
  `runnel_circuit_state` and a bounded tracker (max 256 domains;
  overflow counted in `runnel_metrics_dropped_domains_total`).
- Single-line access log per `/proxy` request (method, domain, status,
  duration, cache result, error class, request ID). Upstream failures carry
  distinct classes (`dns_error`, `timeout`, `tls_error`,
  `response_read_error`, `response_too_large`). URLs, query values,
  headers, and bodies are never logged.

### Changed
- `runnel_queue_depth` is now the exact tracked-domain total maintained by
  atomic deltas instead of the last-written queue length. Domains beyond the
  256 tracking bound are excluded from the total.
- The configured cache TTL is an upper bound; upstream freshness directives can only shorten entry lifetimes.

## [0.1.6] - 2026-09-09

### Added
- Added `runnel status` for a human-readable gateway health and circuit summary.
- Added `runnel-get --verbose` for gateway, HTTP, cache, and retry metadata.

### Changed
- Corrected the embedded agent skill to use the shipped status command and added controlled Cloudflare 1010 diagnostics.
- Marked the legacy `runnel --install-skill` flag as deprecated in favor of `runnel install-skill [directory]`.

## [0.1.5] - 2026-09-09

### Security
- Prevent cached upstream authentication headers, including `Set-Cookie`, from being replayed to other clients.
- Prevent HTTP `206 Partial Content` responses from poisoning full-resource cache entries, including entries created by older versions.
- Make the container retain the loopback bind by default and disable CORS in the example configuration.

### Fixed
- Clamp upstream `Retry-After` cooldowns to the configured maximum.
- Recheck circuit state after queue and limiter delays before contacting the upstream.
- Allow `default_cache_ttl_sec: 0` to disable caching.
- Refund rate-limit tokens when a request is canceled during jitter.
- Log the effective listener address at startup.

## [0.1.4] - 2026-09-08

### Fixed
- Explicitly handle CLI output and response-close results so the native client passes the full CI lint gate.

## [0.1.3] - 2026-09-08

### Changed
- Replaced the Python `runnel-get` helper with a native CGO-free Go binary.
- Release archives now contain both `runnel` and `runnel-get` executables with no Python runtime requirement.

## [0.1.2] - 2026-09-08

### Added
- Embedded `install-skill` command for installing the bundled agent skill.
- Cross-platform `runnel-get` helper in release archives.
- Opt-in `--output markdown` mode that converts HTML locally while leaving gateway cache entries raw.

## [0.1.1] - 2026-09-08

### Added
- Standard HTTP `Cache-Control: no-cache` (along with `max-age=0`, `no-store`, and `Pragma: no-cache`) request header support to bypass response cache and force upstream revalidation.
- Official AI coding agent skill package (`skills/runnel/SKILL.md`) for Antigravity, Codex, Cursor, and Claude Code.
- Integration tests for cache bypass and response overwriting.

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
