#!/usr/bin/env bash
# AyeusANN on one computer, for you and people you know (docs/private-network.md).
#
#   scripts/private.sh init [--address URL] [--port N]   write the secrets file (once)
#   scripts/private.sh up                                build, migrate and start
#   scripts/private.sh down                              stop (data is kept)
#   scripts/private.sh status                            health, address, last backup
#   scripts/private.sh logs [service]                    follow the logs
#   scripts/private.sh backup                            take a database dump now
#   scripts/private.sh restore FILE                      check that a dump restores
#   scripts/private.sh restore FILE --replace            replace the database with it
#   scripts/private.sh address URL                       move to a new address
#
# Everything that must survive lives outside this repository, in
# ~/.ayeusann-platform: the secrets file (private.env) and the backups.
set -euo pipefail
cd "$(dirname "$0")/.."

PLATFORM_DIR="${AYEUSANN_PLATFORM_DIR:-$HOME/.ayeusann-platform}"
ENV_FILE="$PLATFORM_DIR/private.env"
PROJECT="${PRIVATE_PROJECT:-ayeusann-private}"
COMPOSE_FILE="deploy/compose/docker-compose.private.yml"
SERVICES="gateway control-api scheduler coordinator inference-gateway trust-engine"

RESTORE_COPY=""   # a dump copied into the backups folder for one restore

die() { echo "$*" >&2; exit 1; }

# setting KEY: the value of KEY in the secrets file ("" when absent).
setting() { sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null | tail -1; }

# put KEY VALUE: set KEY in the secrets file, keeping everything else.
put() {
  local tmp="$ENV_FILE.tmp.$$"
  ( umask 077
    awk -v key="$1" -v value="$2" '
      index($0, key "=") == 1 { print key "=" value; done = 1; next }
      { print }
      END { if (!done) print key "=" value }' "$ENV_FILE" > "$tmp" )
  mv "$tmp" "$ENV_FILE"
}

need_env() {
  [ -f "$ENV_FILE" ] || die "No installation found at $PLATFORM_DIR. Run: make private-init"
}

compose() {
  need_env
  docker info >/dev/null 2>&1 || die "Docker is not running. Start Docker Desktop (and turn on 'Start Docker Desktop when you sign in' so the platform comes back after a reboot)."
  local backups
  backups="$(setting BACKUP_DIR)"
  backups="${backups:-$PLATFORM_DIR/backups}"
  mkdir -p "$backups" dist/agent
  HOST_UID="$(id -u)" HOST_GID="$(id -g)" PRIVATE_ENV_FILE="$ENV_FILE" BACKUP_DIR="$backups" \
    docker compose --progress "${COMPOSE_PROGRESS:-auto}" -p "$PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" "$@"
}

# This machine's address on the local network.
lan_address() {
  local ip="" dev
  if command -v ipconfig >/dev/null 2>&1; then          # macOS
    for dev in en0 en1 en2 en3 en4 en5; do
      ip=$(ipconfig getifaddr "$dev" 2>/dev/null || true)
      [ -n "$ip" ] && break
    done
  elif command -v ip >/dev/null 2>&1; then               # Linux
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
  fi
  [ -n "$ip" ] || ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
  printf '%s' "$ip"
}

check_url() {
  case "$1" in
    http://*|https://*) ;;
    *) die "An address looks like http://192.168.1.4:8090 (got \"$1\")." ;;
  esac
  case "$1" in
    *localhost*|*127.0.0.1*) die "\"$1\" only works on this computer. Use the address other machines reach it by." ;;
  esac
}

