#!/usr/bin/env bash
# Local development environment.
#
#   scripts/dev.sh up       infra + database + all services + web console
#   scripts/dev.sh db       create, migrate and seed ayeusann_dev and ayeusann_test
#   scripts/dev.sh services (re)build and (re)start the Go services only
#   scripts/dev.sh restart NAME   restart one service without rebuilding it
#   scripts/dev.sh pid NAME       print one service's process id
#   scripts/dev.sh web      start the web console dev server
#   scripts/dev.sh down     stop services and the console (infra keeps running)
#   scripts/dev.sh status   health of every service
#
# Open http://localhost:8080 (gateway) once it is up. To add your own machine as
# a host, sign in, open Hosts > Add a machine, and run the "From this repo"
# command in another terminal. Other machines on the same network can open the
# console and connect their GPUs through this machine's network address, which
# `up` and `status` print (see docs/connect-a-gpu.md).
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT="$(pwd)"
LOGS="$ROOT/logs"
PIDS="$ROOT/.dev-pids"          # Go services
WEB_PID="$ROOT/.dev-web-pid"    # web console
COMPOSE="deploy/compose/docker-compose.yml"
PG_URL_BASE="postgres://ayeusann:ayeusann_dev@localhost:5433"
DEV_DB="ayeusann_dev"
TEST_DB="ayeusann_test"
SERVICES=(control-api scheduler coordinator inference-gateway trust-engine gateway)

# Shared development configuration. Every service reads the same values, which
# is what makes signed service-to-service calls work.
export SN_ENV=dev
export DATABASE_URL="$PG_URL_BASE/$DEV_DB?sslmode=disable"
export REDIS_URL="${REDIS_URL:-redis://localhost:6379}"
# PUBLIC_URL is deliberately not set: in development the control API hands out
# the address each request arrived on, so install commands work on other
# machines. Set it in .env to pin an address. Agents use the same address
# (the gateway forwards their sessions) unless COORDINATOR_PUBLIC_URL names
# another.
export WEB_URL="${WEB_URL:-http://localhost:3000}"
export CORS_ALLOWED_ORIGINS="${CORS_ALLOWED_ORIGINS:-http://localhost:3000}"
export HOST_PROBATION_DAYS="${HOST_PROBATION_DAYS:-0}"     # dev hosts take work immediately
export REPUTATION_INTERVAL_SEC="${REPUTATION_INTERVAL_SEC:-300}"
export PLATFORM_ADMIN_EMAILS="${PLATFORM_ADMIN_EMAILS:-}"
export INSTALL_DIR="$ROOT/web/install"
export DOWNLOADS_DIR="$ROOT/dist/agent"
[ -f .env ] && set -a && . ./.env && set +a

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1 ($2)" >&2; exit 1; }; }

infra() {
  need docker "https://docs.docker.com/get-docker/"
  docker compose -f "$COMPOSE" up -d postgres redis
  printf "waiting for Postgres"
  until docker exec ann-postgres pg_isready -U ayeusann >/dev/null 2>&1; do printf "."; sleep 1; done
  echo " ready"
}

