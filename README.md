# runnel

> Architected & directed via agentic workflows; strictly tested & verified.

`runnel` is a small HTTP egress gateway for controlling requests to
external APIs and web services on a per-domain basis. It combines rate
limiting, circuit breaking, request queuing, and a bounded response cache in a
single process.

It is intended for scrapers, indexers, workers, and other applications that
make regular requests to external services and need predictable handling of
upstream limits. It is not a general-purpose public forward proxy or an AI
agent runtime.

## How it works

Clients send the target URL to the `/proxy` endpoint:

```text
client → URL/SSRF validation → domain limiter → circuit breaker → upstream
                                           ↓
                                    cache / queue
```

Each upstream domain is tracked independently. A `429` or `503` response opens
the circuit; `Retry-After`, when present, is used to calculate the cooldown.
Once the cooldown expires, one lightweight queued GET request is used as a
canary. If the upstream is healthy again, queued traffic is released. If not,
the cooldown is increased exponentially.

## Features

- Per-domain token-bucket rate limiting with jitter
- Per-domain `CLOSED`, `OPEN`, and `HALF-OPEN` circuit breakers
- Bounded, prioritized in-memory request queues
- `Retry-After` handling and exponential cooldowns
- Lightweight GET canary probes in `HALF-OPEN`
- SQLite-backed cache for GET and HEAD responses
- Optional stale-cache responses while a circuit is open
- Singleflight coalescing for identical idempotent requests
- Redirect target validation
- SSRF protection against private, loopback, link-local, multicast, and metadata IPs
- Hop-by-hop header removal and request body limits
- Health, metrics, and circuit administration endpoints
- CGO-free Go binary

## Security

`runnel` is designed as an internal egress gateway for trusted local services,
**not** a general-purpose public open proxy. It must not be exposed directly to
the public internet or untrusted clients without an authentication proxy or
explicit domain restrictions (`security.allowed_domains`).

By default, `runnel` binds safely to `127.0.0.1:8090`. Before exposing it on any
network interface, ensure you configure the following:

- Keep `security.block_private_ips: true` enabled to prevent SSRF against private,
  loopback, link-local, multicast, and cloud metadata endpoints.
- Use `security.allowed_domains` to explicitly restrict upstream destinations.
- Set a strong `security.admin_token` or `RUNNEL_ADMIN_TOKEN` value. When empty,
  administrative mutations (`POST /_circuit/reset`) are rejected with `403 Forbidden`.
- Never expose administration endpoints (`/_*`) directly to the internet.

For vulnerability disclosure instructions, see [SECURITY.md](SECURITY.md).

## Performance

The repository includes integration and soak tests covering the complete
request path. The current local measurements are:

- Idle resident memory: approximately **2 MB RSS** for the running process.
- Local Linux ARM64 development binary: approximately **14.7 MiB** (unstripped).
- Soak test: **10,000 requests** at **32 concurrent workers**, with one upstream
  call due to request coalescing and approximately **10 KB heap growth** during
  the run.

These are baseline measurements from the included test environment, not hard
capacity guarantees. Memory usage increases with concurrent requests, queued
items, and response sizes. Individual upstream responses are bounded at 64 MB;
incoming request bodies are bounded by the configured 10 MB default.

## Installation

