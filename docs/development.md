# AyeusANN — Local Development Guide

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| Go | 1.23+ | `brew install go` |
| Rust | stable | `brew install rust` |
| Node.js | 22+ | `nvm install 22` |
| Docker | Latest | [Docker Desktop](https://www.docker.com/products/docker-desktop/) |
| protoc | 3.x | `brew install protobuf` |
| buf | Latest | `brew install buf` |
| migrate | Latest | `brew install golang-migrate` |
| Python | 3.11+ | System or `brew install python` |

## Quick Start

```bash
# Clone the repository
git clone <repo-url> && cd AyeusANN

# Copy environment variables
cp .env.example .env

# Start everything
make dev
```

This starts:
- **Postgres** on `:5432` (user: `AyeusANN`, pass: `AyeusANN_dev`)
- **Redis** on `:6379`
- **NATS** on `:4222` (monitoring: `:8222`)
- **MinIO** on `:9000` (console: `:9001`, user: `AyeusANN`, pass: `AyeusANN_dev`)
- **8 Go services** on ports `8080–8087`

## Service Ports

| Service | Port | Description |
|---------|------|-------------|
| gateway | 8080 | API Gateway (authn, rate limit) |
| control-api | 8081 | CRUD for orgs, users, deployments |
| scheduler | 8082 | GPU placement and scheduling |
| coordinator | 8083 | Agent session management |
| router | 8084 | Inference request routing |
| inference-gateway | 8085 | TLS, key auth, per-key limits |
| billing-meter | 8086 | Usage → ledger → invoices |
| trust-engine | 8087 | Reputation and verification |

## Common Commands

```bash
make dev            # Start full dev environment
make dev-stop       # Stop everything
make check          # Health check all services
make build          # Build all binaries
make test           # Run all tests
make lint           # Lint all code
make fmt            # Format all code
make migrate        # Run database migrations
make migrate-down   # Rollback last migration
make seed           # Seed database with catalog model
make proto          # Generate protobuf code
make clean          # Clean build artifacts
```

## Database

Connect directly:
```bash
psql postgres://AyeusANN:AyeusANN_dev@localhost:5432/AyeusANN
```

Reset:
```bash
make migrate-down
make migrate
make seed
```

## Fake GPU Mode

The development environment runs in **fake GPU mode** by default (`SN_FAKE_GPU=true`).
This allows the complete control plane workflow without NVIDIA hardware.

> ⚠️ Fake GPU mode does NOT validate real GPU execution. Use staging with real GPUs for that.

## Architecture

See [03_System_Architecture.md](../docs/) for the full system design.

```
Customer → Gateway → Control API → Scheduler → Coordinator → Agent
                                                                ↓
                              Inference GW → Router → Replica (vLLM)
                                                        ↓
                                              Usage Event → Billing Meter
```
