#!/usr/bin/env bash
# Isolated AgentD + PERSIST CLI integration acceptance. Never targets installed services.
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PERSIST_ROOT=${PERSIST_ROOT:-/Users/danieliser/Toolkit/persistence--agentctl}
PROVIDER=${1:-}
case "$PROVIDER" in claude|codex) ;; *) echo "Usage: $0 claude|codex" >&2; exit 2 ;; esac

for command in go pnpm node python3 docker; do
  command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 2; }
done
[[ -d "$PERSIST_ROOT/src" && -f "$PERSIST_ROOT/package.json" ]] || { echo "PERSIST checkout not found: $PERSIST_ROOT" >&2; exit 2; }

RUN_ROOT="$ROOT/.agentctl-live-test/persist-$PROVIDER"
mkdir -p "$RUN_ROOT"
chmod 700 "$RUN_ROOT"
[[ "$(git -C "$ROOT" check-ignore -q "$RUN_ROOT"; echo $?)" == 0 ]] || { echo 'Run directory must be gitignored' >&2; exit 2; }
RUN_ROOT=$(mktemp -d "$RUN_ROOT/run.XXXXXX")
chmod 700 "$RUN_ROOT"
AGENTD_PORT=${AGENTCTL_PERSIST_AGENTD_PORT:-$([[ "$PROVIDER" == claude ]] && echo 18091 || echo 18092)}
PERSIST_PORT=${AGENTCTL_PERSIST_PORT:-$([[ "$PROVIDER" == claude ]] && echo 18803 || echo 18804)}
AGENTD_DATA="$RUN_ROOT/agentd-data"
PERSIST_DATA_DIR="$RUN_ROOT/persist-data"
PERSIST_CONFIG_DIR="$RUN_ROOT/persist-config"
mkdir -m 700 "$AGENTD_DATA" "$PERSIST_DATA_DIR" "$PERSIST_CONFIG_DIR" "$RUN_ROOT/bin"

AGENTD_PID=''
PERSIST_PID=''
cleanup() {
  local status=$?
  if [[ -n "$PERSIST_PID" ]] && kill -0 "$PERSIST_PID" 2>/dev/null; then kill "$PERSIST_PID" 2>/dev/null || true; wait "$PERSIST_PID" 2>/dev/null || true; fi
  if [[ -n "$AGENTD_PID" ]] && kill -0 "$AGENTD_PID" 2>/dev/null; then kill "$AGENTD_PID" 2>/dev/null || true; wait "$AGENTD_PID" 2>/dev/null || true; fi
  # Containers are tagged by this run's Docker proxy and removed by the same
  # scoped docker command used by AgentD. No global prune or broad label scan.
  if [[ -x "$RUN_ROOT/bin/docker" ]]; then
    AGENTCTL_LIVE_REAL_DOCKER=$(command -v docker || true) AGENTCTL_LIVE_SCOPE=$(basename "$RUN_ROOT") \
      "$RUN_ROOT/bin/docker" ps -aq --filter "label=agentctl.live_test=$(basename "$RUN_ROOT")" | xargs -r docker rm -f >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

export AGENTD_URL="http://127.0.0.1:$AGENTD_PORT"
export AGENTD_TOKEN_FILE="$AGENTD_DATA/auth.token"
export PERSIST_PORT PERSIST_DATA_DIR PERSIST_CONFIG_DIR
export PERSIST_URL="http://127.0.0.1:$PERSIST_PORT"
export AGENTCTL_CURSOR_DIR="$RUN_ROOT/cursors"
mkdir -m 700 "$AGENTCTL_CURSOR_DIR"

# PERSIST reads this private override ahead of its checkout defaults. It gets
# only the isolated AgentD URL and token path; the generated token is never shown.
cat > "$PERSIST_CONFIG_DIR/config.yaml" <<EOF
agentruntime:
  url: $AGENTD_URL
  token_file: $AGENTD_TOKEN_FILE
  resume_idempotency_placement: header
  resume_idempotency_header: idempotency-key
  resume_idempotency_field: idempotency_key
model_routing:
  worker_runtime: agentruntime
EOF
chmod 600 "$PERSIST_CONFIG_DIR/config.yaml"

go build -buildvcs=false -o "$RUN_ROOT/bin/agentd" ./cmd/agentd
go build -buildvcs=false -o "$RUN_ROOT/bin/agentctl" ./cmd/agentctl
cp "$ROOT/scripts/agentctl_docker_scope.py" "$RUN_ROOT/bin/docker"
chmod 700 "$RUN_ROOT/bin/docker"
AGENTCTL_LIVE_REAL_DOCKER=$(command -v docker)
AGENTCTL_LIVE_SCOPE=$(basename "$RUN_ROOT")
export AGENTCTL_LIVE_REAL_DOCKER AGENTCTL_LIVE_SCOPE
PATH="$RUN_ROOT/bin:$PATH" "$RUN_ROOT/bin/agentd" --host 127.0.0.1 --port "$AGENTD_PORT" --runtime docker --credential-sync --data-dir "$AGENTD_DATA" >"$RUN_ROOT/agentd.log" 2>&1 &
AGENTD_PID=$!

PERSIST_FOREGROUND=1 pnpm --dir "$PERSIST_ROOT" exec tsx "$PERSIST_ROOT/src/main-daemon.ts" >"$RUN_ROOT/persist.log" 2>&1 &
PERSIST_PID=$!

wait_http() {
  local url=$1
  for _ in {1..120}; do
    if python3 -c 'import sys,urllib.request; urllib.request.urlopen(sys.argv[1], timeout=1)' "$url" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "Timed out waiting for isolated service: $url" >&2
  return 1
}
wait_http "$AGENTD_URL/health"
wait_http "$PERSIST_URL/api/v1/health"

export PERSIST_URL
# Scenarios are delegated to the companion driver so the shell owns process
# isolation and teardown while all workflow actions remain CLI operations.
python3 "$ROOT/scripts/agentctl_persist_live_test.py" \
  --provider "$PROVIDER" --root "$RUN_ROOT" --agentd-port "$AGENTD_PORT" \
  --persist-port "$PERSIST_PORT" --persist-root "$PERSIST_ROOT" \
  --persist-cli "$(command -v pnpm)" --agentctl "$RUN_ROOT/bin/agentctl"
