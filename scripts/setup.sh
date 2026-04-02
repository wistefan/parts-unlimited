#!/usr/bin/env bash
# Master setup script for the dev-env.
# Installs k3s, deploys Gitea and Taiga, and initializes both services.
#
# Usage: sudo ./scripts/setup.sh
#
# Prerequisites:
#   - curl, helm, python3 must be installed
#   - Ports 3001 (Gitea) and 9000 (Taiga) must be free (configurable via env vars)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Configurable via environment
GITEA_PORT="${GITEA_PORT:-3001}"
TAIGA_PORT="${TAIGA_PORT:-9000}"
TAIGA_SECRET_KEY="${TAIGA_SECRET_KEY:-$(python3 -c "import secrets; print(secrets.token_hex(32))")}"
HUMAN_USERNAME="${HUMAN_USERNAME:-wistefan}"
HUMAN_PASSWORD="${HUMAN_PASSWORD:-password}"
HUMAN_EMAIL="${HUMAN_EMAIL:-${HUMAN_USERNAME}@dev-env.local}"

# Default port value used in static k8s manifests.  When GITEA_PORT differs,
# apply_manifest() rewrites the port on-the-fly so the files on disk stay as
# documentation/reference while the deployed resources use the configured value.
GITEA_DEFAULT_PORT=3001

# --- Helpers ---

# Apply a k8s manifest with port substitution.
# Replaces the default Gitea and Taiga in-cluster service ports with the
# values configured via GITEA_PORT / TAIGA_PORT.
apply_manifest() {
    local file="$1"
    sed \
        -e "s|gitea-http\.gitea\.svc\.cluster\.local:${GITEA_DEFAULT_PORT}|gitea-http.gitea.svc.cluster.local:${GITEA_PORT}|g" \
        -e "s|localhost:${GITEA_DEFAULT_PORT}|localhost:${GITEA_PORT}|g" \
        "${file}" | kubectl apply -f -
}

# Apply a network policy manifest, replacing the default Gitea service port.
apply_network_policy() {
    local file="$1"
    # The Gitea port appears as a bare number under the gitea namespace rule.
    # Use awk to replace only the port line immediately after the gitea selector.
    awk -v old="${GITEA_DEFAULT_PORT}" -v new="${GITEA_PORT}" '
        /name: gitea/ { in_gitea=1 }
        in_gitea && /port:/ && $NF == old { sub(old, new); in_gitea=0 }
        { print }
    ' "${file}" | kubectl apply -f -
}

# --- Pre-flight checks ---

echo "=== Dev-Env Setup ==="
echo ""

# Check root
if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This script must be run as root (sudo)."
    exit 1
fi

# Determine the real user (when running with sudo)
REAL_USER="${SUDO_USER:-$(whoami)}"
REAL_HOME=$(eval echo "~${REAL_USER}")

# Check prerequisites
for cmd in curl helm python3; do
    if ! command -v "${cmd}" &>/dev/null; then
        echo "ERROR: '${cmd}' is required but not installed."
        exit 1
    fi
done

# Check port availability
for port in "${GITEA_PORT}" "${TAIGA_PORT}"; do
    if ss -tlnp 2>/dev/null | grep -q ":${port} "; then
        echo "ERROR: Port ${port} is already in use."
        echo "  Stop the conflicting service or set a different port:"
        echo "  GITEA_PORT=3002 TAIGA_PORT=9001 sudo -E ./scripts/setup.sh"
        exit 1
    fi
done

echo "Pre-flight checks passed."
echo ""

# --- Step 1: Install k3s ---

echo "=== Step 1: k3s ==="

if ! command -v k3s &>/dev/null; then
    bash "${SCRIPT_DIR}/install-k3s.sh"
else
    echo "k3s already installed: $(k3s --version)"
fi

# Ensure kubeconfig is accessible
export KUBECONFIG="/etc/rancher/k3s/k3s.yaml"

# Copy kubeconfig for the real user
KUBE_DIR="${REAL_HOME}/.kube"
mkdir -p "${KUBE_DIR}"
cp "${KUBECONFIG}" "${KUBE_DIR}/config"
chown -R "${REAL_USER}:${REAL_USER}" "${KUBE_DIR}"

# Wait for k3s node to be ready
echo "Waiting for k3s node..."
kubectl wait --for=condition=Ready node --all --timeout=120s

