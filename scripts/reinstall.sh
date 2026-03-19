#!/usr/bin/env bash
# Quick rebuild + reinstall + restart agentd.
# Usage: ./scripts/reinstall.sh
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building..."
go build -o agentd ./cmd/agentd
go build -o agentruntime-sidecar ./cmd/sidecar
go build -o agentd-tui ./cmd/agentd-tui

echo "Installing to ~/.local/bin/"
cp agentd agentruntime-sidecar agentd-tui ~/.local/bin/

echo "Restarting agentd..."
launchctl kickstart -k "gui/$(id -u)/com.agentruntime.agentd" 2>/dev/null || true

sleep 1
if curl -sf http://localhost:8090/health >/dev/null 2>&1; then
    echo "agentd healthy."
else
    echo "Warning: agentd not responding on :8090"
fi