init() {
  local address="" port=8090
  while [ $# -gt 0 ]; do
    case "$1" in
      --address) address="${2:?--address needs a value}"; shift 2 ;;
      --port) port="${2:?--port needs a value}"; shift 2 ;;
      *) die "unknown option: $1" ;;
    esac
  done
  if [ -f "$ENV_FILE" ]; then
    echo "An installation already exists: $ENV_FILE"
    echo "It was left untouched. Its secrets cannot be regenerated without every"
    echo "machine enrolling again, so this command never overwrites them."
    return 0
  fi
  command -v openssl >/dev/null 2>&1 || die "openssl is needed to generate the secrets."

  if [ -z "$address" ]; then
    local ip
    ip="$(lan_address)"
    [ -n "$ip" ] || die "This computer does not seem to be on a network. Connect it, or name the address: scripts/private.sh init --address http://<address>:$port"
    address="http://$ip:$port"
  fi
  address="${address%/}"
  check_url "$address"

  mkdir -p "$PLATFORM_DIR/backups"
  chmod 700 "$PLATFORM_DIR"
  ( umask 077
    cat > "$ENV_FILE" <<EOF
# AyeusANN private installation. Created $(date -u +%Y-%m-%dT%H:%M:%SZ).
#
# KEEP THIS FILE. It holds the keys of this installation. A copy is saved next
# to every database backup. If it is lost, every account has to sign in again
# and every machine has to enrol again.

# The address people and host agents use. Change it with:
#   scripts/private.sh address http://new-address:$port
PUBLIC_URL=$address
PRIVATE_PORT=$port
COORDINATOR_URLS=

# Signs sign-in sessions and enrolment tokens.
JWT_SECRET=$(openssl rand -hex 32)
# Signs calls between the platform's own services.
INTERNAL_SERVICE_SECRET=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -hex 24)
# Signs every job sent to a host. Hosts pin it when they enrol.
MANIFEST_SIGNING_KEY=$(openssl rand -base64 32)
# Entered once, by the first account, which becomes the operator.
OWNER_CODE=$(openssl rand -hex 4)-$(openssl rand -hex 4)

# Accounts that see the operations console (comma-separated emails).
PLATFORM_ADMIN_EMAILS=

# Where database dumps are written. Somewhere a cloud-sync or Time Machine
# covers is a good choice.
BACKUP_DIR=$PLATFORM_DIR/backups
EOF
  )
  echo "Created $ENV_FILE"
  echo
  echo "  Address    $address"
  echo "  Backups    $PLATFORM_DIR/backups"
  echo
  echo "Keep that file safe: it holds this installation's keys. Next: make private-up"
}

up() {
  need_env
  echo "Building and starting (the first build takes a few minutes)..."
  compose up -d --build --wait
  echo
  status
}

status() {
  need_env
  local url port backups last
  url="$(setting PUBLIC_URL)"
  port="$(setting PRIVATE_PORT)"; port="${port:-8090}"
  backups="$(setting BACKUP_DIR)"; backups="${backups:-$PLATFORM_DIR/backups}"
  compose ps --format 'table {{.Service}}\t{{.Status}}' 2>/dev/null || true
  echo
  if curl -sf -m 3 "http://localhost:$port/readyz" >/dev/null 2>&1; then
    echo "  Running    $url   (on this computer: http://localhost:$port)"
  else
    echo "  Not answering on port $port. Start it with: make private-up"
  fi
  if [ -f "$backups/last-error" ]; then
    echo "  Backups    FAILING since $(cat "$backups/last-error"); see: make private-logs"
  elif [ -f "$backups/last-backup" ]; then
    last="$(cat "$backups/last-backup")"
    echo "  Backups    last at $last, in $backups"
  else
    echo "  Backups    none yet (the first is taken a few minutes after the start), in $backups"
  fi
  local published
  published=$(ls dist/agent 2>/dev/null | wc -l | tr -d ' ')
  if [ "$published" = 0 ]; then
    echo "  Agents     none published yet, so install commands cannot download one."
    echo "             make dist-agent (this OS), dist-agent-linux, dist-agent-windows"
  fi
}

# quiet: a Compose command without its own progress lines.
quiet() { COMPOSE_PROGRESS=quiet compose "$@"; }

backup() {
  # Inside the running backup container when there is one, so it runs as part
  # of the stack; otherwise in a one-off container.
  if [ -n "$(compose ps -q backup 2>/dev/null)" ]; then
    quiet exec -T backup backup once
  else
    quiet run --rm --no-deps -T backup backup once
  fi
}

