#!/bin/sh
# AyeusANN Host Agent macOS/Linux Installer
set -e

TOKEN=""
COORDINATOR="http://127.0.0.1:50051"

while [ "$#" -gt 0 ]; do
    case "$1" in
        --token)
            TOKEN="$2"
            shift 2
            ;;
        --coordinator|--coordinator-url)
            COORDINATOR="$2"
            shift 2
            ;;
        --region)
            REGION="$2"
            shift 2
            ;;
        *)
            if [ -z "$TOKEN" ]; then
                TOKEN="$1"
            fi
            shift
            ;;
    esac
done

REGION="${REGION:-IN-SOUTH}"

TOKEN="${TOKEN:-$SN_REGISTRATION_TOKEN}"
COORDINATOR="${COORDINATOR:-$SN_COORDINATOR_URL}"

if [ -z "$TOKEN" ]; then
    echo "ERROR: Host registration token is required."
    echo "Usage: curl -fsSL <server>/install.sh | sh -s -- --token <TOKEN> --coordinator <URL>"
    exit 1
fi

echo "===================================================="
echo "⚡ AyeusANN Host Agent Installer (macOS/Linux)"
echo "===================================================="
echo "Coordinator Endpoint: $COORDINATOR"
echo "Registration Token:   ${TOKEN:0:16}..."
echo "Architecture:         $(uname -m) / $(uname -s)"
echo "===================================================="

INSTALL_DIR="$HOME/.ayeusann/bin"
mkdir -p "$INSTALL_DIR"

# Verify connectivity to coordinator
echo "[1/3] Verifying network connectivity to coordinator..."
CLEAN_COORD=$(echo "$COORDINATOR" | sed -E 's|^https?://||' | cut -d/ -f1)
COORD_HOST=$(echo "$CLEAN_COORD" | cut -d: -f1)
COORD_PORT=$(echo "$CLEAN_COORD" | cut -d: -f2)
if [ -z "$COORD_PORT" ] || [ "$COORD_HOST" = "$COORD_PORT" ]; then
    COORD_PORT=50051
fi

if nc -z -w 3 "$COORD_HOST" "$COORD_PORT" >/dev/null 2>&1; then
    echo "  ✓ Coordinator reachable at $COORDINATOR"
elif curl -fsSL --connect-timeout 3 "$COORDINATOR" >/dev/null 2>&1; then
    echo "  ✓ Coordinator reachable at $COORDINATOR"
else
    echo "  ! Warning: Unable to connect to coordinator at $COORDINATOR ($COORD_HOST:$COORD_PORT). Ensure the service is running."
fi

# Locate or build the AyeusANN host agent
echo "[2/3] Checking AyeusANN agent binary..."
AGENT_BIN=""

if command -v ayeusann-agent >/dev/null 2>&1; then
    AGENT_BIN="$(command -v ayeusann-agent)"
elif [ -x "$INSTALL_DIR/ayeusann-agent" ]; then
    AGENT_BIN="$INSTALL_DIR/ayeusann-agent"
elif [ -x "$PWD/agent/target/release/AyeusANN-agent" ]; then
    cp -f "$PWD/agent/target/release/AyeusANN-agent" "$INSTALL_DIR/ayeusann-agent"
    AGENT_BIN="$INSTALL_DIR/ayeusann-agent"
elif [ -x "$HOME/Documents/SN/agent/target/release/AyeusANN-agent" ]; then
    cp -f "$HOME/Documents/SN/agent/target/release/AyeusANN-agent" "$INSTALL_DIR/ayeusann-agent"
    AGENT_BIN="$INSTALL_DIR/ayeusann-agent"
elif [ -f "./agent/Cargo.toml" ] && command -v cargo >/dev/null 2>&1; then
    echo "  Compiling agent binary from local workspace with cargo..."
    (cd agent && cargo build --release)
    cp -f "agent/target/release/AyeusANN-agent" "$INSTALL_DIR/ayeusann-agent" 2>/dev/null || cp -f "agent/target/release/ayeusann-agent" "$INSTALL_DIR/ayeusann-agent"
    AGENT_BIN="$INSTALL_DIR/ayeusann-agent"
fi

echo "[3/3] Installation completed successfully."
echo ""
if [ -n "$AGENT_BIN" ]; then
    echo "To launch your host agent now, run:"
    echo "  $AGENT_BIN --coordinator-url $COORDINATOR --token $TOKEN --region $REGION"
else
    echo "To run the agent using cargo:"
    echo "  cd agent && cargo run --release -- --coordinator-url $COORDINATOR --token $TOKEN --region $REGION"
fi
