# Archive

Code kept for reference that is no longer built. Go tooling ignores
directories starting with `_`.

- `router-service/` — the standalone request router. Superseded by ADR-011:
  replica selection now lives in `services/inference-gateway/routing.go`, and
  requests reach hosts through the coordinator's tunnel instead of the
  WireGuard overlay IPs this service targeted. Its `worker.go` ran a proxy to a
  local Ollama inside the router process, which is why inference only ever ran
  on the control-plane machine. It holds uncommitted local edits, so it was
  moved rather than deleted; remove it once you no longer need it.
- `legacy-frontend/` — the previous UIs: `frontend-vite/` (a single 1,700-line
  React component with hard-coded hardware numbers, a hard-coded host id and
  developer name, and a fake "Mesh Online" badge) and `web/` (the vanilla-JS
  page it replaced, six stale build bundles, old installers, and a checked-in
  agent binary). Replaced by the Next.js console in `web/console`.
