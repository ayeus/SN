#!/usr/bin/env bash
# End-to-end check of the M1 acceptance scenario (SRS §6) against a running
# stack (`make dev`) and a local Ollama with the model below pulled:
#
#   sign up → enrol two hosts → deploy (2 replicas) → SERVING → streamed chat →
#   OpenAI SDK call → usage recorded for the customer and the host → kill one
#   the coordinator restarts, then freezes: the models stay loaded through both
#   → kill one host → requests keep succeeding → stop the deployment
#
#   scripts/smoke.sh            # uses gemma2:2b via gemma-2-2b-it
#   MODEL=qwen2.5-7b-instruct scripts/smoke.sh
#
# The agents started here inherit the environment, so SN_RUNTIME_URL points
# them at another runtime and SN_FAKE_GPU=true lets them run without a GPU.
# `make smoke-fake` uses both to run this script with no GPU and no Ollama.
# REQUIRE_SDK=1 fails the run when the OpenAI Python SDK is missing.
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${BASE:-http://localhost:8080}"
# Agents normally connect through the gateway, on the same address as
# everything else. The coordinator's own port still works, so one of the two
# hosts below uses each.
COORD="${COORD:-$BASE}"
COORD_DIRECT="${COORD_DIRECT:-http://localhost:50051}"
MODEL="${MODEL:-gemma-2-2b-it}"
RUNTIME_URL="${SN_RUNTIME_URL:-http://127.0.0.1:11434}"
# How to restart, freeze and thaw the coordinator. The defaults suit `make dev`;
# a stack run another way (the private installation) names its own commands.
COORD_RESTART="${COORD_RESTART:-scripts/dev.sh restart coordinator}"
COORD_FREEZE="${COORD_FREEZE:-kill -STOP \$(scripts/dev.sh pid coordinator)}"
COORD_THAW="${COORD_THAW:-kill -CONT \$(scripts/dev.sh pid coordinator)}"
WORK="$(mktemp -d)"
AGENTS=()
RUN="$(uuidgen | tr A-Z a-z | cut -c1-8)"   # fresh host identities each run
cleanup() {
  # A coordinator left frozen would take the whole stack down with it.
  [ -z "${FROZEN:-}" ] || eval "$COORD_THAW" >/dev/null 2>&1 || true
  # Runs on success and on failure: a leftover running deployment would claim
  # every GPU that connects afterwards.
  if [ -n "${TOKEN:-}" ]; then
    [ -n "${DEP:-}" ] && api DELETE "/v1/deployments/$DEP" >/dev/null 2>&1 || true
    for H in $(api GET /v1/hosts 2>/dev/null | python3 -c "import json,sys; print(' '.join(h['id'] for h in json.load(sys.stdin).get('hosts', [])))" 2>/dev/null); do
      api DELETE "/v1/hosts/$H" >/dev/null 2>&1 || true
    done
  fi
  for p in "${AGENTS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  \033[32mpass\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; exit 1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval('d'+sys.argv[1]))" "$1"; }
api()  { local m=$1 p=$2; shift 2; curl -sS -X "$m" "$BASE$p" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

echo "Smoke test against $BASE with $MODEL"
curl -sf "$BASE/v1/network/stats" >/dev/null || fail "gateway not reachable at $BASE (run make dev)"
curl -sf "$RUNTIME_URL/api/version" >/dev/null || fail "no model runtime at $RUNTIME_URL (start Ollama, or run make smoke-fake)"

# Hosts serve whichever deployment is waiting, so a deployment already short of
# replicas would take the two hosts enrolled below before the smoke deployment.
if docker exec ann-postgres true 2>/dev/null; then
  WAITING=$(docker exec ann-postgres psql -U ayeusann -d "${DEV_DB:-ayeusann_dev}" -Atc "
    SELECT string_agg(d.name || ' (' || o.name || ')', ', ')
    FROM deployments d JOIN organizations o ON o.id = d.org_id
    WHERE d.deleted_at IS NULL AND d.desired_state = 'running' AND d.state <> 'failed'
      AND (SELECT COUNT(*) FROM replicas r WHERE r.deployment_id = d.id
           AND r.state NOT IN ('stopped', 'failed', 'stopping')) < GREATEST(d.min_replicas, 1)")
  [ -z "$WAITING" ] || fail "other deployments are waiting for a host and would take the smoke hosts: $WAITING. Stop or pause them first."
fi

# 1. Sign up. Where accounts are by invitation, OPERATOR_TOKEN (an operator's
# session) lets the test invite itself, the way a person would be invited.
INVITE=""
if [ -n "${OPERATOR_TOKEN:-}" ]; then
  INVITE=$(curl -sS -X POST "$BASE/v1/admin/invites" -H "Authorization: Bearer $OPERATOR_TOKEN" -H 'Content-Type: application/json' \
      -d '{"note":"smoke test"}' | json "['token']") || fail "could not create an invitation with OPERATOR_TOKEN"
fi
EMAIL="smoke@$(uuidgen | tr A-Z a-z | cut -c1-8).example.com"
R=$(curl -sS -X POST "$BASE/v1/auth/signup" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"Smoke-Test-2026\",\"name\":\"Smoke Test\",\"country\":\"IN\",\"invite\":\"$INVITE\"}")
TOKEN=$(echo "$R" | json "['access_token']") || fail "signup: $R"
pass "signed up $EMAIL"

# 2. Enrol two hosts on this machine (distinct dev instances).
cargo build --release --quiet --manifest-path agent/Cargo.toml
for i in a b; do
  REG=$(api POST /v1/hosts/register-token -d '{"tier":"t3","region":"IN-SOUTH"}' | json "['registration_token']")
  ADDR="$COORD"; [ "$i" = b ] && ADDR="$COORD_DIRECT"
  agent/target/release/ayeusann-agent --token "$REG" --coordinator "$ADDR" --region IN-SOUTH \
      --data-dir "$WORK/$i" --instance "smoke-$RUN-$i" > "$WORK/agent-$i.log" 2>&1 &
  AGENTS+=($!)
done
for _ in $(seq 1 60); do
  N=$(api GET /v1/hosts | python3 -c "import json,sys; print(sum(1 for h in json.load(sys.stdin)['hosts'] if h['online'] and h['runtime_healthy']))")
  [ "$N" -ge 2 ] && break; sleep 1
done
[ "$N" -ge 2 ] || fail "hosts did not come online; see $WORK/agent-*.log"
pass "two hosts online with a healthy runtime (one through the gateway at $COORD, one direct at $COORD_DIRECT)"

# 3. Deploy with two replicas.
MID=$(curl -sS "$BASE/v1/models" | python3 -c "import json,sys; print([m['id'] for m in json.load(sys.stdin)['models'] if m['name']=='$MODEL'][0])")
D=$(api POST /v1/deployments -H "Idempotency-Key: $(uuidgen)" \
    -d "{\"model_id\":\"$MID\",\"name\":\"smoke\",\"tier\":\"t3\",\"region\":\"IN-SOUTH\",\"min_replicas\":2,\"max_replicas\":2}")
DEP=$(echo "$D" | json "['deployment']['id']") || fail "deploy: $D"
KEY=$(echo "$D" | json "['secret']")
BASE_URL=$(echo "$D" | json "['base_url']")
START=$(date +%s)
for _ in $(seq 1 300); do
  S=$(api GET "/v1/deployments/$DEP" | json "['deployment']['state']")
  [ "$S" = "serving" ] && break
  [ "$S" = "failed" ] && fail "deployment failed: $(api GET /v1/deployments/$DEP | json "['deployment']['last_error']")"
  sleep 1
done
[ "$S" = "serving" ] || fail "not serving after 5 minutes (state $S)"
pass "deployment serving in $(( $(date +%s) - START ))s"

# 4. Streamed chat completion with the one-time key.
OUT=$(curl -sN "$BASE_URL/chat/completions" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
      -d '{"model":"smoke","messages":[{"role":"user","content":"Say hello in five words."}],"stream":true,"max_tokens":20}')
echo "$OUT" | grep -q 'data: \[DONE\]' || fail "stream did not finish: $OUT"
echo "$OUT" | grep -q '"model":"'"$MODEL"'"' || fail "model name not rewritten"
pass "streamed completion ($(echo "$OUT" | grep -c '^data: {') chunks)"

# 5. Non-streamed call reports exact usage.
U=$(curl -sS "$BASE_URL/chat/completions" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
      -d '{"model":"smoke","messages":[{"role":"user","content":"2+2="}],"max_tokens":5}')
[ "$(echo "$U" | json "['usage']['completion_tokens']")" -gt 0 ] || fail "no usage: $U"
pass "non-streamed completion with usage"

# 6. The OpenAI Python SDK, unchanged except base_url (PRD F-4).
if python3 -c "import openai" 2>/dev/null; then
  python3 - "$BASE_URL" "$KEY" <<'PY' || fail "OpenAI SDK call failed"
import sys
from openai import OpenAI
c = OpenAI(base_url=sys.argv[1], api_key=sys.argv[2])
r = c.chat.completions.create(model="smoke", messages=[{"role": "user", "content": "Hi"}], max_tokens=8)
assert r.choices[0].message.content
assert [m.id for m in c.models.list()] == ["smoke"]
PY
  pass "OpenAI Python SDK works with only base_url changed"
elif [ "${REQUIRE_SDK:-0}" = "1" ]; then
  fail "the OpenAI Python SDK is not installed (pip install openai)"
else
  echo "  skip  OpenAI Python SDK not installed (pip install openai)"
fi

# 7. Usage: every request is recorded for the customer and for the hosts.
sleep 1
REQS=$(api GET /v1/usage/daily | python3 -c "import json,sys; print(sum(d['requests'] for d in json.load(sys.stdin)['days']))")
[ "$REQS" -ge 2 ] || fail "customer usage shows $REQS requests, expected at least 2"
SERVED=$(api GET /v1/hosts/activity | json "['summary']['lifetime']['requests']")
TOKENS=$(api GET /v1/hosts/activity | json "['summary']['lifetime']['tokens']")
[ "$SERVED" -ge 2 ] && [ "$TOKENS" -gt 0 ] || fail "host activity shows $SERVED requests and $TOKENS tokens"
pass "usage recorded: $REQS requests for the customer; hosts served $SERVED requests, $TOKENS tokens"

# 8. The platform restarts, and then stalls the way a sleeping laptop does.
# The hosts only lose their connection, so the models must stay loaded: the
# same two replicas, nothing sent again, nothing reloaded, and answers again
# within 30 s.
ask() {
  curl -s -m 20 -o /dev/null -w '%{http_code}' "$BASE_URL/chat/completions" -H "Authorization: Bearer $KEY" \
    -H 'Content-Type: application/json' -d '{"model":"smoke","messages":[{"role":"user","content":"ping"}],"max_tokens":3}'
}
serving() { api GET "/v1/deployments/$DEP/replicas" | python3 -c "import json,sys; print(' '.join(sorted(r['id'] for r in json.load(sys.stdin)['replicas'] if r['state']=='serving')))"; }
last_event() { api GET "/v1/deployments/$DEP/logs" | python3 -c "import json,sys; e=json.load(sys.stdin)['events']; print(e[-1]['id'] if e else 0)"; }
since() { api GET "/v1/deployments/$DEP/logs?after=$1" | python3 -c "import json,sys; print('; '.join(e['message'] for e in json.load(sys.stdin)['events']))"; }
# survived WHAT: both hosts are back and serving the replicas they had before,
# with no event logged for the deployment in between.
survived() {
  local t0 c="" n=0 now
  t0=$(date +%s)
  # Both replicas answer, not just one: each is asked in turn by the router.
  for _ in $(seq 1 60); do
    c=$(ask); if [ "$c" = 200 ]; then n=$((n + 1)); else n=0; fi
    [ "$n" -ge 4 ] && break
    [ $(( $(date +%s) - t0 )) -ge 30 ] && break
    sleep 0.5
  done
  [ "$n" -ge 4 ] || fail "$1: requests did not work again within 30 s (last answer $c)"
  now=$(serving)
  [ "$now" = "$REPLICAS" ] || fail "$1: the serving replicas changed: before [$REPLICAS], after [$now]"
  [ "$(last_event)" = "$EVENT" ] || fail "$1: the deployment was disturbed: $(since "$EVENT")"
  pass "$1: same replicas, nothing reloaded, answering again after $(( $(date +%s) - t0 ))s"
}
REPLICAS=$(serving)
[ "$(echo "$REPLICAS" | wc -w | tr -d ' ')" = 2 ] || fail "expected two serving replicas before the restart, got [$REPLICAS]"
EVENT=$(last_event)

eval "$COORD_RESTART" >/dev/null || fail "could not restart the coordinator ($COORD_RESTART)"
survived "coordinator restarted"

FROZEN=1
eval "$COORD_FREEZE" || fail "could not freeze the coordinator ($COORD_FREEZE)"
sleep 25
eval "$COORD_THAW" || fail "could not thaw the coordinator ($COORD_THAW)"
FROZEN=""
survived "coordinator frozen for 25 s"
# The grace period after a stall is not a blind spot: both hosts are still there.
N=$(api GET /v1/hosts | python3 -c "import json,sys; print(sum(1 for h in json.load(sys.stdin)['hosts'] if h['online'] and h['runtime_healthy']))")
[ "$N" -ge 2 ] || fail "only $N of the two hosts is online after the freeze"

# 9. Failover: kill one host, requests keep working (NFR-2: < 5 s).
kill "${AGENTS[0]}"
T0=$(date +%s)
for i in 1 2 3 4 5; do
  C=$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/chat/completions" -H "Authorization: Bearer $KEY" \
      -H 'Content-Type: application/json' -d '{"model":"smoke","messages":[{"role":"user","content":"ping"}],"max_tokens":3}')
  [ "$C" = "200" ] || fail "request $i after host loss returned $C"
done
pass "5/5 requests succeeded within $(( $(date +%s) - T0 ))s of losing a host"

# 10. Stop.
api DELETE "/v1/deployments/$DEP" >/dev/null
for _ in $(seq 1 60); do
  S=$(api GET "/v1/deployments/$DEP" | json "['deployment']['state']"); [ "$S" = "stopped" ] && break; sleep 1
done
[ "$S" = "stopped" ] || fail "deployment did not stop (state $S)"
pass "deployment stopped"

echo "All checks passed."
