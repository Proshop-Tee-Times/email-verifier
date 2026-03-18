# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Go-based email validation microservice that checks syntax, domain existence, MX records, disposable domains, role-based addresses, and aliases. Privacy-first design — only domain results are cached, no email data is stored.

## Common Commands

### Build & Run
```bash
go build                          # Build binary
go run .                          # Run directly
PORT=8080 REDIS_URL=redis://... go run .  # With config
docker-compose up -d              # Full stack (Redis, Prometheus, Grafana)
```

### Testing
```bash
go test ./...                     # All tests
go test -race ./...               # With race detector (CI uses this)
go test ./tests/unit/... -v       # Unit tests only
go test ./tests/integration/... -v  # Integration tests
go test ./tests/acceptance/... -v   # Acceptance tests
go test ./... -skip "Load"        # Skip load tests
go test -run TestSpecificName ./tests/unit/validator/  # Single test
```

### Linting
```bash
golangci-lint run                 # Configured in .golangci.yml
go fmt ./...                      # Format
go vet ./...                      # Vet
```

## Architecture

**Layered design with dependency injection through interfaces (`internal/service/interfaces.go`).**

```
main.go (server setup, routing, middleware)
  → internal/api/handlers.go (HTTP handlers: /api/validate, /api/validate/batch, /api/typo-suggestions, /api/status)
    → internal/service/ (business logic orchestration)
      → pkg/validator/ (core validation: syntax, domain, disposable, role, alias)
      → pkg/cache/ (Redis + in-memory domain caching)
      → pkg/monitoring/ (Prometheus metrics + HTTP middleware)
```

### Key design decisions
- **Batch optimization**: `BatchValidationService` groups emails by domain to avoid redundant DNS lookups, validates each domain once
- **Concurrent domain checks**: `ConcurrentDomainValidationService` runs A record + MX record lookups in parallel goroutines
- **Redis is optional**: Falls back to in-memory cache (`DomainCacheManager`) when `REDIS_URL` is not set
- **Scoring**: Emails get a 0-100 score; status constants (VALID, PROBABLY_VALID, INVALID, etc.) are in `internal/model/email.go`
- **Alias detection** (`pkg/validator/alias_detector.go`): Provider-specific normalization (Gmail strips dots/plus, Yahoo strips hyphens, Outlook strips plus)

### Test structure
- `tests/unit/` — validators and services with mocks
- `tests/integration/` — HTTP handler tests with real service wiring
- `tests/acceptance/` — business logic scenarios (batch grouping, null MX)
- Uses `stretchr/testify` for assertions and mocks

## Environment
- Go 1.21+
- Default port: 8080
- Deployed to Fly.io (Amsterdam region)
