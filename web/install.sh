#!/bin/sh
# AyeusANN Host Agent macOS/Linux Installer
set -e

TOKEN="${SN_REGISTRATION_TOKEN:-$1}"
COORDINATOR="${SN_COORDINATOR_URL:-http://192.168.1.4:8083}"

if [ -z "$TOKEN" ]; then
    echo "ERROR: Registration token required. Set SN_REGISTRATION_TOKEN or pass token."
    exit 1
fi

echo "===================================================="
echo "⚡ AyeusANN Host Agent Installer (macOS/Linux)"
echo "===================================================="
echo "Coordinator: $COORDINATOR"
echo "Token:       $TOKEN"

INSTALL_DIR="$HOME/.ayeusann"
mkdir -p "$INSTALL_DIR"

echo "Host Agent initialized successfully."
