#!/usr/bin/env bash
# Quick rebuild + reinstall + restart agentd.
# Usage: ./scripts/reinstall.sh
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building..."
AGENTD_VERSION="${AGENTD_VERSION:-$(sed -n 's/^version = "\([^"]*\)"/\1/p' packages/agentd-py/pyproject.toml | head -1)}"
AGENTD_COMMIT="${AGENTD_COMMIT:-$(git rev-parse HEAD)}"
go build -trimpath -ldflags="-X github.com/danieliser/agentruntime/pkg/buildinfo.Version=${AGENTD_VERSION} -X github.com/danieliser/agentruntime/pkg/buildinfo.Commit=${AGENTD_COMMIT}" -o agentd ./cmd/agentd
go build -o agentd-tui ./cmd/agentd-tui
./agentd --require-build "${AGENTD_VERSION}@${AGENTD_COMMIT}"

echo "Installing to ~/.local/bin/"
rm -f ~/.local/bin/agentd ~/.local/bin/agentd-tui
cp agentd agentd-tui ~/.local/bin/
xattr -cr ~/.local/bin/agentd ~/.local/bin/agentd-tui 2>/dev/null || true

echo "Restarting agentd..."
launchctl kickstart -k "gui/$(id -u)/com.agentruntime.agentd" 2>/dev/null || true

sleep 1
if curl -sf http://localhost:8090/health >/dev/null 2>&1; then
    echo "agentd healthy."
else
    echo "Warning: agentd not responding on :8090"
fi
