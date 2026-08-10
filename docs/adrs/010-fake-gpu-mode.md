# ADR-010: Fake GPU Mode for Development

**Date:** 2026-08-10
**Status:** Accepted
**Deciders:** Engineering team

## Context

The SpazeNode control plane needs to be testable on development machines that lack NVIDIA GPUs. The complete deployment workflow (register host → detect GPU → schedule → deploy → serve → meter) must be exercisable locally.

## Decision

Implement a "fake GPU mode" activated by `SN_FAKE_GPU=true`. In this mode:

1. The agent reports a synthetic GPU inventory (e.g., 1× RTX 4090, 24GB VRAM)
2. The benchmark suite returns plausible synthetic scores
3. The runtime uses `DockerRuntime` (ADR-009) instead of Firecracker
4. The inference server is a mock that echoes responses with synthetic token counts

## Constraints

- Fake GPU mode MUST be clearly identifiable in logs and metrics (`gpu_mode=fake`)
- Fake GPU mode MUST NOT pretend that real GPU execution has been validated
- Real GPU testing remains a separate environment (staging/production)
- Mock implementations MUST be behind explicit interfaces; production code cannot import mock packages

## Consequences

- `make dev` works on any machine without CUDA
- CI can run the complete integration test with fake GPU mode
- The fake/real boundary is the `GPUDetector` and `Runtime` interfaces
