# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| >=0.1.x | :white_check_mark: |
| < 0.1.0 | :x:                |

## Operational Scope and Boundaries

`runnel` is an internal egress gateway intended to run on loopback (`127.0.0.1`)
or within a protected private network segment. It is **not** designed as an open
public proxy.

When deploying `runnel`, adhere to the following baseline security controls:
- **SSRF Safeguards:** Keep `security.block_private_ips: true` enabled to prevent
  requests to loopback, RFC 1918 private subnets, link-local addresses, and cloud
  instance metadata endpoints (e.g., `169.254.169.254`).
- **Domain Whitelisting:** Use `security.allowed_domains` to explicitly restrict
  egress to known upstream hosts whenever possible.
- **Admin Endpoints:** Guard mutations (`POST /_circuit/reset`) with a strong
  `security.admin_token` or `RUNNEL_ADMIN_TOKEN`. When empty, mutation endpoints
  reject all requests with `403 Forbidden`. Do not expose `/_*` admin endpoints to
  untrusted networks.

## Reporting a Vulnerability

If you discover a security vulnerability in `runnel` (such as SSRF bypasses,
authorization escapes, or memory exhaustion vectors), please report it
responsibly.

- **Preferred method:** Open a private security advisory via GitHub at
  [https://github.com/lynchest/runnel/security/advisories/new](https://github.com/lynchest/runnel/security/advisories/new).
- **Alternative:** Contact `77403803+lynchest@users.noreply.github.com` with
  details of the vulnerability, reproduction steps, and any proof of concept.

Please do not open public GitHub issues for suspected security vulnerabilities.

### Response Process

- We will acknowledge receipt of your report within 48 hours.
- We will evaluate the impact, develop a fix, and coordinate disclosure.
- Fixes will be released via tagged GitHub releases.