echo ""

# --- Step 2: Namespaces ---

echo "=== Step 2: Namespaces and RBAC ==="
kubectl apply -f "${PROJECT_DIR}/k8s/namespaces.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/agents/rbac.yaml"
apply_network_policy "${PROJECT_DIR}/k8s/agents/network-policy.yaml"
# Apply service endpoints ConfigMap (extracted from job-template.yaml)
kubectl apply -f - <<ENDPOINTS
apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-service-endpoints
  namespace: agents
data:
  GITEA_URL: "http://gitea-http.gitea.svc.cluster.local:${GITEA_PORT}"
  TAIGA_URL: "http://taiga-gateway.taiga.svc.cluster.local:${TAIGA_PORT}"
ENDPOINTS
echo ""

# --- Step 3: Cache external container images ---

echo "=== Step 3: Image Cache ==="
echo "Pre-pulling external images to avoid Docker Hub rate limits..."
bash "${SCRIPT_DIR}/cache-images.sh"
echo ""

# --- Step 4: Shared volume directories ---

echo "=== Step 4: Shared volumes ==="
# Taiga directories — static/media (UID 999) and PostgreSQL (UID 999)
mkdir -p /var/lib/dev-env/taiga/static
mkdir -p /var/lib/dev-env/taiga/media
mkdir -p /var/lib/dev-env/taiga/db
mkdir -p /var/lib/dev-env/taiga/rabbitmq
chown -R 999:999 /var/lib/dev-env/taiga

# RabbitMQ runs as UID 100 (rabbitmq user in alpine image)
chown -R 100:100 /var/lib/dev-env/taiga/rabbitmq

# Gitea directories — data (UID 1000, git user) and PostgreSQL (UID 1001, bitnami)
mkdir -p /var/lib/dev-env/gitea/data
mkdir -p /var/lib/dev-env/gitea/postgresql
chown -R 1000:1000 /var/lib/dev-env/gitea/data
chown -R 1001:1001 /var/lib/dev-env/gitea/postgresql

echo "Created host directories for persistent volumes."
echo ""

# --- Step 5: Deploy Gitea ---

echo "=== Step 5: Gitea ==="

# Create hostPath-backed PVs and PVCs before Helm touches the namespace.
# These must exist before helm install so the chart binds to them instead
# of creating its own PVCs via the default StorageClass.
kubectl apply -f "${PROJECT_DIR}/k8s/gitea/volumes.yaml"

# Add Helm repo
helm repo add gitea-charts https://dl.gitea.io/charts/ 2>/dev/null || true
helm repo update >/dev/null

# Install or upgrade Gitea
if helm status gitea -n gitea &>/dev/null; then
    echo "Gitea already deployed, upgrading..."
    helm upgrade gitea gitea-charts/gitea \
        -n gitea \
        -f "${PROJECT_DIR}/k8s/gitea/values.yaml" \
        --set "service.http.port=${GITEA_PORT}" \
        --set "gitea.config.server.ROOT_URL=http://localhost:${GITEA_PORT}" \
        --wait --timeout 5m
else
    echo "Installing Gitea..."
    helm install gitea gitea-charts/gitea \
        -n gitea \
        -f "${PROJECT_DIR}/k8s/gitea/values.yaml" \
        --set "service.http.port=${GITEA_PORT}" \
        --set "gitea.config.server.ROOT_URL=http://localhost:${GITEA_PORT}" \
        --wait --timeout 5m
fi

echo "Waiting for Gitea pods..."
bash "${SCRIPT_DIR}/wait-for-ready.sh" gitea 300
echo ""

# --- Step 6: Deploy Taiga ---

echo "=== Step 6: Taiga ==="

# Generate and inject the secret key
TAIGA_SECRET_YAML="${PROJECT_DIR}/k8s/taiga/secret.yaml"
sed "s|REPLACE_ME_WITH_GENERATED_SECRET|${TAIGA_SECRET_KEY}|" \
    "${TAIGA_SECRET_YAML}" | kubectl apply -f -

# Apply all Taiga manifests (order: configmaps, volumes, then services)
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/configmap.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/volumes.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/rabbitmq.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/postgres.yaml"