db() {
  need migrate "brew install golang-migrate"
  infra
  for name in "$DEV_DB" "$TEST_DB"; do
    exists=$(docker exec ann-postgres psql -U ayeusann -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = '$name'")
    if [ "$exists" != "1" ]; then
      echo "creating database $name"
      docker exec ann-postgres psql -U ayeusann -d postgres -c "CREATE DATABASE $name OWNER ayeusann" >/dev/null
    fi
    migrate -path schema/migrations -database "$PG_URL_BASE/$name?sslmode=disable" up
    for f in schema/seeds/*.sql; do
      docker exec -i ann-postgres psql -v ON_ERROR_STOP=1 -q -U ayeusann -d "$name" < "$f"
    done
    echo "database $name migrated and seeded"
  done
}

# The pid file holds one "pid name" line per service.
stop_pids() {
  [ -f "$PIDS" ] || return 0
  while read -r pid _; do kill "$pid" 2>/dev/null || true; done < "$PIDS"
  rm -f "$PIDS"
}

pid_of() {
  [ -f "$PIDS" ] || return 0
  awk -v s="$1" '$2 == s { print $1 }' "$PIDS" | tail -1
}

# restart NAME: stop one service and start the same binary again. This is what
# a crash or an upgrade of that service looks like to everything else.
restart() {
  local s="${1:-}" known=0 x pid
  for x in "${SERVICES[@]}"; do [ "$x" = "$s" ] && known=1; done
  [ "$known" = 1 ] || { echo "unknown service: $s (one of: ${SERVICES[*]})" >&2; exit 2; }
  [ -x "bin/$s" ] || { echo "bin/$s is not built; run scripts/dev.sh services" >&2; exit 1; }
  pid="$(pid_of "$s")"
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 200); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    # Two of the same service would both answer; make sure the old one is gone.
    kill -9 "$pid" 2>/dev/null || true
    grep -v " $s\$" "$PIDS" > "$PIDS.tmp" || true
    mv "$PIDS.tmp" "$PIDS"
  fi
  "./bin/$s" >> "$LOGS/$s.log" 2>&1 &
  echo "$! $s" >> "$PIDS"
  echo "restarted $s"
}

services() {
  need go "https://go.dev/dl/"
  mkdir -p bin "$LOGS"
  stop_pids
  for s in "${SERVICES[@]}"; do
    echo "building $s"
    CGO_ENABLED=0 go build -o "bin/$s" "./services/$s"
  done
  for s in "${SERVICES[@]}"; do
    "./bin/$s" > "$LOGS/$s.log" 2>&1 &
    echo "$! $s" >> "$PIDS"
  done
  sleep 2
  status
}

stop_web() {
  [ -f "$WEB_PID" ] && kill "$(cat "$WEB_PID")" 2>/dev/null || true
  rm -f "$WEB_PID"
  # next spawns a child server; make sure nothing is left on the port.
  lsof -ti tcp:3000 -sTCP:LISTEN 2>/dev/null | xargs kill 2>/dev/null || true
}

web() {
  need npm "https://nodejs.org"
  mkdir -p "$LOGS"
  stop_web
  [ -d web/console/node_modules ] || (cd web/console && npm install --no-audit --no-fund)
  # Detached, with its own log, so the script returns and pipes are not held.
  ( cd web/console && API_ORIGIN="http://localhost:8080" NEXT_TELEMETRY_DISABLED=1 \
      nohup npx next dev --port 3000 > "$LOGS/web.log" 2>&1 < /dev/null & echo $! > "$WEB_PID" )
  echo "web console starting on http://localhost:3000 (logs: logs/web.log)"
}

status() {
  local ports=(8081 8082 8083 8085 8087 8080)
  for i in "${!SERVICES[@]}"; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${ports[$i]}/readyz" || true)
    if [ "$code" = "200" ]; then mark="ok"; else mark="DOWN ($code) — see logs/${SERVICES[$i]}.log"; fi
    printf "  %-18s :%s  %s\n" "${SERVICES[$i]}" "${ports[$i]}" "$mark"
  done
  # The addresses install commands carry. They differ from localhost when this
  # machine is on a network, which is what lets other machines join.
  local addr server coord
  addr=$(curl -s -m 2 "http://localhost:8080/v1/network/address" || true)
  server=$(printf '%s' "$addr" | sed -n 's/.*"server_url":"\([^"]*\)".*/\1/p')
  coord=$(printf '%s' "$addr" | sed -n 's/.*"coordinator_url":"\([^"]*\)".*/\1/p')
  case "$server" in
    ""|*localhost*|*127.0.0.1*) ;;
    *)
      echo
      echo "  From other machines on this network: $server"
      if [ "$coord" = "$server" ] || [ -z "$coord" ]; then
        echo "  (one address for the console, the API and agents; allow incoming connections if your firewall asks)"
      else
        echo "  (agents connect to ${coord#http://}; allow incoming connections if your firewall asks)"
      fi
      ;;
  esac
}

case "${1:-up}" in
  up)
    db
    services
    web
    echo
    echo "Open http://localhost:8080 — sign up, then deploy a model or add this machine as a host."
    ;;
  db) db ;;
  services) services ;;
  restart) restart "${2:-}" ;;
  pid) pid_of "${2:-}" ;;
  web) web ;;
  down) stop_pids; stop_web; echo "stopped" ;;
  status) status ;;
  *) sed -n '2,14p' "$0"; exit 2 ;;
esac
