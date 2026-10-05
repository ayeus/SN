#!/usr/bin/env bash
# Proves the private installation works, on a throwaway copy of it.
#
# It creates a scratch installation (its own secrets, database, backups, project
# name and port), so a real one on this computer is never touched, then:
#
#   init → up → only one port is published → nobody can sign up uninvited →
#   the owner code makes one operator → both smoke tests with no GPU, each
#   invited by the operator → backup → the dump restores → replace the database
#   from it → a damaged dump changes nothing → a locked-out operator gets back
#   in → a simulated GPU is refused once the test switch is off → everything is
#   removed
#
#   scripts/private-check.sh
#   KEEP=1 scripts/private-check.sh      # leave the stack running to look at
#
# Needs Docker, and a published agent for the smoke tests (make dist-agent,
# make dist-agent-linux ARCH=...). CI runs this on every push.
set -euo pipefail
cd "$(dirname "$0")/.."

export AYEUSANN_PLATFORM_DIR="${AYEUSANN_PLATFORM_DIR:-$(mktemp -d)}"
export PRIVATE_PROJECT="${PRIVATE_PROJECT:-ayeusann-private-check}"
PORT="${PRIVATE_CHECK_PORT:-8091}"
BASE="http://localhost:$PORT"

pass() { printf '  \033[32mpass\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; exit 1; }
cleanup() {
  local code=$?
  if [ "$code" != 0 ]; then
    mkdir -p logs
    scripts/private.sh compose logs --no-color --tail=300 > logs/private-stack.log 2>&1 || true
    echo "The stack's logs were saved to logs/private-stack.log" >&2
  fi
  if [ "${KEEP:-0}" = 1 ]; then
    echo "Left running at $BASE (files in $AYEUSANN_PLATFORM_DIR). Remove with:"
    echo "  AYEUSANN_PLATFORM_DIR=$AYEUSANN_PLATFORM_DIR PRIVATE_PROJECT=$PRIVATE_PROJECT scripts/private.sh compose down -v"
    return
  fi
  scripts/private.sh compose down -v >/dev/null 2>&1 || true
  rm -rf "$AYEUSANN_PLATFORM_DIR"
}
trap cleanup EXIT

echo "Private installation check ($PRIVATE_PROJECT on port $PORT)"

# 1. A fresh installation, and init never overwrites one.
scripts/private.sh init --port "$PORT" >/dev/null
ENV_FILE="$AYEUSANN_PLATFORM_DIR/private.env"
[ -f "$ENV_FILE" ] || fail "init wrote no secrets file"
MODE=$(stat -c '%a' "$ENV_FILE" 2>/dev/null || stat -f '%Lp' "$ENV_FILE")   # GNU, then BSD
[ "$MODE" = 600 ] || fail "the secrets file is readable by others (mode $MODE)"
BEFORE=$(cksum < "$ENV_FILE")
scripts/private.sh init --port 1 >/dev/null
[ "$(cksum < "$ENV_FILE")" = "$BEFORE" ] || fail "a second init changed the secrets file"
pass "init wrote the secrets file (mode 600) and a second init left it alone"

# 2. It starts, on real secrets, with the smoke tests' simulated GPUs allowed.
ALLOW_FAKE_GPU=true scripts/private.sh up >/dev/null 2>&1 || { scripts/private.sh compose logs --tail=40; fail "the stack did not start"; }
curl -sf "$BASE/readyz" >/dev/null || fail "the gateway does not answer at $BASE"
pass "the stack is up at $BASE"

# 3. One way in. Nothing but the gateway may be published.
PUBLISHED=$(docker ps --filter "label=com.docker.compose.project=$PRIVATE_PROJECT" --format '{{.Label "com.docker.compose.service"}} {{.Ports}}' | grep -- '->' || true)
[ "$(echo "$PUBLISHED" | grep -c .)" = 1 ] || fail "expected exactly one published service, got: $PUBLISHED"
case "$PUBLISHED" in
  gateway*":$PORT->8080/tcp"*) ;;
  *) fail "the published port is not the gateway's: $PUBLISHED" ;;
