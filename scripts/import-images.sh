#!/usr/bin/env bash
# import-images.sh — Build and push local images to the in-cluster registry.
#
# Usage:
#   scripts/import-images.sh              # build + push both images
#   scripts/import-images.sh orchestrator  # only orchestrator
#   scripts/import-images.sh agent-worker  # only agent-worker
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# Registry endpoint (must match k8s/registry/registry.yaml hostPort)
REGISTRY="${REGISTRY:-localhost:5000}"

# Images and their build contexts
declare -A IMAGE_CONTEXTS=(
    ["orchestrator"]="${PROJECT_DIR}/orchestrator"
    ["agent-worker"]="${PROJECT_DIR}/agent"
)

# Which images to process (default: all)
TARGETS=("${@:-orchestrator agent-worker}")

# import_image builds a Docker image, tags it for the local registry, and
# pushes it.  k3s pulls from the same registry via its registries.yaml
# mirror config, so no manual "docker save | k3s ctr import" is needed.
import_image() {
    local name="$1"
    local context="${IMAGE_CONTEXTS[$name]}"
    local registry_tag="${REGISTRY}/${name}:latest"

    echo "--- ${name} ---"

    # Build
    echo "  Building ${registry_tag}..."
    docker build -t "${registry_tag}" "${context}" --quiet

    # Push to in-cluster registry
    echo "  Pushing to ${REGISTRY}..."
    docker push "${registry_tag}"

    echo "  OK: ${name} pushed to registry."
}

for target in ${TARGETS[@]}; do
    if [[ -z "${IMAGE_CONTEXTS[$target]+x}" ]]; then
        echo "Unknown image: ${target}" >&2
        echo "Available: ${!IMAGE_CONTEXTS[*]}" >&2
        exit 1
    fi
    import_image "${target}"
done

echo "All images pushed to registry at ${REGISTRY}."
