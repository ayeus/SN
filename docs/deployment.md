# Production deployment

One machine, Docker Compose. Caddy terminates TLS and is the only container that
publishes ports; the gateway, the services, Postgres and Redis stay on the
internal network.

```
Internet ──► Caddy :443 ──► app domain          ──► gateway ──► services, console
                        └─► coordinator domain  ──► coordinator gRPC (h2c)
```

## What you need

- A Linux machine with Docker and the Compose plugin. 2 vCPU and 4 GB of RAM is
  enough to start: the models run on hosts, not here.
- Two DNS names pointing at it, for example `app.example.com` (console and API)
  and `coordinator.example.com` (host agents).
- Ports 80 and 443 open. Caddy needs both to obtain certificates.

## First deploy

```bash
git clone <repo-url> && cd <repo>
cp deploy/compose/.env.prod.example deploy/compose/.env.prod
```

Fill in `.env.prod`:

| Setting | Value |
|---|---|
| `APP_DOMAIN`, `COORDINATOR_DOMAIN` | The two DNS names |
| `ACME_EMAIL` | Where Let's Encrypt sends expiry notices |
| `JWT_SECRET`, `INTERNAL_SERVICE_SECRET`, `POSTGRES_PASSWORD` | `openssl rand -hex 32`, a different one for each |
| `MANIFEST_SIGNING_KEY` | `openssl rand -base64 32` |
| `OWNER_CODE` | `openssl rand -hex 12`. The first account presents it and becomes the operator |
| `SMTP_*` | A mail relay. Without one, password reset emails cannot be sent |

Then:

```bash
make prod-up        # build images, migrate, start, wait until healthy
make prod-status
```

Open `https://<APP_DOMAIN>/signup?owner=<OWNER_CODE>` and create your account. It
becomes the operator: the account that invites everyone else (Operations >
Invitations) and sees the operations console. The code works once. Sign-up is
by invitation unless `SIGNUP_MODE=open` is set.

**Back up `MANIFEST_SIGNING_KEY`.** Hosts pin it when they enrol. If it changes,
every enrolled host refuses jobs until it enrols again.

## Host agent binaries

The installer at `https://<APP_DOMAIN>/install.sh` downloads a prebuilt agent
from `/downloads/`, which the gateway serves from `dist/agent/` on this machine.
Publish the platforms your hosts run:

```bash
make dist-agent-linux                # linux/amd64, built in Docker
make dist-agent-linux ARCH=arm64
```

Tagged releases also attach binaries for Linux and Apple-silicon macOS (see
`.github/workflows/release.yml`); download them into `dist/agent/` with
`gh release download <tag> -D dist/agent -p 'ayeusann-agent-*'`.

New files are served immediately; no restart is needed. A platform without a
binary falls back to building from source on the host.

## Agent updates

Enrolled machines update themselves from releases you sign. Create the release
key once, away from the server (`go run ./cmd/release-sign keygen -out
release.key` prints the public half), put the public half in
`RELEASE_PUBLIC_KEY`, and publish with `make release-agent RELEASE_KEY=release.key`
after building the agents. The server never holds the private key. The full
description is in [private-network.md](private-network.md#updating-the-agents-on-other-peoples-machines).

## Updating

```bash
git pull
make prod-up
```

This rebuilds the images, applies pending migrations and seed changes, and
replaces the containers. Connected agents reconnect on their own. Requests in
flight on a restarted service fail, so deploy when traffic is low.

## Operating

| Task | Command |
|---|---|
| Logs | `make prod-logs` |
| Health | `make prod-status` |
| Stop (data kept) | `make prod-down` |
| Database shell | `docker compose -f deploy/compose/docker-compose.prod.yml --env-file deploy/compose/.env.prod exec postgres psql -U ayeusann ayeusann` |
| Backup | `... exec -T postgres pg_dump -U ayeusann -Fc ayeusann > backup.dump` |
| Restore | `... exec -T postgres pg_restore -U ayeusann -d ayeusann --clean < backup.dump` |

Postgres data lives in the `ayeusann_pgdata` volume and certificates in
`ayeusann_caddydata`. `docker compose down -v` deletes both. Schedule the
backup; nothing here does it for you.

Each service exposes Prometheus metrics at `/metrics` on its internal port. The
gateway, whose port is the public one, serves them on port 9080 instead. None
are reachable from outside.

## What the edge does

- Redirects HTTP to HTTPS and renews certificates.
- Sets HSTS, `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`,
  `Permissions-Policy` and a `Content-Security-Policy` that forbids framing.
- Passes the client address to the services (`TRUST_PROXY_HEADERS=true`), which
  the login abuse limits depend on. A forwarded-for header sent by a
  client is discarded.

## Limits of this setup

- One machine: no redundancy for the control plane or the database.
- A dedicated inference hostname (`INFERENCE_HOST`, with
  `{deployment}.inference.example.com` subdomains) needs a wildcard certificate,
  which means a Caddy build with your DNS provider's plugin. Until then,
  customers use `https://<APP_DOMAIN>/v1`.
- `deploy/k8s` and `deploy/terraform` are empty placeholders.
