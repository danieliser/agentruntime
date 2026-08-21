#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TARGET="${1:-all}"
AGENTD_VERSION="${AGENTD_VERSION:-$(sed -n 's/^version = "\([^"]*\)"/\1/p' "${REPO_ROOT}/packages/agentd-py/pyproject.toml" | head -1)}"
AGENTD_COMMIT="${AGENTD_COMMIT:-$(git -C "${REPO_ROOT}" rev-parse HEAD)}"

if [[ ! "${AGENTD_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "invalid AGENTD_VERSION: ${AGENTD_VERSION}" >&2
  exit 1
fi
if [[ ! "${AGENTD_COMMIT}" =~ ^[a-f0-9]{40,64}$ ]]; then
  echo "invalid AGENTD_COMMIT: ${AGENTD_COMMIT}" >&2
  exit 1
fi

build_agent() {
  local provider="$1"
  local image="agentruntime-agent"
  if [ "${provider}" != "all" ]; then
    image="agentruntime-agent-${provider}"
  fi
  echo "Building ${image}:${AGENTD_VERSION} (${provider}) at ${AGENTD_COMMIT} ..."
  docker build \
    --build-arg HOST_UID="$(id -u)" \
    --build-arg HOST_GID="$(id -g)" \
    --build-arg AGENTD_VERSION="${AGENTD_VERSION}" \
    --build-arg AGENTD_COMMIT="${AGENTD_COMMIT}" \
    --build-arg AGENTD_PROVIDER="${provider}" \
    -t "${image}:${AGENTD_VERSION}" \
    -t "${image}:latest" \
    -f "${REPO_ROOT}/docker/Dockerfile.agent" \
    "${REPO_ROOT}"
}

build_proxy() {
  echo "Building agentruntime-proxy:${AGENTD_VERSION} at ${AGENTD_COMMIT} ..."
  docker build \
    --build-arg AGENTD_VERSION="${AGENTD_VERSION}" \
    --build-arg AGENTD_COMMIT="${AGENTD_COMMIT}" \
    -t "agentruntime-proxy:${AGENTD_VERSION}" \
    -t agentruntime-proxy:latest \
    -f "${REPO_ROOT}/docker/Dockerfile.proxy" \
    "${REPO_ROOT}/docker"
}

case "${TARGET}" in
  agent)
    build_agent all
    ;;
  codex)
    build_agent codex
    ;;
  claude)
    build_agent claude
    ;;
  proxy)
    build_proxy
    ;;
  all)
    build_agent all
    build_agent codex
    build_agent claude
    build_proxy
    ;;
  *)
    echo "Unknown target: ${TARGET}. Use: agent | codex | claude | proxy | all" >&2
    exit 1
    ;;
esac
