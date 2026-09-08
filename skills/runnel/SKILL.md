---
name: runnel
description: >-
  Use whenever making HTTP GET requests, scraping web pages, or querying external APIs
  subject to rate limits, burst penalties, or IP bans (such as Reddit, Steam, IGDB,
  GitHub, Twitter, or public web targets). Instructs the agent how to route requests
  through the runnel egress gateway, check circuit breaker states, utilize singleflight
  coalescing and SQLite caching, and force cache bypass when fresh upstream data is required.
---

# runnel — Smart API Circuit Breaker & Rate-Limit Shield

`runnel` is an internal egress gateway running on port `8090` (default host `127.0.0.1` or network host `0.0.0.0`). It safeguards your client IP against aggressive burst penalties, 429 rate limits, and cascading retry loops.

## Core Capabilities

1. **Rate Limiting & Jitter:** Automatically spaces out requests per domain with randomized human jitter (100–500 ms).
2. **Circuit Breaking:** Trips to `OPEN` on upstream `429` / `503` responses. Freezes traffic for the upstream `Retry-After` duration or exponential backoff.
3. **Singleflight Deduplication:** Coalesces simultaneous identical requests across all clients and agents into a single upstream request.
4. **Dual-Layer SQLite Cache:** GET and HEAD responses are stored in SQLite WAL (`default TTL: 3600s`). Cached hits resolve in 0 ms.
5. **Stale Serving on Failure:** If upstream is `OPEN` or failing, serves stale cache (`Warning: 110`) rather than failing completely.
6. **Canary Probing:** Tests upstream recovery in `HALF-OPEN` with a single lightweight GET probe.

---

## When to Use This Skill

- **DO USE** for any external HTTP requests to rate-limited services (Reddit, Steam, IGDB, GitHub, web scraping, documentation fetching, RSS feeds).
- **DO NOT USE** for internal services, localhost endpoints, private LAN IPs (runnel blocks RFC 1918 IPs by SSRF policy), or large binary downloads > 64 MB.

---

## How to Make Requests

### Method 1: Universal CLI Client (`runnel-get`) — Recommended

The `runnel-get` CLI utility automatically discovers the active gateway across localhost, MagicDNS, Tailscale, and LAN:

```bash
# Standard request (uses cache if available)
runnel-get "https://api.github.com/zen"

# Force refresh (bypasses cache, fetches fresh from upstream, updates SQLite)
runnel-get --no-cache "https://api.github.com/repos/lynchest/runnel"
runnel-get -f "https://reddit.com/r/golang.json"

# With custom request headers
runnel-get -H "Authorization: Bearer <token>" "https://api.github.com/user"
```

### Method 2: Direct HTTP Gateway (`curl` / `fetch` / `urllib`)

Target endpoint: `/proxy?url=<URL_ENCODED_TARGET>`

```bash
# Via curl
curl -s "http://127.0.0.1:8090/proxy?url=https%3A%2F%2Fapi.github.com%2Fzen"

# Force refresh via standard HTTP header
curl -s -H "Cache-Control: no-cache" "http://127.0.0.1:8090/proxy?url=https%3A%2F%2Fapi.github.com%2Fzen"
```

In Python:
```python
import urllib.parse
import urllib.request

target = "https://api.github.com/zen"
gateway = "http://127.0.0.1:8090"  # or http://hermes:8090
proxy_url = f"{gateway}/proxy?url={urllib.parse.quote(target, safe='')}"

# To bypass cache, pass Cache-Control header:
req = urllib.request.Request(proxy_url, headers={"Cache-Control": "no-cache"})
with urllib.request.urlopen(req) as resp:
    data = resp.read().decode("utf-8")
```

---

## Network & Multi-Device Access

When running on remote machines or nodes:

| Machine / Environment | Primary Gateway URL | Fallback URL |
| :--- | :--- | :--- |
| **Local Hub (Hermes)** | `http://127.0.0.1:8090` | `http://hermes:8090` |
| **MacBook Air / macOS** | `http://hermes:8090` (MagicDNS) | Tailscale `http://100.87.224.51:8090` / LAN `http://192.168.1.120:8090` |
| **Windows / RTX PC** | `http://hermes:8090` (MagicDNS) | Tailscale `http://100.87.224.51:8090` / LAN `http://192.168.1.120:8090` |

---

## Administrative & Health Endpoints

- **Health check:** `GET /_healthz` → `{"status":"ok"}`
- **Active circuits:** `GET /_circuit` → lists domain states (`CLOSED`, `OPEN`, `HALF-OPEN`)
- **Prometheus metrics:** `GET /_metrics` → cache hits, rate limits, request totals
- **Circuit reset:** `POST /_circuit/reset[?domain=...]` (requires `X-Admin-Token` if configured)
- **CLI status view:** `runnel-status`
