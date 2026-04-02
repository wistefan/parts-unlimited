#!/usr/bin/env bash
# import-images.sh — Build (if sources changed) and reliably import local
# images into k3s containerd.  Can be run standalone after code changes or
# called from setup.sh.
#
# Usage:
#   scripts/import-images.sh              # build + import both images
#   scripts/import-images.sh orchestrator  # only orchestrator
#   scripts/import-images.sh agent-worker  # only agent-worker
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# Images and their build contexts
declare -A IMAGE_CONTEXTS=(
    ["orchestrator"]="${PROJECT_DIR}/orchestrator"
    ["agent-worker"]="${PROJECT_DIR}/agent"
)

# Which images to process (default: all)
TARGETS=("${@:-orchestrator agent-worker}")

# import_image builds a Docker image, exports it into k3s containerd, and
# ensures both the short tag (name:latest) and the fully-qualified tag
# (docker.io/library/name:latest) exist.  This prevents ErrImagePull
# caused by containerd failing to resolve the short name.
# Use sudo for k3s commands when not running as root.
K3S_CTR="k3s ctr"
if [ "$(id -u)" -ne 0 ]; then
    K3S_CTR="sudo k3s ctr"
fi

import_image() {
    local name="$1"
    local context="${IMAGE_CONTEXTS[$name]}"
    local short_tag="${name}:latest"
    local fq_tag="docker.io/library/${short_tag}"

    echo "--- ${name} ---"

    # Build
    echo "  Building ${short_tag}..."
    docker build -t "${short_tag}" "${context}" --quiet

    # Export from Docker and import into k3s containerd
    echo "  Importing into k3s containerd..."
    docker save "${short_tag}" | ${K3S_CTR} images import -

    # Ensure both tag forms exist so k3s can resolve either one.
    # containerd may store the import under the short or fq name depending
    # on the Docker save format; add the missing one.
    if ${K3S_CTR} images ls -q | grep -qF "${fq_tag}"; then
        # fq exists — add short alias if missing
        if ! ${K3S_CTR} images ls -q | grep -qxF "${short_tag}"; then
            ${K3S_CTR} images tag "${fq_tag}" "${short_tag}" 2>/dev/null || true
        fi
    elif ${K3S_CTR} images ls -q | grep -qF "${short_tag}"; then
        # short exists — add fq alias if missing
        if ! ${K3S_CTR} images ls -q | grep -qxF "${fq_tag}"; then
            ${K3S_CTR} images tag "${short_tag}" "${fq_tag}" 2>/dev/null || true
        fi
    fi

    # Verify
    if ${K3S_CTR} images ls -q | grep -qF "${name}"; then
        echo "  OK: ${name} available in k3s."
    else
        echo "  ERROR: ${name} not found in k3s after import!"
        return 1
    fi
}

for target in ${TARGETS[@]}; do
    if [[ -z "${IMAGE_CONTEXTS[$target]+x}" ]]; then
        echo "Unknown image: ${target}" >&2
        echo "Available: ${!IMAGE_CONTEXTS[*]}" >&2
        exit 1
    fi
    import_image "${target}"
done

echo "All images imported successfully."
