#!/usr/bin/env bash
# Isolated live acceptance run. Provider sessions are driven only by agentctl.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
exec python3 "$ROOT/scripts/agentctl_live_test.py" "$@"
