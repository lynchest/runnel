# Contributing to runnel

Thank you for your interest in contributing to `runnel`!

`runnel` is a lightweight, focused HTTP egress gateway with strict boundaries
around simplicity, predictable memory usage, and concurrency safety.

## Prerequisites

- **Go:** 1.22 or newer (1.24+ recommended)
- **Make:** standard POSIX `make`
- **golangci-lint:** v1.64 or newer
- **GoReleaser:** v2 or newer (for build verification)

## Development Workflow

1. Clone the repository:
   ```bash
   git clone https://github.com/lynchest/runnel.git
   cd runnel
   ```

2. Run the test suite:
   ```bash
   go test -v ./...
   ```

3. Run static analysis and formatting checks:
   ```bash
   go vet ./...
   make lint
   test -z "$(gofmt -l .)"
   ```

4. Compile the binary:
   ```bash
   make build
   ```

5. (Optional) Run the race detector on supported platforms (Linux AMD64, macOS, Windows):
   ```bash
   make test-race
   ```

## Contribution Principles

- **Simplicity First (YAGNI):** Avoid single-use abstractions or unnecessary
  dependencies. Rely on standard library capabilities whenever possible.
- **Surgical Changes:** Keep pull requests focused on a specific bug fix or feature.
  Avoid reformatting unrelated code or refactoring working subsystems.
- **Security & Safety:** Changes touching URL parsing, SSRF validation, body limits,
  or header sanitization must include tests proving safeguards cannot be bypassed.
- **Verification:** Every change should include automated unit or integration
  tests verifying expected behavior and edge cases.
