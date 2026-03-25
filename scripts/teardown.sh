#!/usr/bin/env bash
# Tears down the dev-env. Removes all k8s resources and optionally uninstalls k3s.
#
# Usage: sudo ./scripts/teardown.sh [--full]
#   --full: Also uninstall k3s and remove all data

set -euo pipefail

FULL_TEARDOWN=false
if [ "${1:-}" = "--full" ]; then
    FULL_TEARDOWN=true
fi

export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

echo "=== Dev-Env Teardown ==="
echo ""

# Check if k3s is running
if ! command -v k3s &>/dev/null || ! kubectl cluster-info &>/dev/null 2>&1; then
    echo "k3s is not running. Nothing to tear down."
    if [ "${FULL_TEARDOWN}" = true ] && [ -f /usr/local/bin/k3s-uninstall.sh ]; then
        echo "Running k3s uninstall..."
        /usr/local/bin/k3s-uninstall.sh
    fi
    exit 0
fi

# Remove Gitea
echo "Removing Gitea..."
helm uninstall gitea -n gitea 2>/dev/null || echo "  Gitea helm release not found."
kubectl delete pvc --all -n gitea 2>/dev/null || true

# Remove Taiga
echo "Removing Taiga..."
kubectl delete -f "$(dirname "$0")/../k8s/taiga/" --ignore-not-found=true 2>/dev/null || true
kubectl delete pvc --all -n taiga 2>/dev/null || true
kubectl delete pv taiga-static-pv taiga-media-pv --ignore-not-found=true 2>/dev/null || true

# Remove namespaces
echo "Removing namespaces..."
kubectl delete namespace gitea taiga agents --ignore-not-found=true 2>/dev/null || true

echo "Kubernetes resources removed."

if [ "${FULL_TEARDOWN}" = true ]; then
    echo ""
    echo "Full teardown: removing k3s and data..."

    # Remove shared volume data
    rm -rf /var/lib/dev-env/taiga

    # Uninstall k3s
    if [ -f /usr/local/bin/k3s-uninstall.sh ]; then
        /usr/local/bin/k3s-uninstall.sh
        echo "k3s uninstalled."
    fi

    echo "Full teardown complete."
else
    echo ""
    echo "k3s is still running. Use --full to also uninstall k3s."
fi
