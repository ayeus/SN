# AyeusANN

**Managed AI inference on Indian GPU capacity.** *(Working name; the product name lives in `web/console/src/lib/brand.ts`.)*

Two sides, one platform:

- **Customers** deploy open-source models (Llama, Qwen, Mistral, Gemma) and call them with the OpenAI SDK by changing only `base_url`. They pay per token from a prepaid rupee wallet.
- **Hosts** connect GPUs, from data centres to college labs and personal laptops, with a single agent, and keep 75% of what customers pay for their GPU time.

Every machine sits in a trust tier that decides its SLA, what may run on it, and its price:

| Tier | Who | Reliability |
|---|---|---|
| T1 | Data centres, enterprise servers | 99.5% SLA |
| T2 | College labs, vetted workstations | 99% SLA |
| T3 | Personal laptops and desktops | Spot: best effort, 55% of on-demand price |

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

1. Create an account. New Indian accounts get ₹500 of credit.
2. Connect this machine as a host:
   - Go to **Host → Add a machine → Create install command**.
   - Run the **From this repo** command in a new terminal and leave it running.
3. Deploy a model:
   - Go to **Deploy → Models**.
   - Deploy `gemma-2-2b-it` on T3 and watch it reach **Serving**.
4. Chat with it in the **Playground**, or call it from code:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk_live_...")
client.chat.completions.create(model="<deployment-name>", messages=[{"role": "user", "content": "Hi"}])
```

To verify the whole flow automatically, run `scripts/smoke.sh`. It signs up, enrols two hosts, deploys, streams, checks billing, kills a host to test failover, and stops the deployment.

## Commands

| Command | What it does |
|---|---|
| `make dev` | Start everything |
| `make down` | Stop services and the console |
| `make status` | Health of every service |
| `make services` | Rebuild and restart the Go services |
| `make db` | Create, migrate and seed the dev and test databases |
| `make test` | Go, Rust and web tests |
| `make lint` | `go vet`, `clippy`, `buf lint` |
| `make proto` | Regenerate protobuf code |
| `make dist-agent` | Publish this machine's agent build so the installer can download it |

Logs are written to `logs/<service>.log`.

## How it works

```
Customer ──► gateway :8080 ──► control-api    accounts, models, deployments, hosts, ops
                  │        ──► billing-meter  wallet, usage, invoices, Razorpay top-ups
                  │        ──► trust-engine   host reputation
                  │        ──► web console    Next.js
                  └──────────► inference-gateway ──► coordinator ══gRPC══► host agent ──► Ollama / vLLM
                               (auth, routing,       (sessions,           (outbound-only,
                                metering)             dispatch, tunnel)    runs the model)

scheduler: places replicas on GPUs, backfills failures, pauses deployments on empty wallets
```

1. **Deploy.** The control API records the deployment as `pending`. The scheduler scores online GPUs by reputation, price, locality and cache warmth, then reserves one. The coordinator sends the host a signed manifest. The agent pulls, loads and warms the model, and the deployment moves through *Scheduling → Pulling → Loading → Warming → Serving*.
2. **Inference.** Hosts only ever connect *out*, so requests travel back to them over the agent's existing gRPC stream. That means no open ports, and it works behind home and campus NAT. Tokens stream straight through to the customer.
3. **Billing.** Each request writes one usage event, charges the customer's wallet, and credits the host 75%, all in a single database transaction.
4. **Failure.** A host that misses three heartbeats (15 s) goes offline, its replicas are failed, and the scheduler places new ones elsewhere. Requests retry on another replica before the first byte is sent.

See [ADR-011](docs/adrs/011-inference-tunnel-and-control-loops.md) for why the design differs from the original M0 architecture.

## Repository layout

```
agent/            Rust host agent: GPU detection, benchmark, runtime supervision, inference tunnel
services/         Go services: gateway, control-api, scheduler, coordinator,
                  inference-gateway, billing-meter, trust-engine
internal/         Shared Go packages: auth, billing, placement, lifecycle, money, db, httpx
proto/            Agent ↔ coordinator gRPC contract (generated code in gen/go)
schema/           SQL migrations and seed data (catalogue, GPU rate card)
web/console/      Next.js console: landing page, customer console, host console, ops
web/install/      Host installers served at /install.sh
scripts/          dev.sh (local environment), smoke.sh (end-to-end test)
deploy/           docker-compose, k8s, terraform
docs/             Development guide and architecture decision records
_archive/         Superseded code kept for reference (old frontends, standalone router)
```

## Status

**Working end to end:**
- Sign-up and wallets
- Deploy wizard, placement and serving
- Streaming inference through the OpenAI SDK
- Per-token billing with host earnings
- Failover
- Host onboarding
- Reputation scoring
- Ops console

**Not built yet:**
- Firecracker job isolation
- KYC and weekly host payouts
- Google/GitHub sign-in
- CLI and Python SDK
- Autoscaling, scale-to-zero and burst-to-spot
- The NATS event bus

## Configuration

Every service has working development defaults. In production the services refuse to start without real secrets and TLS. See [`.env.example`](.env.example) for every setting, including Razorpay keys, platform admin emails and public URLs.

## License

Proprietary — Ayeus Private Limited
