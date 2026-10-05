# AyeusANN

**Managed AI inference on Indian GPU capacity.** *(Working name; the product name lives in `web/console/src/lib/brand.ts`.)*

Two sides, one platform:

- **Customers** deploy open-source models (Llama, Qwen, Mistral, Gemma) and call them with the OpenAI SDK by changing only `base_url`.
- **Hosts** connect GPUs, from data centres to college labs and personal laptops, with a single agent.

Every machine sits in a trust tier that decides its SLA and what may run on it:

| Tier | Who | Reliability |
|---|---|---|
| T1 | Data centres, enterprise servers | 99.5% SLA |
| T2 | College labs, vetted workstations | 99% SLA |
| T3 | Personal laptops and desktops | Spot: best effort, interruptible |

## Quick start

Prerequisites:
- Docker, Go 1.25+, Rust (stable), Node 22+
- [`golang-migrate`](https://github.com/golang-migrate/migrate) (`brew install golang-migrate`)
- [Ollama](https://ollama.com/download), running

```bash
make dev
```

This does four things:
1. Starts Postgres and Redis.
2. Creates, migrates and seeds the `ayeusann_dev` and `ayeusann_test` databases.
3. Builds and starts every service.
4. Starts the web console.

Then open **http://localhost:8080**:

1. Create an account.
2. Connect this machine as a host:
   - Go to **Host → Add a machine → Create install command**.
   - Run the **From this repo** command in a new terminal and leave it running. (The other tabs install the agent as a background service that starts at login, which is what you want on a machine that is not for development.)
3. Deploy a model:
   - Go to **Deploy → Models**.
   - Deploy `gemma-2-2b-it` on T3 and watch it reach **Serving**.
4. Chat with it in the **Playground**, or call it from code:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk_live_...")
client.chat.completions.create(model="<deployment-name>", messages=[{"role": "user", "content": "Hi"}])
```

To use a GPU that sits in a different computer (a friend's laptop, a lab machine), follow [docs/connect-a-gpu.md](docs/connect-a-gpu.md). `make dev` prints the address other machines on your network use.

To run it for real for yourself and people you know (generated secrets, its own database, daily backups, back after a reboot), follow [docs/private-network.md](docs/private-network.md).

To verify the whole flow automatically, run `scripts/smoke.sh`. It signs up, enrols two hosts, deploys, streams, checks the usage records, kills a host to test failover, and stops the deployment.

## Commands

| Command | What it does |
|---|---|
| `make dev` | Start everything |
| `make down` | Stop services and the console |
| `make status` | Health of every service |
| `make services` | Rebuild and restart the Go services |
| `make db` | Create, migrate and seed the dev and test databases |
| `make test` | Go, Rust and web tests |
| `make smoke-fake` | End-to-end tests with no GPU and no Ollama (needs `make dev` running) |
| `make lint` | `go vet`, `clippy`, `buf lint` |
| `make proto` | Regenerate protobuf code |
| `make dist-agent` | Publish this machine's agent build so the installer can download it |
| `make dist-agent-linux` | Build the Linux agent in Docker for the installer |
| `make dist-agent-windows` | Cross-compile the Windows agent in Docker for the installer |
| `make private-up` | Start a private installation on this computer ([guide](docs/private-network.md)) |
| `make private-check` | Prove the private installation works, on a throwaway copy |
| `make prod-up` | Build, migrate and start the production stack ([guide](docs/deployment.md)) |

Logs are written to `logs/<service>.log`.

## How it works

```
Customer ──► gateway :8080 ──► control-api    accounts, models, deployments, hosts, ops
                  │        ──► trust-engine   host reputation
                  │        ──► web console    Next.js
                  └──────────► inference-gateway ──► coordinator ══gRPC══► host agent ──► Ollama / vLLM
                               (auth, routing,       (sessions,           (outbound-only,
                                usage)                dispatch, tunnel)    runs the model)

scheduler: places replicas on GPUs and backfills failures
```

1. **Deploy.** The control API records the deployment as `pending`. The scheduler scores online GPUs by reputation, tier fit, locality and cache warmth, then reserves one. The coordinator sends the host a signed manifest. The agent pulls, loads and warms the model, and the deployment moves through *Scheduling → Pulling → Loading → Warming → Serving*.
2. **Inference.** Hosts only ever connect *out*, so requests travel back to them over the agent's existing gRPC stream. That means no open ports, and it works behind home and campus NAT. Tokens stream straight through to the customer.
3. **Usage.** Each request writes one usage event (tokens, duration, outcome). Dashboards, host activity and reputation are all derived from those events.
4. **Failure.** A host that misses three heartbeats (15 s) goes offline, its replicas are failed, and the scheduler places new ones elsewhere. Requests retry on another replica before the first byte is sent.

See [ADR-011](docs/adrs/011-inference-tunnel-and-control-loops.md) for why the design differs from the original M0 architecture.

## Repository layout

```
agent/            Rust host agent: GPU detection, benchmark, runtime supervision, inference tunnel
services/         Go services: gateway, control-api, scheduler, coordinator,
                  inference-gateway, trust-engine
internal/         Shared Go packages: auth, usage, placement, lifecycle, db, httpx
proto/            Agent ↔ coordinator gRPC contract (generated code in gen/go)
schema/           SQL migrations and seed data (catalogue, GPU rate card)
web/console/      Next.js console: landing page, customer console, host console, ops
web/install/      Host installers served at /install.sh
scripts/          dev.sh (local environment), private.sh (private installation), smoke*.sh and private-check.sh (end-to-end tests)
cmd/              fake-runtime: a stand-in for Ollama used by the end-to-end tests
deploy/           Dockerfiles and docker-compose (dev infrastructure, private installation, production stack)
docs/             Guides (development, connecting a GPU, private network, deployment), architecture decision records
_archive/         Superseded code kept for reference (old frontends, standalone router)
```

## Status

**Working end to end:**
- Sign-up and sign-in; on a real installation accounts are by invitation and the first account, made with a one-time owner code, is the operator
- Deploy wizard, placement and serving
- Streaming inference through the OpenAI SDK
- Per-request usage records (requests, tokens, latency) for customers and hosts
- Failover
- Host onboarding from other machines: one-line installers for macOS, Linux and Windows, over one address and one port
- A private installation on one computer, with generated secrets and checked backups
- Reputation scoring
- Ops console

**Not built yet:**
- Billing: wallets, prices, invoices and host payouts. It was removed so the platform can be finished first; the database tables are still in the schema and the previous implementation is in git history (last present in commit `8f65a73`).
- Firecracker job isolation
- Google/GitHub sign-in
- CLI and Python SDK
- Autoscaling, scale-to-zero and burst-to-spot
- The NATS event bus

## Configuration

Every service has working development defaults. `SN_ENV` chooses the mode: `dev`, `private` (a real installation on a network you trust: real secrets required, plain HTTP allowed) or `production` (real secrets and TLS required; also what an unset or misspelled value means). See [`.env.example`](.env.example) for every setting, including platform admin emails and public URLs.

## Production

`make prod-up` runs the whole platform on one machine with Docker Compose: Caddy for TLS, the services, the console, Postgres and Redis. See [docs/deployment.md](docs/deployment.md).

## License

Proprietary — Ayeus Private Limited
