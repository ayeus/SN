# Local development

## Prerequisites

| Tool | Version | Install (macOS) |
|---|---|---|
| Docker | recent | [Docker Desktop](https://www.docker.com/products/docker-desktop/) |
| Go | 1.25+ | `brew install go` |
| Rust | stable | [rustup.rs](https://rustup.rs) |
| Node.js | 22+ | `nvm install 22` |
| golang-migrate | 4.x | `brew install golang-migrate` |
| Ollama | recent | [ollama.com/download](https://ollama.com/download) |
| buf, protoc | recent | `brew install buf protobuf` (only to regenerate protobuf code) |

## Start

```bash
make dev
```

This starts Postgres (`:5433`) and Redis (`:6379`) in Docker, creates, migrates
and seeds `ayeusann_dev` and `ayeusann_test`, builds and starts the Go services,
and starts the web console. Open http://localhost:8080.

No `.env` is needed: every service has development defaults. Copy
`.env.example` to `.env` to override them.

| Service | Port | Role |
|---|---|---|
| gateway | 8080 | Public entry point; proxies the API and the console |
| control-api | 8081 | Accounts, models, deployments, hosts, ops |
| scheduler | 8082 | Places replicas on GPUs |
| coordinator | 8083, gRPC 50051 | Agent sessions, manifests, inference tunnel |
| inference-gateway | 8085 | OpenAI-compatible API, key auth, metering |
| billing-meter | 8086 | Wallet, usage, invoices, top-ups |
| trust-engine | 8087 | Host reputation |
| web console | 3000 | Next.js dev server, reached through the gateway |

## Commands

| Command | What it does |
|---|---|
| `make dev` | Start everything |
| `make down` | Stop the services and the console (Postgres and Redis keep running) |
| `make status` | Health of every service |
| `make services` | Rebuild and restart the Go services |
| `make web` | Restart the console dev server |
| `make db` | Create, migrate and seed both databases |
| `make migrate` / `make migrate-down` | Apply or roll back one dev migration |
| `make test` | Go, Rust and web tests |
| `make lint` | `go vet`, `clippy`, `buf lint` |
| `make fmt` | `gofmt` and `cargo fmt` |
| `make proto` | Regenerate `gen/go` from `proto/` |

Logs are in `logs/<service>.log` and `logs/web.log`.

## Adding a host

Sign in, open **Host → Add a machine → Create install command**, and run the
**From this repo** command in another terminal. It builds the agent and connects
this machine through Ollama.

For a second local host without a second GPU:

```bash
./run-node.sh --fake-gpu --instance b --token <token>
```

A fake-GPU host reports invented hardware and is labelled as such; it exercises
enrolment, scheduling and failover, not real inference performance.

## Tests

```bash
make test-go        # needs `make db`; integration tests use ayeusann_test
make test-agent
make test-web       # type-check
scripts/smoke.sh    # end to end against a running `make dev` and Ollama
```

`scripts/smoke.sh` signs up, enrols two hosts, deploys, streams a chat, checks
billing, kills a host to test failover, and stops the deployment.

Go integration tests skip when the test database is unreachable. Set
`SN_REQUIRE_DB=1` to make them fail instead, as CI does.

## Database

```bash
psql postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann_dev
```

Migrations are in `schema/migrations`, seed data (model catalogue, GPU rate
card) in `schema/seeds`. Seeds are idempotent and re-applied by `make db`.

## Password reset in development

With no SMTP relay configured, the reset email is written to
`logs/control-api.log` instead of being sent.

## Production

See [deployment.md](deployment.md). To try the production images locally, set
`APP_DOMAIN=localhost` and `COORDINATOR_DOMAIN=coordinator.localhost` in
`deploy/compose/.env.prod`; Caddy then issues certificates from its own local
authority, which browsers and agents do not trust by default.
