# SpazeNode

**Data Center as a Service for AI Compute**

SpazeNode aggregates underutilized GPUs — data centres, college labs, workstations, and personal machines — into managed AI infrastructure. Customers deploy open-source models and receive production-grade inference endpoints. Hosts earn revenue from idle GPU capacity.

## Quick Start

```bash
cp .env.example .env
make dev
```

See [docs/development.md](docs/development.md) for the full setup guide.

## Architecture

```
Customer → API Gateway → Control API → Scheduler → Coordinator → Host Agent
                                                                      ↓
                         Inference GW → Request Router → vLLM Replica
                                                              ↓
                                                   Usage Event → Billing
```

- **Control Plane:** Go services (gateway, control-api, scheduler, coordinator, billing-meter, trust-engine)
- **Inference Plane:** Go (inference-gateway, request-router) → vLLM on host GPUs
- **Host Agent:** Rust binary (GPU detection, benchmarking, workload isolation, WireGuard)
- **Data:** PostgreSQL, Redis, NATS, S3-compatible object storage
- **Frontend:** Next.js (customer console, host console, marketing site)

See [docs/](docs/) for architecture documentation, ADRs, and implementation guide.

## Repository Structure

```
spazenode/
├── proto/              # gRPC/protobuf contracts
├── services/           # Go backend services (8 services)
├── agent/              # Rust host agent
├── runtime/            # vLLM images and microVM configs
├── web/                # Next.js frontends
├── cli/                # Go CLI
├── sdk/python/         # Python SDK
├── schema/             # SQL migrations and seeds
├── deploy/             # Docker Compose, K8s, Terraform
├── ops/                # Dashboards, alerts, runbooks
└── docs/               # Documentation and ADRs
```

## License

Proprietary — Spazor Private Limited
