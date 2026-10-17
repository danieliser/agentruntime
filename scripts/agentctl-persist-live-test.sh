#!/usr/bin/env bash
# Process isolation and cleanup live in the companion bootstrap, not prompt env.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
exec python3 "$ROOT/scripts/agentctl_persist_bootstrap.py" "$@"