esac
[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/metrics")" = 404 ] || fail "/metrics is served on the public port"
pass "only the gateway is published; the database, Redis and internal services are not"

# 4. The door. Accounts are by invitation, and the first one needs the owner code.
PW="Private-Check-2026"
OWNER_EMAIL="owner@private-check.example.com"
post() { curl -sS -o "$AYEUSANN_PLATFORM_DIR/last.json" -w '%{http_code}' -X POST "$BASE$1" -H 'Content-Type: application/json' ${3:+-H "Authorization: Bearer $3"} -d "$2"; }
field() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])" "$AYEUSANN_PLATFORM_DIR/last.json" "$1"; }
account() { printf '{"email":"%s","password":"%s","name":"Check","country":"IN"%s}' "$1" "${3:-$PW}" "${2:+,$2}"; }
# JSON bodies are built here rather than inline: bash 3.2 (macOS) brace-expands
# a literal {a,b} written inside "$(...)".
pair() { printf '{"%s":"%s","%s":"%s"}' "$1" "$2" "$3" "$4"; }
# op: a fresh operator session (they last 15 minutes).
op() {
  local body
  body=$(pair email "$OWNER_EMAIL" password "${OWNER_PW:-$PW}")
  [ "$(post /v1/auth/login "$body")" = 200 ] || fail "the operator could not sign in"
  field access_token
}
# invited EMAIL: create an account the way a friend gets one. Prints the status.
invited() {
  [ "$(post /v1/admin/invites '{"note":"private-check"}' "$(op)")" = 201 ] || { echo 000; return; }
  post /v1/auth/signup "$(account "$1" "\"invite\":\"$(field token)\"")"
}

[ "$(post /v1/auth/signup "$(account stranger@private-check.example.com)")" = 403 ] || fail "someone signed up without an invitation"
[ "$(post /v1/auth/signup "$(account guesser@private-check.example.com '"owner_code":"0000-not-the-code"')")" = 403 ] || fail "a wrong owner code was accepted"
OWNER_LINK=$(scripts/private.sh status | sed -n 's/.*\(http[^ ]*signup?owner=[^ ]*\).*/\1/p')
[ -n "$OWNER_LINK" ] || fail "status does not show the owner how to create the first account"
OWNER_CODE="${OWNER_LINK##*owner=}"
[ "$(post /v1/auth/signup "$(account "$OWNER_EMAIL" "\"owner_code\":\"$OWNER_CODE\"")")" = 201 ] || fail "the owner code did not create the first account: $(cat "$AYEUSANN_PLATFORM_DIR/last.json")"
[ "$(post /v1/auth/signup "$(account second@private-check.example.com "\"owner_code\":\"$OWNER_CODE\"")")" = 403 ] || fail "the owner code worked twice"
case "$(scripts/private.sh status)" in *"signup?owner="*) fail "status still prints the owner code after it was used" ;; esac
ME=$(curl -sS "$BASE/v1/auth/me" -H "Authorization: Bearer $(op)")
case "$ME" in *'"is_platform_admin":true'*) ;; *) fail "the owner is not the operator: $ME" ;; esac
pass "nobody signs up uninvited; the owner code made exactly one operator and is spent"

# 5. The whole product path, with no GPU. Each test is invited by the operator.
export BASE
OPERATOR_TOKEN="$(op)" COORD_DIRECT="$BASE" scripts/smoke-fake.sh || fail "smoke tests failed against the private stack"

