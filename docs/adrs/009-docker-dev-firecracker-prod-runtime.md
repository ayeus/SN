# ADR-009: Docker as Development Runtime; Firecracker as Production Runtime

**Date:** 2026-08-10
**Status:** Accepted
**Deciders:** Engineering team

## Context

SpazeNode requires workload isolation for running vLLM inference on host machines. The production target is Firecracker microVMs (ADR-002), but developers need to run the complete control plane workflow on laptops without KVM/Firecracker support (especially macOS).

## Decision

We introduce a `Runtime` interface with two implementations:

- `DockerRuntime` — used in development and on hosts where Firecracker is unavailable
- `FirecrackerRuntime` — used in production on T2/T3 hosts

The runtime is selected via configuration (`SN_RUNTIME=docker|firecracker`), never by compile-time conditionals.

## Consequences

- Developers can run the full deployment lifecycle on any laptop
- `DockerRuntime` does NOT pretend to provide the security guarantees of Firecracker
- Tests must explicitly state which runtime they validate
- The `Runtime` interface is the abstraction boundary; scheduler and coordinator never reference Docker or Firecracker directly

## Related

- ADR-002: Firecracker microVM per job on T2/T3
- ADR-010: Fake GPU mode for development