echo "Waiting for Taiga database and RabbitMQ..."
kubectl wait --for=condition=Ready pod -l app=taiga-db -n taiga --timeout=120s 2>/dev/null || true
kubectl wait --for=condition=Ready pod -l app=taiga-rabbitmq -n taiga --timeout=120s 2>/dev/null || true

kubectl apply -f "${PROJECT_DIR}/k8s/taiga/back.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/async.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/events.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/protected.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/front.yaml"
kubectl apply -f "${PROJECT_DIR}/k8s/taiga/gateway.yaml"

echo "Waiting for all Taiga pods..."
bash "${SCRIPT_DIR}/wait-for-ready.sh" taiga 600
echo ""

# --- Step 7: Initialize services ---

echo "=== Step 7: Initialization ==="

echo "--- Initializing Gitea ---"
GITEA_URL="http://localhost:${GITEA_PORT}" \
    HUMAN_USERNAME="${HUMAN_USERNAME}" \
    HUMAN_PASSWORD="${HUMAN_PASSWORD}" \
    HUMAN_EMAIL="${HUMAN_EMAIL}" \
    bash "${SCRIPT_DIR}/init-gitea.sh"
echo ""

echo "--- Initializing Taiga ---"
TAIGA_URL="http://localhost:${TAIGA_PORT}" \
    HUMAN_USERNAME="${HUMAN_USERNAME}" \
    HUMAN_PASSWORD="${HUMAN_PASSWORD}" \
    HUMAN_EMAIL="${HUMAN_EMAIL}" \
    bash "${SCRIPT_DIR}/init-taiga.sh"
echo ""

# --- Step 8: Agent policies ---

echo "=== Step 8: Agent policies ==="
kubectl apply -f "${PROJECT_DIR}/k8s/agents/policies.yaml"
echo ""

# --- Step 9: Anthropic API key ---

echo "=== Step 9: Claude Credentials ==="
CLAUDE_CREDENTIALS="${REAL_HOME}/.claude/.credentials.json"

# Two credential mechanisms are supported. If ANTHROPIC_API_KEY is set, it is
# stored as a dedicated Secret and takes precedence at runtime. The credentials
# file is always set up as a fallback (from ~/.claude/.credentials.json).

if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    kubectl create secret generic anthropic-api-key \
        --namespace=agents \
        --from-literal=api-key="${ANTHROPIC_API_KEY}" \
        --dry-run=client -o yaml | kubectl apply -f -
    echo "  API key secret created from ANTHROPIC_API_KEY."
fi

if [ -f "${CLAUDE_CREDENTIALS}" ]; then
    # Copy the host's Claude Code credentials file into the cluster.
    # Each agent container gets its own copy and handles OAuth refresh
    # independently.
    kubectl create secret generic claude-credentials \
        --namespace=agents \
        --from-file=credentials.json="${CLAUDE_CREDENTIALS}" \
        --dry-run=client -o yaml | kubectl apply -f -
    echo "  Credentials file secret created from ${CLAUDE_CREDENTIALS}."
fi

# Check that at least one credential source is available
if [ -z "${ANTHROPIC_API_KEY:-}" ] && [ ! -f "${CLAUDE_CREDENTIALS}" ]; then
    if kubectl get secret anthropic-api-key -n agents &>/dev/null || \
       kubectl get secret claude-credentials -n agents &>/dev/null; then
        echo "  Using existing credential secrets."
    else
        echo "  WARNING: No Claude credentials found. Checked:"
        echo "    - ANTHROPIC_API_KEY environment variable"
        echo "    - ${CLAUDE_CREDENTIALS}"
        echo "  Agents will not be able to run until credentials are provided."
        echo "  Either run 'claude login' and re-run setup, or provide an API key:"
        echo "    ANTHROPIC_API_KEY='sk-ant-...' sudo -E ./scripts/setup.sh"
    fi
fi
echo ""

# --- Step 10: CI/CD Runner (optional) ---

echo "=== Step 10: CI/CD Runner ==="
# The act-runner needs a registration token from Gitea.
# Generate one via the Gitea API and create the secret.
echo "Generating Gitea Actions runner registration token..."
RUNNER_TOKEN=$(curl -sf -u "${GITEA_ADMIN_USERNAME:-claude}:${GITEA_ADMIN_PASSWORD:-password}" \
    -X POST "http://localhost:${GITEA_PORT}/api/v1/admin/runners/registration-token" \
    -H "Content-Type: application/json" \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || true)

