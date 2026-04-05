#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EMBED_DIR="${REPO_ROOT}/embed"

# Read CRUN_VERSION from mise.toml [env] section.
CRUN_VERSION=$(grep '^CRUN_VERSION' "${REPO_ROOT}/mise.toml" | sed 's/.*= *"\(.*\)"/\1/' | head -1)

if [[ -z "${CRUN_VERSION}" ]]; then
    echo "error: CRUN_VERSION not found in mise.toml [env]" >&2
    exit 1
fi

ARCHES=("amd64" "arm64")
BASE_URL="https://github.com/containers/crun/releases/download/${CRUN_VERSION}"

mkdir -p "${EMBED_DIR}"

for arch in "${ARCHES[@]}"; do
    OUTPUT="${EMBED_DIR}/crun_linux_${arch}"
    ASSET="crun-${CRUN_VERSION}-linux-${arch}"
    URL="${BASE_URL}/${ASSET}"

    if [[ -f "${OUTPUT}" ]]; then
        echo "crun ${CRUN_VERSION} linux/${arch} already present, skipping."
        continue
    fi

    echo "Downloading crun ${CRUN_VERSION} linux/${arch} ..."
    curl -fsSL "${URL}" -o "${OUTPUT}"

    if [[ ! -s "${OUTPUT}" ]]; then
        echo "error: downloaded file is empty: ${OUTPUT}" >&2
        exit 1
    fi

    # Validate ELF magic bytes.
    if ! head -c 4 "${OUTPUT}" | grep -q $'\x7fELF'; then
        echo "error: ${OUTPUT} is not an ELF binary" >&2
        rm -f "${OUTPUT}"
        exit 1
    fi

    chmod +x "${OUTPUT}"
    echo "Saved ${OUTPUT}"
done

echo "crun ${CRUN_VERSION} binaries ready in embed/"