> **Note for users and automated agents:** You do **not** need to install Go or build from source. Pre-compiled, zero-dependency binaries for Linux (`amd64`, `arm64`), macOS (`amd64`, `arm64`), and Windows are available in [GitHub Releases](https://github.com/lynchest/runnel/releases), as well as multi-arch container images on GHCR.

### Run with Docker (Recommended for Services & Sidecars)

Multi-arch container images (`linux/amd64`, `linux/arm64`) are published to GitHub Container Registry:

```bash
docker run -d --name runnel \
  -p 8090:8090 \
  -v runnel-data:/data \
  ghcr.io/lynchest/runnel:latest
```

Or as a sidecar in `docker-compose.yml`:

```yaml
services:
  runnel:
    image: ghcr.io/lynchest/runnel:latest
    ports:
      - "8090:8090"
    volumes:
      - runnel-data:/data
    restart: unless-stopped

volumes:
  runnel-data:
```

### Run a release binary

Pre-built binaries are available for all major platforms:

| Platform | Architecture | Archive | Executable |
| --- | --- | --- | --- |
| **Linux** | x86_64 (`amd64`), ARM64 (`arm64`) | `runnel_*_linux_<arch>.tar.gz` | `./runnel` |
| **macOS** | Apple Silicon (`arm64`), Intel (`amd64`) | `runnel_*_darwin_<arch>.tar.gz` | `./runnel` |
| **Windows** | x86_64 (`amd64`) | `runnel_*_windows_amd64.zip` | `runnel.exe` |

Download the archive for your platform from [GitHub Releases](https://github.com/lynchest/runnel/releases):

#### Linux & macOS

```bash
# Extract archive (example for Linux/macOS):
tar -xzf runnel_*_linux_amd64.tar.gz   # Linux x86_64
# tar -xzf runnel_*_linux_arm64.tar.gz   # Linux ARM64
# tar -xzf runnel_*_darwin_arm64.tar.gz  # macOS Apple Silicon (M1/M2/M3/M4)
# tar -xzf runnel_*_darwin_amd64.tar.gz  # macOS Intel

./runnel -config runnel.example.yaml
```

Or download via GitHub CLI:

```bash
# Linux AMD64
gh release download -R lynchest/runnel --pattern "*linux_amd64.tar.gz"

# macOS Apple Silicon
gh release download -R lynchest/runnel --pattern "*darwin_arm64.tar.gz"
```

#### Windows (PowerShell)

```powershell
Expand-Archive -Path runnel_*_windows_amd64.zip -DestinationPath .
.\runnel.exe -config runnel.example.yaml
```

### Build from source (Developers)

Requirement: Go 1.22 or newer.

```bash
git clone https://github.com/lynchest/runnel.git
cd runnel
go build -o bin/runnel ./cmd/runnel
```

## Quick start

With the default configuration, the service listens on `127.0.0.1:8090`:

```bash
./runnel -config runnel.example.yaml
# (on Windows: .\runnel.exe -config runnel.example.yaml)
# (or ./bin/runnel if built from source)
```

Send a request through the gateway:

```bash
curl -i "http://127.0.0.1:8090/proxy?url=https://httpbin.org/get"
```

Provide a YAML configuration with `-config` or the `RUNNEL_CONFIG`
environment variable. All supported settings are documented in
[runnel.example.yaml](runnel.example.yaml).

## Endpoints

| Method | Endpoint | Description |
| --- | --- | --- |
| Any | `/proxy?url=<target>` | Validates and forwards a request to the upstream |
| GET | `/_healthz` | SQLite and application readiness check |
| GET | `/_metrics` | Prometheus-compatible counters |
| GET | `/_circuit` | JSON view of domain circuit states |
| POST | `/_circuit/reset[?domain=...]` | Resets one domain or all circuits |

The `/_circuit/reset` endpoint requires an `X-Admin-Token` header or a Bearer
token:

```bash
curl -X POST \
  "http://127.0.0.1:8090/_circuit/reset?domain=api.example.com" \
  -H "X-Admin-Token: your-secret-token"
```

## Cache and failure behavior

Successful GET and HEAD responses can be cached for the configured TTL. When a
circuit is open and an older cached response is available, it is returned with
`X-Cache: STALE` and `Warning: 110` headers. Without a cached response, the
request waits in the domain queue. If the queue is full or its timeout expires,
the gateway returns `503 Service Unavailable` with a `Retry-After` header.

Non-idempotent methods such as POST, PUT, PATCH, and DELETE are not queued while
a circuit is open; they are rejected with the applicable cooldown information.

## Development

```bash
# Tests
go test -v ./...

# Static analysis
go vet ./...
make lint

# Binary
make build

# Race detector (on supported hosts)
make test-race
```

The race detector depends on the Go toolchain and the host operating system's
virtual address space support. The CI workflow runs race tests on Linux AMD64
and builds a matrix for Linux, macOS, and Windows.

For contribution guidelines and coding standards, see [CONTRIBUTING.md](CONTRIBUTING.md).
See [CHANGELOG.md](CHANGELOG.md) for release history.

## License

[MIT](LICENSE) © 2026 Lynchest
