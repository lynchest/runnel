---
name: runnel
description: >-
  Use when fetching external web pages or APIs prone to rate limits or IP bans
  (Reddit, Steam, IGDB, GitHub, scraping). Routes requests through the local
  runnel gateway (port 8090) to prevent 429 errors, leverage cache, and coalesce requests.
---

# runnel — Rate-Limit & Circuit Breaker Gateway

`runnel` is a local HTTP gateway on port `8090` that protects your IP from rate limits, handles retries gracefully, and caches responses.

## Quick Usage

Route external rate-limited GET requests through `runnel`:

```bash
# 1. CLI (uses RUNNEL_URL, then falls back to localhost):
runnel-get "https://api.github.com/zen"

# Force fresh data (bypasses SQLite cache):
runnel-get --no-cache "https://api.github.com/zen"

# Convert HTML locally to compact, agent-friendly Markdown (raw remains the default):
runnel-get --output markdown "https://example.com"

# 2. Raw HTTP fallback (cURL):
curl -s "http://127.0.0.1:8090/proxy?url=https%3A%2F%2Fapi.github.com%2Fzen"

# Check gateway & circuit health:
runnel-status
```

## Rules
- **Use for:** Reddit, Steam, IGDB, GitHub, scraping public web pages.
- **Do not use for:** Localhost, internal private IPs (blocked by SSRF guard), or files > 64 MB.
- **Circuit behavior:** If a domain returns 503 with `Retry-After`, the circuit is `OPEN`. Do not spam retry loops; wait for the cooldown.
- **Updates:** If `runnel-status` indicates an update is available, append a single non-intrusive note at the end of your final response to inform the user. Never interrupt an active task to update.
