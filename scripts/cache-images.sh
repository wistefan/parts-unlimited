#!/usr/bin/env bash
# Pre-pulls external container images to the host Docker daemon and imports
# them into k3s containerd.  On subsequent runs, images that already exist in
# k3s are skipped; images that exist in Docker but not k3s are imported
# directly without hitting Docker Hub.
#
# This avoids Docker Hub pull-rate limits during repeated setup/teardown
# cycles by treating the host Docker daemon as a persistent image cache.
#
# Usage:  sudo ./scripts/cache-images.sh [image...]
#   With no arguments, caches the default set of images used by dev-env
#   manifests.  Pass explicit image refs to cache only those.
#
# Requires: docker (host daemon), k3s (for ctr)

set -euo pipefail

# ---- Default images used by dev-env k8s manifests ----
# Keep in sync with the image: fields in k8s/**/*.yaml.
DEFAULT_IMAGES=(
    "taigaio/taiga-back:latest"
    "taigaio/taiga-events:latest"
    "taigaio/taiga-front:latest"
    "taigaio/taiga-protected:latest"
    "rabbitmq:3.8-management-alpine"
    "postgres:12.3"
    "nginx:1.19-alpine"
    "gitea/act_runner:latest"
    "docker:dind"
)

# Use arguments if provided, otherwise the default list.
if [ $# -gt 0 ]; then
    IMAGES=("$@")
else
    IMAGES=("${DEFAULT_IMAGES[@]}")
fi

# ---- Pre-flight checks ----

if ! command -v docker &>/dev/null; then
    echo "WARNING: docker not found on the host. Image caching requires docker."
    echo "  k3s will pull images directly from registries (rate limits may apply)."
    exit 0
fi

if ! command -v k3s &>/dev/null; then
    echo "WARNING: k3s not found. Skipping image cache (nothing to import into)."
    exit 0
fi

# ---- Helpers ----

# Normalise a short image reference to the fully-qualified form that
# containerd uses (docker.io/library/ for official images, docker.io/ for
# namespaced images).
normalise_ref() {
    local ref="$1"
    case "${ref}" in
        */*)
            # Already has a namespace (e.g. gitea/act_runner:latest)
            if [[ "${ref}" != *"."* ]]; then
                # No registry prefix → docker.io
                echo "docker.io/${ref}"
            else
                echo "${ref}"
            fi
            ;;
        *)
            # Official image (e.g. postgres:12.3 → docker.io/library/postgres:12.3)
            echo "docker.io/library/${ref}"
            ;;
    esac
}

# Check whether an image already exists in k3s containerd.
image_in_k3s() {
    local fq_ref
    fq_ref=$(normalise_ref "$1")
    k3s ctr images ls -q 2>/dev/null | grep -qF "${fq_ref}"
}

# Check whether an image exists in the host Docker daemon.
image_in_docker() {
    docker image inspect "$1" &>/dev/null
}

# ---- Main loop ----

CACHED=0
PULLED=0
SKIPPED=0
FAILED=0

for img in "${IMAGES[@]}"; do
    # 1) Already in k3s → skip entirely
    if image_in_k3s "${img}"; then
        echo "  [cached]  ${img}  (already in k3s)"
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    # 2) In host Docker but not k3s → import
    if image_in_docker "${img}"; then
        echo "  [import]  ${img}  (from host Docker → k3s)"
        if docker save "${img}" | k3s ctr images import - >/dev/null 2>&1; then
            CACHED=$((CACHED + 1))
        else
            echo "  [FAIL]    ${img}  import failed"
            FAILED=$((FAILED + 1))
        fi
        continue
    fi

    # 3) Not anywhere locally → pull to Docker, then import to k3s
    echo "  [pull]    ${img}  (Docker Hub → host Docker → k3s)"
    if docker pull "${img}" >/dev/null 2>&1; then
        PULLED=$((PULLED + 1))
        if docker save "${img}" | k3s ctr images import - >/dev/null 2>&1; then
            CACHED=$((CACHED + 1))
        else
            echo "  [FAIL]    ${img}  import failed after pull"
            FAILED=$((FAILED + 1))
        fi
    else
        echo "  [FAIL]    ${img}  pull failed"
        FAILED=$((FAILED + 1))
    fi
done

echo ""
echo "Image cache: ${SKIPPED} already cached, ${PULLED} pulled, ${CACHED} imported, ${FAILED} failed."
if [ "${FAILED}" -gt 0 ]; then
    echo "  Some images could not be cached. k3s will attempt to pull them directly."
fi
