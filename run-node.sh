#!/usr/bin/env sh
# Runs the host agent from this repository (development and self-builders).
#
#   ./run-node.sh --token <token> [--coordinator http://localhost:50051] [--region IN-SOUTH]
#   ./run-node.sh                       # reconnect with the stored credential
#   ./run-node.sh --fake-gpu --instance b --token <token>   # a second local dev host
#
# All arguments go straight to the agent; see `ayeusann-agent --help`.
set -eu
cd "$(dirname "$0")"
command -v cargo >/dev/null 2>&1 || { echo "Rust is required: https://rustup.rs" >&2; exit 1; }
cargo build --release --quiet --manifest-path agent/Cargo.toml
exec agent/target/release/ayeusann-agent "$@"
