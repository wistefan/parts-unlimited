#!/usr/bin/env bash
# Verifies that all dev-env components are running and accessible.

set -euo pipefail

GITEA_URL="${GITEA_URL:-http://localhost:3000}"
TAIGA_URL="${TAIGA_URL:-http://localhost:9000}"
HUMAN_USERNAME="${HUMAN_USERNAME:-wistefan}"

PASS=0
FAIL=0

check() {
    local name="$1"
    local cmd="$2"

    if eval "${cmd}" >/dev/null 2>&1; then
        echo "  [OK]   ${name}"
        PASS=$((PASS + 1))
    else
        echo "  [FAIL] ${name}"
        FAIL=$((FAIL + 1))
    fi
}

echo "Verifying dev-env..."
echo ""

echo "Kubernetes cluster:"
check "k3s node ready" "kubectl get nodes --no-headers | grep -q ' Ready'"
check "Namespace: gitea" "kubectl get namespace gitea"
check "Namespace: taiga" "kubectl get namespace taiga"
check "Namespace: agents" "kubectl get namespace agents"

echo ""
echo "Gitea:"
check "Gitea API reachable" "curl -sf ${GITEA_URL}/api/v1/version"
check "Gitea admin login" "curl -sf -u claude:password ${GITEA_URL}/api/v1/user"
check "Gitea human user '${HUMAN_USERNAME}' (admin)" "curl -sf -u claude:password ${GITEA_URL}/api/v1/users/${HUMAN_USERNAME} | python3 -c \"import sys,json; assert json.load(sys.stdin)['is_admin']\""

echo ""
echo "Taiga:"
check "Taiga API reachable" "curl -sf ${TAIGA_URL}/api/v1/"
check "Taiga admin login" "curl -sf -X POST ${TAIGA_URL}/api/v1/auth -H 'Content-Type: application/json' -d '{\"type\":\"normal\",\"username\":\"admin\",\"password\":\"password\"}'"

echo ""
echo "Pods:"
check "Gitea pods running" "kubectl get pods -n gitea --no-headers | grep -v Completed | grep -c Running"
check "Taiga pods running" "kubectl get pods -n taiga --no-headers | grep -v Completed | grep -c Running"

echo ""
echo "---"
echo "Results: ${PASS} passed, ${FAIL} failed"

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
