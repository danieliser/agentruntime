#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-}"
COMMIT="${2:-}"

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || [[ ! "$COMMIT" =~ ^[a-f0-9]{40,64}$ ]]; then
  echo "usage: verify-release.sh VERSION FULL_COMMIT" >&2
  exit 2
fi
if [ "$(git -C "$REPO_ROOT" rev-parse HEAD)" != "$COMMIT" ]; then
  echo "release commit is not the current HEAD" >&2
  exit 1
fi

WRAPPER_VERSION="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_ROOT/packages/agentd-py/pyproject.toml" | head -1)"
SOURCE_VERSION="$(sed -n 's/^const ReleaseVersion = "\([^"]*\)"/\1/p' "$REPO_ROOT/pkg/buildinfo/buildinfo.go" | head -1)"
if [ "$WRAPPER_VERSION" != "$VERSION" ] || [ "$SOURCE_VERSION" != "$VERSION" ]; then
  echo "release metadata mismatch: wrapper=$WRAPPER_VERSION source=$SOURCE_VERSION expected=$VERSION" >&2
  exit 1
fi

VERIFY_DIR="$(mktemp -d)"
trap 'rm -rf "$VERIFY_DIR"' EXIT
go build -trimpath \
  -ldflags="-X github.com/danieliser/agentruntime/pkg/buildinfo.Version=${VERSION} -X github.com/danieliser/agentruntime/pkg/buildinfo.Commit=${COMMIT}" \
  -o "$VERIFY_DIR/agentd" "$REPO_ROOT/cmd/agentd"
test "$("$VERIFY_DIR/agentd" --version)" = "$VERSION"
"$VERIFY_DIR/agentd" --require-build "${VERSION}@${COMMIT}"

if [ "${REQUIRE_RELEASE_TAG:-0}" = "1" ] && ! git -C "$REPO_ROOT" tag --points-at "$COMMIT" | grep -qx "v${VERSION}"; then
  echo "v${VERSION} does not point at $COMMIT" >&2
  exit 1
fi

if [ "${VERIFY_DOCKER_IMAGES:-0}" = "1" ]; then
  for image in "agentruntime-agent:${VERSION}" "agentruntime-proxy:${VERSION}"; do
    test "$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image")" = "$VERSION"
    test "$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image")" = "$COMMIT"
  done
fi

echo "verified AgentD v${VERSION}@${COMMIT}"
