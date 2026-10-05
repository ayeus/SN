# ADR-011: Inference over the agent stream, reconciling control loops, router in the inference gateway

- **Status:** Accepted (September 2026)
- **Supersedes in part:** ADR-001 for the data path at MVP scale; Architecture §3 container boundaries for the request router
- **Context documents:** 03_System_Architecture §3–§9, 04_UML_Diagrams §4 and §8, 02_SRS FR-33/FR-40/FR-41/FR-53

## Context

An audit in September 2026 found that no deployment had ever reached SERVING:

1. **Nothing dispatched work.** The control API inserted a replica row and returned. The scheduler answered a synchronous HTTP call and did nothing else. The coordinator had a `DispatchManifest` function that no code called.
2. **The agent pretended.** On receiving a manifest it immediately reported SERVING without starting a runtime.
3. **The data path assumed a mesh that did not exist.** The router proxied to `http://<overlay-ip>:8000`, a WireGuard address. The agent generated keys but never created an interface, and no concentrator was deployed. Remote hosts were unreachable.
4. **Inference ran on the control plane.** The router process started a "ReplicaWorker" on :8000 that forwarded to Ollama on the same machine, so the only inference that ever worked never touched a host.
5. **Streaming was simulated.** The router buffered the whole response; the inference gateway split it into words and slept 15 ms between them.

## Decision

### 1. Inference rides the agent's outbound gRPC stream

The agent already holds a long-lived, outbound, authenticated stream to the coordinator. `InferenceRequest` (coordinator → agent) and `InferenceChunk` (agent → coordinator) messages now carry requests and streamed responses over it. The coordinator exposes an internal, service-authenticated `POST /internal/v1/infer` that turns one stream into one HTTP response. Exact token counts travel back as HTTP trailers.

This keeps the principle behind ADR-001: hosts are outbound-only, need no inbound ports, and work behind home and campus NAT. It does so without deploying a WireGuard concentrator. The agent still generates and registers a WireGuard key pair, so the overlay can later carry bulk traffic without re-enrolling hosts.

**Trade-off:** every token crosses the coordinator. At M1–M3 scale (tens to hundreds of GPUs) this is negligible. At Phase-5 scale, add coordinators per PoP with session-affinity routing, or move bulk traffic to the overlay.

### 2. Control is reconciliation against Postgres, not RPC chains

| Loop | Owner | Converges |
|---|---|---|
| Placement / drain / spend cap | scheduler | `deployments.desired_state` → replica rows |
| Dispatch / stop / confirm-timeout / liveness | coordinator | replica rows → agents; heartbeats → host status |
| Rollup | both (`internal/lifecycle`) | replica states → `deployments.state` (UML §4) |

A deployment created while the scheduler is down is placed when it comes back. A replica lost to a dead host is backfilled without anyone asking. Every transition is logged in `deployment_events` (FR-21).

Deviation from UML §8: reservations live in Postgres (`gpus.replica_id`, claimed with a conditional update) instead of Redis with a TTL. Postgres is already the system of record, and one store for one fact avoids a class of divergence bugs. The 60 s "agent confirms within TTL" rule is kept, enforced by the coordinator.

#### Losing the connection is not losing the host

The stream between an agent and the coordinator drops for ordinary reasons: Wi-Fi, a coordinator restart, a laptop lid, the platform's own computer going to sleep. The models stay loaded through all of them, so a dropped connection must not be treated as a dead host.

- **Reconnect is a handshake, not a reset.** An agent with the `replica-report` capability lists, in `RegisterRequest.replicas`, the replicas it is still serving, each checked against its runtime just before. The coordinator (`reconcileOnRegister`) leaves a replica both sides agree on exactly as it is, re-sends what the agent does not have, and tells the agent to stop anything the platform has given up on. An agent without the capability is sent everything again, as before.
- **A closed session ends its stream.** When the coordinator closes a session (a newer one replaced it, or the host was declared offline) the stream's handler returns, the agent sees the stream end and reconnects. A session never lingers in the registry looking connected.
- **Silence is judged only when it means something.** For 90 s after the coordinator starts, and after any stall of its control loop longer than 10 s (measured on the wall clock, so that a sleeping computer counts), no host is declared offline and no unconfirmed job is expired. Agents back off at most 30 s between attempts, so they are back well inside that.
- **Offline is two steps for a serving replica.** When a host misses its heartbeats it is marked offline at once and its connection is dropped, but a replica it was serving only becomes `degraded` and keeps its GPU. If the host returns within 60 s the replica carries on untouched; otherwise it is failed and the scheduler replaces it. A replica that was still starting is failed immediately.
- **The agent keeps its side.** When a connection ends the agent aborts whatever was tied to it (a replica still starting, a request in flight) and keeps what is serving. Asked to start a replica it already serves, it answers at once. After ten minutes with no platform at all (`SN_ORPHAN_UNLOAD_SECS`) it unloads everything and gives the GPU's memory back to its owner.

The cost is that a host that really died is replaced about a minute later than before. A deployment with two replicas keeps answering from the other throughout.

### 3. The request router is a package inside the inference gateway

The router's responsibilities (least-outstanding selection, 5 s unhealthy cool-down, retry before first byte, tier ordering) are unchanged and live in `services/inference-gateway/routing.go`. Architecture §9 already places both in the same PoP VM. A separate process added a network hop and had to buffer the response to decide on retries.

### 4. Metering is in-process and transactional

The inference gateway records usage through `internal/billing` in one Postgres transaction: the usage event (customer charge and host accrual in one row), the wallet debit, and the replica counters. Previously a fire-and-forget HTTP POST to billing-meter could lose events. The documented event bus (NATS/Redpanda) is still the scale path; adding it later means publishing from an outbox table, not changing the invariant.

## Consequences

- The M1 loop works end to end: deploy → placed → manifest (Ed25519-signed, FR-53) → runtime pulls, loads and warms → SERVING → streamed inference → metered.
- `services/router` is retired (archived in `_archive/router-service`).
- The coordinator is stateful (it holds sessions). Run one instance per PoP until session-affinity routing exists.
- Operators must deploy the coordinator's gRPC port publicly (TLS required in production), not a WireGuard concentrator.
