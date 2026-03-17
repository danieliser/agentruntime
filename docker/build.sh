#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TARGET="${1:-all}"

build_agent() {
  echo "Building agentruntime-agent:latest ..."
  docker build \
    --build-arg HOST_UID="$(id -u)" \
    --build-arg HOST_GID="$(id -g)" \
    -t agentruntime-agent:latest \
    -f "${REPO_ROOT}/docker/Dockerfile.agent" \
    "${REPO_ROOT}"
}

build_proxy() {
  echo "Building agentruntime-proxy:latest ..."
  docker build \
    -t agentruntime-proxy:latest \
    -f "${REPO_ROOT}/docker/Dockerfile.proxy" \
    "${REPO_ROOT}/docker"
}

case "${TARGET}" in
  agent)
    build_agent
    ;;
  proxy)
    build_proxy
    ;;
  all)
    build_agent
    build_proxy
    ;;
  *)
    echo "Unknown target: ${TARGET}. Use: agent | proxy | all" >&2
    exit 1
    ;;
esac
