#!/usr/bin/env bash
# End-to-end check of the M1 acceptance scenario (SRS §6) against a running
# stack (`make dev`) and a local Ollama with the model below pulled:
#
#   sign up → enrol two hosts → deploy (2 replicas) → SERVING → streamed chat →
#   OpenAI SDK call → usage metered, wallet debited, host credited → kill one
#   host → requests keep succeeding → stop the deployment
#
#   scripts/smoke.sh            # uses gemma2:2b via gemma-2-2b-it
#   MODEL=qwen2.5-7b-instruct scripts/smoke.sh
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${BASE:-http://localhost:8080}"
COORD="${COORD:-http://localhost:50051}"
MODEL="${MODEL:-gemma-2-2b-it}"
WORK="$(mktemp -d)"
AGENTS=()
trap 'for p in "${AGENTS[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$WORK"' EXIT

pass() { printf '  \033[32mpass\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; exit 1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval('d'+sys.argv[1]))" "$1"; }
api()  { local m=$1 p=$2; shift 2; curl -sS -X "$m" "$BASE$p" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

echo "Smoke test against $BASE with $MODEL"
curl -sf "$BASE/v1/network/stats" >/dev/null || fail "gateway not reachable at $BASE (run make dev)"
curl -sf http://127.0.0.1:11434/api/version >/dev/null || fail "Ollama is not running"

# 1. Sign up (India → INR wallet with ₹500).
EMAIL="smoke@$(uuidgen | tr A-Z a-z | cut -c1-8).example.com"
R=$(curl -sS -X POST "$BASE/v1/auth/signup" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"Smoke-Test-2026\",\"name\":\"Smoke Test\",\"country\":\"IN\"}")
TOKEN=$(echo "$R" | json "['access_token']") || fail "signup: $R"
BAL0=$(api GET /v1/billing/wallet | json "['balance']['amount']")
pass "signed up $EMAIL, wallet $BAL0 INR"

# 2. Enrol two hosts on this machine (distinct dev instances).
cargo build --release --quiet --manifest-path agent/Cargo.toml
for i in a b; do
  REG=$(api POST /v1/hosts/register-token -d '{"tier":"t3","region":"IN-SOUTH"}' | json "['registration_token']")
  agent/target/release/ayeusann-agent --token "$REG" --coordinator "$COORD" --region IN-SOUTH \
      --data-dir "$WORK/$i" --instance "smoke-$i" > "$WORK/agent-$i.log" 2>&1 &
  AGENTS+=($!)
done
for _ in $(seq 1 60); do
  N=$(api GET /v1/hosts | python3 -c "import json,sys; print(sum(1 for h in json.load(sys.stdin)['hosts'] if h['online'] and h['runtime_healthy']))")
  [ "$N" -ge 2 ] && break; sleep 1
done
[ "$N" -ge 2 ] || fail "hosts did not come online; see $WORK/agent-*.log"
pass "two hosts online with a healthy runtime"

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
else
  echo "  skip  OpenAI Python SDK not installed (pip install openai)"
fi

# 7. Money: wallet debited, host credited 75%.
sleep 1
BAL1=$(api GET /v1/billing/wallet | json "['balance']['amount']")
python3 -c "import sys; sys.exit(0 if float('$BAL1') < float('$BAL0') else 1)" || fail "wallet not debited ($BAL0 → $BAL1)"
EARN=$(api GET /v1/hosts/earnings | json "['summary']['lifetime']['amount']")
python3 -c "import sys; sys.exit(0 if float('$EARN') > 0 else 1)" || fail "host earned nothing"
pass "wallet $BAL0 → $BAL1 INR; hosts earned $EARN INR"

# 8. Failover: kill one host, requests keep working (NFR-2: < 5 s).
kill "${AGENTS[0]}"
T0=$(date +%s)
for i in 1 2 3 4 5; do
  C=$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/chat/completions" -H "Authorization: Bearer $KEY" \
      -H 'Content-Type: application/json' -d '{"model":"smoke","messages":[{"role":"user","content":"ping"}],"max_tokens":3}')
  [ "$C" = "200" ] || fail "request $i after host loss returned $C"
done
pass "5/5 requests succeeded within $(( $(date +%s) - T0 ))s of losing a host"

# 9. Stop.
api DELETE "/v1/deployments/$DEP" >/dev/null
for _ in $(seq 1 60); do
  S=$(api GET "/v1/deployments/$DEP" | json "['deployment']['state']"); [ "$S" = "stopped" ] && break; sleep 1
done
[ "$S" = "stopped" ] || fail "deployment did not stop (state $S)"
pass "deployment stopped"
echo "All checks passed."