# 6. Backups: one is taken on its own, one on request, and it restores.
BACKUPS="$AYEUSANN_PLATFORM_DIR/backups"
ls "$BACKUPS"/daily/*.dump >/dev/null 2>&1 || fail "no dump was taken after the first start"
[ -f "$BACKUPS/private.env" ] || fail "the secrets file was not copied beside the backups"
login()  { post /v1/auth/login "$(pair email "$1" password "$PW")"; }
[ "$(invited before@restore.example.com)" = 201 ] || fail "could not create the account that should survive a restore"
sleep 1   # dumps are named to the second
scripts/private.sh backup >/dev/null || fail "backup failed"
DUMP=$(ls -1t "$BACKUPS"/daily/*.dump | head -1)
[ "$(invited after@restore.example.com)" = 201 ] || fail "could not create the account that should not survive a restore"
pass "a dump is taken after the first start and on request"

OUT=$(scripts/private.sh restore "$DUMP" 2>&1) || { echo "$OUT"; fail "the dump did not restore into a scratch database"; }
case "$OUT" in *"restores cleanly"*) ;; *) echo "$OUT"; fail "restore check did not report success" ;; esac
[ "$(login after@restore.example.com)" = 200 ] || fail "checking a dump changed the live database"
pass "the dump restores into a scratch database and the live one is untouched"

scripts/private.sh restore "$DUMP" --replace --yes >/dev/null 2>&1 || fail "replacing the database from the dump failed"
[ "$(login before@restore.example.com)" = 200 ] || fail "an account from before the dump is missing after the restore"
[ "$(login after@restore.example.com)" = 401 ] || fail "an account created after the dump survived the restore"
ls "$BACKUPS"/before-restore/*.dump >/dev/null 2>&1 || fail "no dump of the replaced database was kept"
pass "replacing the database from the dump works, and the replaced one was saved first"

head -c 4000 "$DUMP" > "$AYEUSANN_PLATFORM_DIR/damaged.dump"
if scripts/private.sh restore "$AYEUSANN_PLATFORM_DIR/damaged.dump" --replace --yes >/dev/null 2>&1; then
  fail "a damaged dump was accepted"
fi
[ "$(login before@restore.example.com)" = 200 ] || fail "a damaged dump damaged the live database, or the services did not come back"
pass "a damaged dump is refused, the database is kept and the services come back"

# 7. An operator who forgot their password gets back in from this computer.
LINK=$(scripts/private.sh reset-link "$OWNER_EMAIL" | sed -n 's/.*reset?token=\([0-9a-f]*\).*/\1/p')
[ -n "$LINK" ] || fail "reset-link printed no link"
OWNER_PW="Recovered-Check-2027"
RESET=$(pair token "$LINK" password "$OWNER_PW")
[ "$(post /v1/auth/password/reset "$RESET")" = 200 ] || fail "the reset link did not work"
op >/dev/null
if scripts/private.sh reset-link nobody@private-check.example.com >/dev/null 2>&1; then fail "a reset link was made for an account that does not exist"; fi
pass "a locked-out operator gets a reset link from this computer"

# 8. Without the test switch, a simulated GPU cannot join.
scripts/private.sh up >/dev/null 2>&1 || fail "restart without ALLOW_FAKE_GPU failed"
[ "$(login before@restore.example.com)" = 200 ] || fail "a member could not sign in"
TOKEN=$(field access_token)
REG=$(curl -sS -X POST "$BASE/v1/hosts/register-token" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"tier":"t3"}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['registration_token'])")
LOG="$AYEUSANN_PLATFORM_DIR/simulated.log"
SN_FAKE_GPU=true SN_RUNTIME_URL=http://127.0.0.1:9 agent/target/release/ayeusann-agent --token "$REG" --coordinator "$BASE" \
  --region IN-SOUTH --data-dir "$AYEUSANN_PLATFORM_DIR/simulated" --instance "private-check-$$" > "$LOG" 2>&1 &
AGENT=$!
for _ in $(seq 1 20); do grep -q "simulated GPU" "$LOG" 2>/dev/null && break; sleep 1; done
kill "$AGENT" 2>/dev/null || true
grep -q "simulated GPU" "$LOG" || { tail -5 "$LOG"; fail "a simulated GPU was not refused"; }
T2=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/hosts/register-token" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"tier":"t2"}')
[ "$T2" = 403 ] || fail "a member enrolled a machine above Tier 3 on a private network ($T2)"
pass "simulated GPUs are refused, and machines join as Tier 3"

echo "All checks passed."
