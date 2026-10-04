#!/usr/bin/env bash
# Checks that a GPU on another machine can join and serve, against a running
# stack (`make dev`) and a local Ollama with the model below pulled.
#
# The "other machine" is a clean Linux container: it has its own hostname,
# filesystem and network, and it enrols with the exact one-line command the
# console hands out. The deployment is then called through this machine's
# network address, the way a second laptop would call it.
#
#   make dist-agent-linux ARCH=arm64   # or amd64: the container's architecture
#   scripts/smoke-remote.sh
#
# The container has no GPU, so the agent reports a simulated one (SN_FAKE_GPU)
# and drives this machine's Ollama. Everything else is the real path.
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${BASE:-http://localhost:8080}"
MODEL="${MODEL:-gemma-2-2b-it}"
NAME="sn-remote-host-$$"

pass() { printf '  \033[32mpass\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; exit 1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval('d'+sys.argv[1]))" "$1"; }
api()  { local m=$1 p=$2; shift 2; curl -sS -X "$m" "$BASE$p" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
cleanup() {
  if [ -n "${TOKEN:-}" ]; then
    [ -n "${DEP:-}" ] && api DELETE "/v1/deployments/$DEP" >/dev/null 2>&1 || true
    [ -n "${HOST:-}" ] && api DELETE "/v1/hosts/$HOST" >/dev/null 2>&1 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "Remote-host smoke test against $BASE with $MODEL"
curl -sf "$BASE/v1/network/stats" >/dev/null || fail "gateway not reachable at $BASE (run make dev)"
curl -sf http://127.0.0.1:11434/api/version >/dev/null || fail "Ollama is not running"
docker info >/dev/null 2>&1 || fail "Docker is not running"
ARCH=$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/;s/aarch64/arm64/')
[ -f "dist/agent/ayeusann-agent-linux-$ARCH" ] || fail "no Linux agent published; run: make dist-agent-linux ARCH=$ARCH"

# 1. The install command must be usable on another machine.
R=$(curl -sS -X POST "$BASE/v1/auth/signup" -H 'Content-Type: application/json' \
    -d "{\"email\":\"remote@$(uuidgen | tr A-Z a-z | cut -c1-8).example.com\",\"password\":\"Remote-Host-2026\",\"name\":\"Remote Host Test\",\"country\":\"IN\"}")
TOKEN=$(echo "$R" | json "['access_token']") || fail "signup: $R"
T=$(api POST /v1/hosts/register-token -d '{"tier":"t3","region":"IN-SOUTH"}')
CMD=$(echo "$T" | json "['commands']['linux_macos']")
SERVER=$(echo "$T" | json "['server_url']")
case "$CMD" in
  *localhost*|*127.0.0.1*) fail "the install command points at this machine only ($SERVER). Is it on a network?" ;;
esac
pass "install command targets $SERVER"

# 2. A clean machine runs it.
docker run -d --name "$NAME" -e SN_FAKE_GPU=true -e SN_RUNTIME_URL=http://host.docker.internal:11434 \
  --add-host host.docker.internal:host-gateway debian:bookworm-slim \
  sh -c "apt-get update -qq >/dev/null && apt-get install -y -qq curl ca-certificates >/dev/null 2>&1 && $CMD" >/dev/null
N=0
for _ in $(seq 1 120); do
  H=$(api GET /v1/hosts)
  N=$(echo "$H" | python3 -c "import json,sys; print(sum(1 for h in json.load(sys.stdin)['hosts'] if h['online'] and h['runtime_healthy']))")
  [ "$N" -ge 1 ] && break; sleep 1
done
[ "$N" -ge 1 ] || { docker logs "$NAME" 2>&1 | tail -30; fail "the remote machine never came online"; }
HOST=$(echo "$H" | json "['hosts'][0]['id']")
pass "remote machine online: $(echo "$H" | python3 -c "import json,sys; h=json.load(sys.stdin)['hosts'][0]; print(h['name'], '/', h['os'], '/', h['gpus'][0]['model'])")"

# 3. A deployment lands on it and answers through the network address.
MID=$(curl -sS "$BASE/v1/models" | python3 -c "import json,sys; print([m['id'] for m in json.load(sys.stdin)['models'] if m['name']=='$MODEL'][0])")
D=$(api POST /v1/deployments -H "Idempotency-Key: $(uuidgen)" \
    -d "{\"model_id\":\"$MID\",\"name\":\"remote\",\"tier\":\"t3\",\"region\":\"IN-SOUTH\",\"min_replicas\":1,\"max_replicas\":1}")
DEP=$(echo "$D" | json "['deployment']['id']") || fail "deploy: $D"
KEY=$(echo "$D" | json "['secret']")
URL=$(echo "$D" | json "['base_url']")
case "$URL" in *localhost*|*127.0.0.1*) fail "the endpoint handed to the customer points at this machine only: $URL" ;; esac
S=pending
for _ in $(seq 1 300); do
  S=$(api GET "/v1/deployments/$DEP" | json "['deployment']['state']")
  [ "$S" = "serving" ] && break
  [ "$S" = "failed" ] && fail "deployment failed: $(api GET "/v1/deployments/$DEP" | json "['deployment']['last_error']")"
  sleep 1
done
[ "$S" = "serving" ] || { docker logs "$NAME" 2>&1 | tail -30; fail "not serving after 5 minutes (state $S)"; }
pass "deployment serving on the remote machine"

OUT=$(curl -sS "$URL/chat/completions" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
      -d '{"model":"remote","messages":[{"role":"user","content":"Reply with the single word: pong"}],"max_tokens":8}')
[ "$(echo "$OUT" | json "['usage']['completion_tokens']")" -gt 0 ] || fail "no completion through $URL: $OUT"
pass "completion through $URL"

sleep 1
SERVED=$(api GET "/v1/hosts/$HOST/activity" | json "['summary']['lifetime']['requests']")
[ "$SERVED" -ge 1 ] || fail "the remote machine's activity shows no request"
pass "the remote machine served $SERVED request(s)"

echo "All checks passed."
