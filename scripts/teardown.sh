#!/usr/bin/env bash
# Tears down the dev-env. Removes all k8s resources and optionally uninstalls k3s.
#
# Usage: sudo ./scripts/teardown.sh [--full]
#   Without --full: removes k8s resources but keeps persistent data on the host.
#     Running setup.sh again will restore the environment with existing data.
#   --full: Also uninstall k3s and remove all persistent data from the host.

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
        rm -rf /var/lib/dev-env
        echo "Persistent data removed."
    fi
    exit 0
fi

# Remove Gitea
echo "Removing Gitea..."
helm uninstall gitea -n gitea 2>/dev/null || echo "  Gitea helm release not found."
kubectl delete pvc --all -n gitea 2>/dev/null || true
kubectl delete pv gitea-data-pv gitea-postgresql-pv --ignore-not-found=true 2>/dev/null || true

# Remove Taiga
echo "Removing Taiga..."
kubectl delete -f "$(dirname "$0")/../k8s/taiga/" --ignore-not-found=true 2>/dev/null || true
kubectl delete pvc --all -n taiga 2>/dev/null || true
kubectl delete pv taiga-static-pv taiga-media-pv taiga-db-pv taiga-rabbitmq-pv --ignore-not-found=true 2>/dev/null || true

# Remove agent resources
echo "Removing agent resources..."
kubectl delete -f "$(dirname "$0")/../k8s/agents/" --ignore-not-found=true 2>/dev/null || true

# Remove monitoring
echo "Removing monitoring..."
kubectl delete -f "$(dirname "$0")/../k8s/monitoring/" --ignore-not-found=true 2>/dev/null || true

# Remove local registry
echo "Removing local registry..."
kubectl delete -f "$(dirname "$0")/../k8s/registry/" --ignore-not-found=true 2>/dev/null || true

# Remove namespaces
echo "Removing namespaces..."
kubectl delete namespace gitea taiga agents monitoring registry --ignore-not-found=true 2>/dev/null || true

echo "Kubernetes resources removed."

if [ "${FULL_TEARDOWN}" = true ]; then
    echo ""
    echo "Full teardown: removing k3s and persistent data..."

    # Remove ALL persistent data from the host
    rm -rf /var/lib/dev-env

    # Uninstall k3s
    if [ -f /usr/local/bin/k3s-uninstall.sh ]; then
        /usr/local/bin/k3s-uninstall.sh
        echo "k3s uninstalled."
    fi

    echo "Full teardown complete."
else
    echo ""
    echo "Persistent data preserved at /var/lib/dev-env/."
    echo "Run setup.sh to restore the environment with existing data."
    echo "Use --full to also uninstall k3s and remove all data."
fi