if [ -n "${RUNNER_TOKEN}" ]; then
    kubectl create secret generic act-runner-token \
        --namespace gitea \
        --from-literal=token="${RUNNER_TOKEN}" \
        --dry-run=client -o yaml | kubectl apply -f -
    apply_manifest "${PROJECT_DIR}/k8s/gitea/act-runner.yaml"
    echo "  Act runner deployed."
else
    echo "  WARNING: Could not generate runner token. Gitea Actions runner not deployed."
    echo "  You can deploy it later by creating the act-runner-token secret manually."
fi
echo ""

# --- Step 11: Build container images ---

echo "=== Step 11: Container Images ==="

# Check for Docker or nerdctl (k3s ships with nerdctl-compatible ctr)
if command -v docker &>/dev/null; then
    BUILD_CMD="docker build"
    SAVE_CMD="docker save"
elif command -v nerdctl &>/dev/null; then
    BUILD_CMD="nerdctl build"
    SAVE_CMD="nerdctl save"
else
    echo "  WARNING: Neither docker nor nerdctl found."
    echo "  Container images must be built manually."
    echo "  See README for build instructions."
    BUILD_CMD=""
fi

if [ -n "${BUILD_CMD}" ]; then
    echo "Building orchestrator image..."
    ${BUILD_CMD} -t orchestrator:latest "${PROJECT_DIR}/orchestrator"
    echo "  orchestrator:latest built."

    echo "Building agent-worker image..."
    ${BUILD_CMD} -t agent-worker:latest "${PROJECT_DIR}/agent"
    echo "  agent-worker:latest built."

    # Import images into k3s containerd.
    # setup.sh runs as root so we access containerd directly (no sudo).
    if [ "${BUILD_CMD}" = "docker build" ]; then
        echo "Importing images into k3s containerd..."
        for img in orchestrator:latest agent-worker:latest; do
            docker save "${img}" | k3s ctr images import -
            # Ensure both short and fully-qualified tags exist so k3s
            # resolves the image regardless of which form it uses.
            fq="docker.io/library/${img}"
            if k3s ctr images ls -q | grep -qF "${fq}"; then
                k3s ctr images tag "${fq}" "${img}" 2>/dev/null || true
            elif k3s ctr images ls -q | grep -qF "${img}"; then
                k3s ctr images tag "${img}" "${fq}" 2>/dev/null || true
            fi
        done
        # Verify
        for img in orchestrator agent-worker; do
            if k3s ctr images ls -q | grep -qF "${img}"; then
                echo "  ${img}: OK"
            else
                echo "  ERROR: ${img} not found in k3s after import!"
                exit 1
            fi
        done
    fi
fi
echo ""

# --- Step 12: Deploy orchestrator ---

echo "=== Step 12: Orchestrator ==="
apply_manifest "${PROJECT_DIR}/k8s/agents/orchestrator.yaml"
# Force a rollout to pick up new image (idempotent on first deploy)
kubectl rollout restart deployment/orchestrator -n agents 2>/dev/null || true
echo "Waiting for orchestrator to be ready..."
kubectl rollout status deployment/orchestrator -n agents --timeout=120s 2>/dev/null || true
echo "  Orchestrator deployed."
echo ""

# --- Step 13: Verify ---

echo "=== Step 13: Verification ==="
GITEA_URL="http://localhost:${GITEA_PORT}" \
    TAIGA_URL="http://localhost:${TAIGA_PORT}" \
    HUMAN_USERNAME="${HUMAN_USERNAME}" \
    bash "${SCRIPT_DIR}/verify.sh"

echo ""
echo "=========================================="
echo "  Dev-Env setup complete!"
echo ""
echo "  Gitea:  http://localhost:${GITEA_PORT}"
echo "    Admin: claude / password"
echo "    Human: ${HUMAN_USERNAME} / ${HUMAN_PASSWORD} (admin)"
echo ""
echo "  Taiga:  http://localhost:${TAIGA_PORT}"
echo "    Admin: admin / password"
echo "    Human: ${HUMAN_USERNAME} / ${HUMAN_PASSWORD} (superuser)"
echo ""
echo "  kubectl: export KUBECONFIG=/etc/rancher/k3s/k3s.yaml"
echo "=========================================="
