#!/bin/sh
# Host agent installer for macOS and Linux (PRD F-11: one-command install).
#
#   curl -fsSL <server>/install.sh | sh -s -- --server <server> --token <token> \
#        --coordinator <grpc-url> --region IN-SOUTH
#
# Installs the agent to ~/.ayeusann/bin and sets it up to run in the background
# and start again at every login. The token is single-use; after enrolling, the
# agent reconnects with its stored credential and remembered settings.
#
#   --foreground   run in this terminal instead of in the background
#   --no-start     install only
set -eu

SERVER=""
TOKEN=""
COORDINATOR=""
REGION=""
RUNTIME=""
START=1
FOREGROUND=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --server) SERVER="$2"; shift 2 ;;
        --token) TOKEN="$2"; shift 2 ;;
        --coordinator|--coordinator-url) COORDINATOR="$2"; shift 2 ;;
        --region) REGION="$2"; shift 2 ;;
        --runtime) RUNTIME="$2"; shift 2 ;;
        --no-start) START=0; shift ;;
        --foreground) FOREGROUND=1; shift ;;
        -h|--help)
            sed -n '2,12p' "$0" 2>/dev/null || true
            exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

say()  { printf '  %s\n' "$*"; }
fail() { printf '\n  error: %s\n\n' "$*" >&2; exit 1; }

INSTALL_DIR="$HOME/.ayeusann/bin"
BIN="$INSTALL_DIR/ayeusann-agent"

case "$(uname -s)" in
    Linux)  OS=linux ;;
    Darwin) OS=darwin ;;
    *) fail "unsupported OS $(uname -s). On Windows, run this inside WSL2." ;;
esac
case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) fail "unsupported CPU architecture $(uname -m)" ;;
esac

printf '\nInstalling the host agent (%s/%s)\n\n' "$OS" "$ARCH"
mkdir -p "$INSTALL_DIR"

# 1. The agent binary: a prebuilt download, else a source build.
if [ -n "$SERVER" ] && curl -fsSL -o "$BIN.tmp" "$SERVER/downloads/ayeusann-agent-$OS-$ARCH" 2>/dev/null; then
    mv "$BIN.tmp" "$BIN"
    chmod +x "$BIN"
    say "downloaded agent from $SERVER"
elif [ -f "./agent/Cargo.toml" ] && command -v cargo >/dev/null 2>&1; then
    rm -f "$BIN.tmp"
    say "no prebuilt agent for $OS/$ARCH; building from source (a few minutes the first time)"
    (cd agent && cargo build --release --quiet)
    cp -f agent/target/release/ayeusann-agent "$BIN"
    say "built agent from ./agent"
elif [ -x "$BIN" ]; then
    rm -f "$BIN.tmp"
    say "using the agent already installed at $BIN"
else
    rm -f "$BIN.tmp"
    fail "no prebuilt agent is published for $OS/$ARCH yet. Clone the repository and run this from its root with Rust installed (https://rustup.rs), or ask the operator to publish binaries."
fi

# 2. The model runtime the agent drives.
if [ "${RUNTIME:-${SN_RUNTIME:-ollama}}" = "ollama" ]; then
    if curl -fsS -m 3 "${SN_RUNTIME_URL:-http://127.0.0.1:11434}/api/version" >/dev/null 2>&1; then
        say "Ollama is running"
    else
        say "warning: Ollama is not running. Install it from https://ollama.com/download and start it;"
        say "         the machine connects now but receives jobs only once Ollama is up."
    fi
fi

# 3. NVIDIA driver floor (PRD F-11). The agent enforces this too.
if command -v nvidia-smi >/dev/null 2>&1; then
    DRIVER=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -n1 | cut -d. -f1)
    if [ -n "$DRIVER" ] && [ "$DRIVER" -lt 535 ] 2>/dev/null; then
        fail "NVIDIA driver R$DRIVER is too old; install R535 or newer first."
    fi
fi

say "installed to $BIN"
printf '\n'

# Only what was asked for is passed on: anything left out keeps the value the
# agent remembered from its last enrolment.
set --
[ -n "$TOKEN" ] && set -- "$@" --token "$TOKEN"
[ -n "$COORDINATOR" ] && set -- "$@" --coordinator "$COORDINATOR"
[ -n "$REGION" ] && set -- "$@" --region "$REGION"
[ -n "$RUNTIME" ] && set -- "$@" --runtime "$RUNTIME"

if [ "$START" -eq 0 ]; then
    printf 'Run it in the background with:\n  %s service install %s\n\n' "$BIN" "$*"
    exit 0
fi
[ -n "$TOKEN" ] || [ -f "$HOME/.ayeusann/agent-state.json" ] || fail "--token is required for the first run; create one in the console under Hosts > Add a machine."

if [ "$FOREGROUND" -eq 0 ]; then
    RC=0
    "$BIN" service install "$@" || RC=$?
    case "$RC" in
        0) printf '\n'; exit 0 ;;
        3) printf '\nRunning it in this terminal instead.\n' ;;   # no service manager here
        *) exit "$RC" ;;
    esac
fi

printf 'Starting the agent. Keep this terminal open; press Ctrl+C to stop.\n'
printf 'Start it again later with: %s\n\n' "$BIN"
exec "$BIN" "$@"
