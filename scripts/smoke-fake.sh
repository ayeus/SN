#!/usr/bin/env bash
# Runs both end-to-end smoke tests with no GPU and no Ollama.
#
# A stand-in runtime (cmd/fake-runtime) takes Ollama's place and the agents
# report a simulated GPU. Everything between them is the real code: installers,
# enrolment, placement, signed jobs, the inference tunnel and usage records.
# Needs the dev stack running (`make dev` or `scripts/dev.sh services`).
#
#   scripts/smoke-fake.sh
#   SMOKE_REMOTE=0 scripts/smoke-fake.sh    # skip the second-machine test (it needs Docker)
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${FAKE_RUNTIME_PORT:-11435}"
mkdir -p bin logs
go build -o bin/fake-runtime ./cmd/fake-runtime

# All interfaces, so the container that plays the second machine can reach it.
./bin/fake-runtime -addr "0.0.0.0:$PORT" > logs/fake-runtime.log 2>&1 &
FAKE=$!
trap 'kill "$FAKE" 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$PORT/api/version" >/dev/null && break
  sleep 0.1
done
curl -sf "http://127.0.0.1:$PORT/api/version" >/dev/null || { echo "the stand-in runtime did not start; see logs/fake-runtime.log" >&2; exit 1; }

export SN_RUNTIME_URL="http://127.0.0.1:$PORT"
export SN_FAKE_GPU=true
export REMOTE_RUNTIME_URL="http://host.docker.internal:$PORT"

scripts/smoke.sh
if [ "${SMOKE_REMOTE:-1}" = "1" ]; then
  scripts/smoke-remote.sh
fi
