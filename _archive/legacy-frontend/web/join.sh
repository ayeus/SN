#!/usr/bin/env bash
# ==============================================================================
# ⚡ AyeusANN — One-Liner Network Join Script
# ==============================================================================

set -euo pipefail

API_BASE="${API_BASE:-http://localhost:8080}"
COORD_URL="${COORD_URL:-http://localhost:50051}"

echo -e "\033[1;36m"
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║           ⚡ AyeusANN Quick Node Join               ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo -e "\033[0m"

echo "Connecting to AyeusANN at ${API_BASE}..."

# If run-node.sh exists locally in current dir, just run it!
if [ -f "./run-node.sh" ]; then
    exec ./run-node.sh
fi

# Otherwise, request a quick token and run install.sh
TOKEN_RESP=$(curl -s -m 5 "${API_BASE}/v1/hosts/quick-token" || true)
TOKEN=$(echo "$TOKEN_RESP" | grep -o '"registration_token":"[^"]*' | cut -d'"' -f4 || true)

if [ -n "$TOKEN" ]; then
    echo "Acquired quick token. Starting installer..."
    curl -fsSL "${API_BASE}/install.sh" | sh -s -- --token "$TOKEN" --coordinator "$COORD_URL" --region "${REGION:-IN-SOUTH}"
    if [ -x "$HOME/.ayeusann/bin/ayeusann-agent" ]; then
        echo -e "\n\033[1;32m🚀 Auto-launching AyeusANN Host Agent...\033[0m\n"
        exec "$HOME/.ayeusann/bin/ayeusann-agent" --coordinator-url "$COORD_URL" --token "$TOKEN" --region "${REGION:-IN-SOUTH}"
    fi
else
    echo "Please visit ${API_BASE} to start your node or run ./run-node.sh"
fi
