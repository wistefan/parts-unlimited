#!/usr/bin/env bash
# Installs k3s on the local machine.
# Must be run with sudo/root privileges.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# k3s configuration directory
K3S_CONFIG_DIR="/etc/rancher/k3s"
K3S_CONFIG_FILE="${K3S_CONFIG_DIR}/config.yaml"

if command -v k3s &>/dev/null; then
    echo "k3s is already installed: $(k3s --version)"
    echo "To reinstall, run: /usr/local/bin/k3s-uninstall.sh"
    exit 0
fi

echo "Installing k3s..."

# Create config directory
mkdir -p "${K3S_CONFIG_DIR}"

# Write k3s configuration
cat > "${K3S_CONFIG_FILE}" <<'EOF'
# Make kubeconfig readable without sudo
write-kubeconfig-mode: "0644"
# Disable Traefik — we use NodePort/LoadBalancer directly
# Uncomment the line below if Traefik causes port conflicts:
# disable:
#   - traefik
EOF

# Install k3s
curl -sfL https://get.k3s.io | sh -

# Wait for k3s to be ready
echo "Waiting for k3s to start..."
TIMEOUT=120
ELAPSED=0
until k3s kubectl get nodes &>/dev/null; do
    if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
        echo "ERROR: k3s did not become ready within ${TIMEOUT}s"
        exit 1
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done

echo "k3s is ready."
k3s kubectl get nodes

# Set up kubeconfig symlink for kubectl
KUBECONFIG_PATH="${K3S_CONFIG_DIR}/k3s.yaml"
if [ -f "${KUBECONFIG_PATH}" ]; then
    KUBE_DIR="${HOME}/.kube"
    mkdir -p "${KUBE_DIR}"
    if [ ! -f "${KUBE_DIR}/config" ]; then
        cp "${KUBECONFIG_PATH}" "${KUBE_DIR}/config"
        echo "Copied kubeconfig to ${KUBE_DIR}/config"
    else
        echo "NOTE: ${KUBE_DIR}/config already exists."
        echo "To use k3s cluster: export KUBECONFIG=${KUBECONFIG_PATH}"
    fi
fi

echo "k3s installation complete."
