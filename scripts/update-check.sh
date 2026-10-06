#!/usr/bin/env bash
# Proves the agent's self-update, end to end, against a running dev stack
# (`make dev` or `scripts/dev.sh services`), with no GPU.
#
#   enrol version A → publish B, signed: the host is running B within a minute
#   → a release whose binary does not start is refused → a binary changed after
#   signing is refused → a release signed by someone else never reaches the
#   host → a version that cannot connect is put back and left alone → a
#   machine enrolled without a release key never adopts one from the platform
#
# It uses its own release key and its own downloads folder, and restarts the
# gateway and coordinator to use them; both are put back at the end.
#
#   scripts/update-check.sh
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${BASE:-http://localhost:8080}"
W="$(mktemp -d)"
DL="$W/downloads"; BIN="$W/bin"; mkdir -p "$DL" "$BIN" "$W/data"
AGENT_PID=""; FAKE=""
OS=$(uname -s | tr A-Z a-z); ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
ARTIFACT="ayeusann-agent-$OS-$ARCH"
A=$(cat VERSION); B=9.9.1

pass() { printf '  \033[32mpass\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; [ -f "$W/agent.log" ] && sed 's/\x1b\[[0-9;]*m//g' "$W/agent.log" | grep -v '^ \|^$' | tail -15; exit 1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval('d'+sys.argv[1]))" "$1"; }
api()  { local m=$1 p=$2; shift 2; curl -sS -X "$m" "$BASE$p" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
cleanup() {
  [ -z "$AGENT_PID" ] || kill "$AGENT_PID" 2>/dev/null || true
  pkill -f "$BIN/ayeusann-agent" 2>/dev/null || true
  [ -z "$FAKE" ] || kill "$FAKE" 2>/dev/null || true
  if [ -n "${TOKEN:-}" ] && [ -n "${HOST:-}" ]; then api DELETE "/v1/hosts/$HOST" >/dev/null 2>&1 || true; fi
  # Back to the stack's own downloads folder and no release key.
  scripts/dev.sh restart gateway >/dev/null 2>&1 || true
  scripts/dev.sh restart coordinator >/dev/null 2>&1 || true
  rm -rf "$W"
}
trap cleanup EXIT

echo "Self-update check against $BASE (version $A, then $B)"
curl -sf "$BASE/v1/network/stats" >/dev/null || fail "gateway not reachable at $BASE (run make dev)"

# Two real builds of the agent that differ only in the version stamped in.
cargo build --release --quiet --manifest-path agent/Cargo.toml
SN_AGENT_VERSION=$B cargo build --release --quiet --manifest-path agent/Cargo.toml --target-dir agent/target/next
NEXT=agent/target/next/release/ayeusann-agent
[ "$(agent/target/release/ayeusann-agent --version)" = "ayeusann-agent $A" ] || fail "the current build does not report version $A"
[ "$("$NEXT" --version)" = "ayeusann-agent $B" ] || fail "the next build does not report version $B"

# A release key for this run, and a stack that uses it.
go build -o "$W/release-sign" ./cmd/release-sign
PUB=$("$W/release-sign" keygen -out "$W/release.key")
"$W/release-sign" keygen -out "$W/other.key" >/dev/null
sign() { "$W/release-sign" sign -dir "$DL" -version "$1" -key "${2:-$W/release.key}" >/dev/null; }
DOWNLOADS_DIR="$DL" scripts/dev.sh restart gateway >/dev/null
DOWNLOADS_DIR="$DL" RELEASE_PUBLIC_KEY="$PUB" scripts/dev.sh restart coordinator >/dev/null
for _ in $(seq 1 30); do curl -sf "$BASE/v1/network/stats" >/dev/null && break; sleep 1; done

go build -o bin/fake-runtime ./cmd/fake-runtime
./bin/fake-runtime -addr 127.0.0.1:11439 > "$W/fake.log" 2>&1 & FAKE=$!

R=$(curl -sS -X POST "$BASE/v1/auth/signup" -H 'Content-Type: application/json' \
    -d "{\"email\":\"update@$(uuidgen | tr A-Z a-z | cut -c1-8).example.com\",\"password\":\"Update-Check-2026\",\"name\":\"Update Check\",\"country\":\"IN\"}")
TOKEN=$(echo "$R" | json "['access_token']") || fail "signup: $R"
REG=$(api POST /v1/hosts/register-token -d '{"tier":"t3","region":"IN-SOUTH"}' | json "['registration_token']")

# The agent runs from its own folder, as an installed one does, and is allowed
# to replace itself. It keeps its process across an update, so one start is all.
cp agent/target/release/ayeusann-agent "$BIN/ayeusann-agent"
start() {
  SN_SELF_UPDATE=1 SN_FAKE_GPU=true SN_RUNTIME_URL=http://127.0.0.1:11439 "$BIN/ayeusann-agent" ${1:+--token "$1"} \
    --coordinator "$BASE" --region IN-SOUTH --data-dir "$W/data" --instance "update-$$" >> "$W/agent.log" 2>&1 &
  AGENT_PID=$!
}
version() { api GET /v1/hosts | python3 -c "import json,sys; h=json.load(sys.stdin)['hosts']; print((h[0].get('agent_version') or '') + ('' if h[0]['online'] else ' (offline)') if h else 'no host')"; }
# reaches VERSION SECONDS: the host reports that version, online, in time.
reaches() { for _ in $(seq 1 "$2"); do [ "$(version)" = "$1" ] && return 0; sleep 1; done; return 1; }
# stays VERSION SECONDS: it is still that version, online, after that long.
stays() { sleep "$2"; [ "$(version)" = "$1" ]; }
logged() { grep -qa "$1" "$W/agent.log"; }

start "$REG"
reaches "$A" 30 || fail "the host did not come online as $A (it reports: $(version))"
HOST=$(api GET /v1/hosts | json "['hosts'][0]['id']")
pass "enrolled and online as $A"

# 1. A signed release appears. Nobody touches the machine.
cp "$NEXT" "$DL/$ARTIFACT"; sign "$B"
reaches "$B" 90 || fail "the host is not running $B after 90 s (it reports: $(version))"
[ "$("$BIN/ayeusann-agent" --version)" = "ayeusann-agent $B" ] || fail "the installed binary is not $B"
[ "$("$BIN/ayeusann-agent.previous" --version)" = "ayeusann-agent $A" ] || fail "the previous version was not kept beside the new one"
kill -0 "$AGENT_PID" 2>/dev/null || fail "the agent's process did not survive its own update"
python3 -c "import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s.get('update') is None else 1)" "$W/data/instance-update-$$/agent-state.json" \
  || fail "the update was not marked as confirmed after it connected"
pass "published $B: the host installed it by itself, connected, and kept $A beside it"

# 2. A release whose binary does not run here. Properly signed, and refused.
printf '#!/bin/sh\nexit 1\n' > "$DL/$ARTIFACT"; chmod +x "$DL/$ARTIFACT"; sign 9.9.2
for _ in $(seq 1 60); do logged "does not start on this machine" && break; sleep 1; done
logged "does not start on this machine" || fail "a release that does not start was not refused"
stays "$B" 3 || fail "after a release that does not start, the host reports: $(version)"
pass "a signed release that does not start is refused; the host stays on $B"

# 3. The binary is swapped for another after the release was signed.
cp "$NEXT" "$DL/$ARTIFACT"; sign 9.9.3; printf 'x' >> "$DL/$ARTIFACT"
for _ in $(seq 1 60); do logged "larger than the release says\|not the one that was signed" && break; sleep 1; done
logged "larger than the release says\|not the one that was signed" || fail "a binary changed after signing was not refused"
stays "$B" 3 || fail "after a tampered binary, the host reports: $(version)"
pass "a binary changed after signing is refused"

# 4. Someone else signs a release. The platform does not pass it on at all.
cp "$NEXT" "$DL/$ARTIFACT"; sign 9.9.4 "$W/other.key"
sleep 20
grep -qaF "version=9.9.4" "$W/agent.log" && fail "a release signed by another key reached the host"
grep -qa "the published agent release was ignored" logs/coordinator.log || fail "the coordinator did not report the release it ignored"
stays "$B" 1 || fail "after a release signed by another key, the host reports: $(version)"
pass "a release signed by another key is ignored by the platform and never reaches the host"

# 5. A version that cannot connect is put back, and is left alone afterwards.
# The agent is stopped and its state made to say what it would after the new
# version had spent its ten minutes failing to connect, several times over.
kill "$AGENT_PID"; wait "$AGENT_PID" 2>/dev/null || true
python3 - "$W/data/instance-update-$$/agent-state.json" "$A" "$B" <<'PY'
import json, sys, time
p, a, b = sys.argv[1:]
s = json.load(open(p))
s["update"] = {"from": a, "to": b, "at": int(time.time()) - 3600, "starts": 1, "failures": 3}
json.dump(s, open(p, "w"))
PY
cp "$NEXT" "$DL/$ARTIFACT"; sign "$B"
start ""
reaches "$A" 60 || fail "the version that could not connect was not put back (the host reports: $(version))"
[ "$("$BIN/ayeusann-agent" --version)" = "ayeusann-agent $A" ] || fail "the binary in place is not $A after the rollback"
for _ in $(seq 1 30); do logged "tried and put back" && break; sleep 1; done
logged "tried and put back" || fail "the version that was put back was offered again and not declined"
stays "$A" 5 || fail "the host left $A again: $(version)"
pass "a version that could not connect is put back to $A, and is left alone when offered again"

# 6. A machine that was never told a release key at enrolment does not take one
# from a later session: the platform cannot name itself the signer afterwards.
kill "$AGENT_PID"; wait "$AGENT_PID" 2>/dev/null || true
python3 - "$W/data/instance-update-$$/agent-state.json" <<'PY'
import json, sys
p = sys.argv[1]
s = json.load(open(p))
s["release_public_key"] = ""      # enrolled when the platform named no key
s["skip_version"] = None; s["skip_until"] = None; s["highest_version"] = None
json.dump(s, open(p, "w"))
PY
: > "$W/agent.log"
start ""
for _ in $(seq 1 60); do logged "has no release key pinned\|enrolled before it named a release key" && break; sleep 1; done
logged "enrolled before it named a release key" || fail "the agent did not say it will not accept this platform's updates"
logged "has no release key pinned" || fail "an update was not refused by a machine with no release key pinned"
stays "$A" 5 || fail "a machine with no pinned key changed version: $(version)"
python3 -c "import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get('release_public_key') == '' else 1)" "$W/data/instance-update-$$/agent-state.json" \
  || fail "a release key was adopted from a reconnect"
pass "a machine enrolled without a release key does not adopt one later, and refuses updates"

echo "All checks passed."
