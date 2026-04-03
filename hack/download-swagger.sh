#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOMOD="${REPO_ROOT}/go.mod"
OUTPUT="${REPO_ROOT}/api/swagger.yaml"

# Parse github.com/moby/moby/v2 version from go.mod.
MOBY_VERSION=$(grep 'github.com/moby/moby/v2 ' "${GOMOD}" | awk '{print $3}' | head -1)

if [[ -z "${MOBY_VERSION}" ]]; then
    echo "error: github.com/moby/moby/v2 not found in go.mod" >&2
    exit 1
fi

URL="https://raw.githubusercontent.com/moby/moby/${MOBY_VERSION}/api/swagger.yaml"
echo "Downloading Docker API spec from moby/moby ${MOBY_VERSION} ..."
curl -fsSL "${URL}" -o "${OUTPUT}"

if [[ ! -s "${OUTPUT}" ]]; then
    echo "error: downloaded file is empty" >&2
    exit 1
fi

if ! grep -q '^swagger:' "${OUTPUT}"; then
    echo "error: file does not look like a swagger spec" >&2
    exit 1
fi

API_VERSION=$(yq '.info.version' "${OUTPUT}")
echo "Docker API version: ${API_VERSION}"