restore() {
  local file="" replace=0 yes=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --replace) replace=1; shift ;;
      --yes) yes=1; shift ;;
      -*) die "unknown option: $1" ;;
      *) file="$1"; shift ;;
    esac
  done
  [ -n "$file" ] || die "usage: scripts/private.sh restore FILE [--replace]"
  [ -f "$file" ] || die "No such file: $file"

  # The backup container sees the backups folder only, so a dump from
  # elsewhere is copied into it for the duration.
  local backups abs inside
  backups="$(setting BACKUP_DIR)"; backups="${backups:-$PLATFORM_DIR/backups}"
  mkdir -p "$backups"
  backups="$(cd "$backups" && pwd -P)"
  abs="$(cd "$(dirname "$file")" && pwd -P)/$(basename "$file")"
  case "$abs" in
    "$backups"/*) inside="/backups/${abs#"$backups"/}" ;;
    *)
      mkdir -p "$backups/incoming"
      cp "$abs" "$backups/incoming/$(basename "$abs")"
      RESTORE_COPY="$backups/incoming/$(basename "$abs")"
      inside="/backups/incoming/$(basename "$abs")"
      ;;
  esac
  trap '[ -z "$RESTORE_COPY" ] || { rm -f "$RESTORE_COPY"; rmdir "$(dirname "$RESTORE_COPY")" 2>/dev/null || true; }' EXIT

  [ -n "$(compose ps -q postgres 2>/dev/null)" ] || die "The database is not running. Start the platform first: make private-up"

  if [ "$replace" = 0 ]; then
    quiet run --rm --no-deps -T backup backup check "$inside"
    echo
    echo "Nothing was changed. To replace the live database with this dump:"
    echo "  scripts/private.sh restore $file --replace"
    return 0
  fi

  if [ "$yes" = 0 ]; then
    echo "This replaces everything in the live database with $(basename "$file")."
    echo "A dump of the current database is taken first."
    printf 'Type REPLACE to continue: '
    read -r answer || answer=""
    [ "$answer" = "REPLACE" ] || die "Nothing was changed."
  fi
  echo "Stopping the services..."
  # shellcheck disable=SC2086
  compose stop $SERVICES backup >/dev/null
  echo "Saving the current database..."
  local restored=0
  quiet run --rm --no-deps -T backup backup once before-restore &&
    quiet run --rm --no-deps -T backup backup replace "$inside" && restored=1
  # Whatever happened, the services come back.
  echo "Starting again (a dump from an older version is brought up to date first)..."
  quiet up -d --wait
  [ "$restored" = 1 ] || die "The dump could not be restored. The database is as it was, and the services are running again."
  echo
  echo "Restored. If this dump came from another computer, its private.env must be"
  echo "in place too ($ENV_FILE), or accounts and machines will not be recognised."
}

address() {
  need_env
  local new="${1:-}" old urls u seen out n
  [ -n "$new" ] || die "usage: scripts/private.sh address http://new-address:port"
  new="${new%/}"
  check_url "$new"
  old="$(setting PUBLIC_URL)"
  if [ "$new" = "$old" ]; then
    echo "The address is already $new."
    return 0
  fi
  # Newest first, then every address used before it (at most four), so agents
  # that are connected learn the new one and those that reconnect find it.
  urls="$new,$old,$(setting COORDINATOR_URLS)"
  seen=","; out=""; n=0
  IFS=',' read -r -a list <<< "$urls"
  for u in "${list[@]}"; do
    u="${u%/}"
    [ -n "$u" ] || continue
    case "$seen" in *",$u,"*) continue ;; esac
    seen="$seen$u,"
    out="${out:+$out,}$u"
    n=$((n + 1))
    [ "$n" -ge 4 ] && break
  done
  put PUBLIC_URL "$new"
  put COORDINATOR_URLS "$out"
  echo "Address changed: $old -> $new"
  if [ -n "$(compose ps -q gateway 2>/dev/null)" ]; then
    compose up -d --wait
  fi
  cat <<EOF

Machines that are connected now are told the new address and will use it by
themselves once the old one stops answering. Keep $old
reachable until 'ayeusann-agent service status' on each machine lists the new
address. A machine that was offline through the change needs it set by hand:
  ayeusann-agent service install --coordinator $new

Endpoints handed to people before the change still name the old address; new
ones use $new.
EOF
}

case "${1:-}" in
  init) shift; init "$@" ;;
  up) up ;;
  down) compose down; echo "Stopped. Data and backups are kept." ;;
  status) status ;;
  logs) shift; compose logs -f --tail=100 "$@" ;;
  backup) backup ;;
  restore) shift; restore "$@" ;;
  address) shift; address "$@" ;;
  compose) shift; compose "$@" ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
