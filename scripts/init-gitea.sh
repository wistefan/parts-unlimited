#!/usr/bin/env bash
# Initializes Gitea: creates the human user (with admin powers) and verifies the admin user.
# Must be run after Gitea is ready.

set -euo pipefail

GITEA_URL="${GITEA_URL:-http://localhost:3000}"
ADMIN_USERNAME="${GITEA_ADMIN_USERNAME:-claude}"
ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-password}"
HUMAN_USERNAME="${HUMAN_USERNAME:-wistefan}"
HUMAN_PASSWORD="${HUMAN_PASSWORD:-password}"
HUMAN_EMAIL="${HUMAN_EMAIL:-${HUMAN_USERNAME}@dev-env.local}"

# Wait for Gitea API to be reachable
echo "Waiting for Gitea API at ${GITEA_URL}..."
TIMEOUT=60
ELAPSED=0
until curl -sf "${GITEA_URL}/api/v1/version" >/dev/null 2>&1; do
    if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
        echo "ERROR: Gitea API not reachable at ${GITEA_URL} after ${TIMEOUT}s"
        exit 1
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done
echo "Gitea API is reachable."

AUTH="-u ${ADMIN_USERNAME}:${ADMIN_PASSWORD}"

# Verify admin user
echo "Verifying admin user '${ADMIN_USERNAME}'..."
ADMIN_CHECK=$(curl -sf ${AUTH} "${GITEA_URL}/api/v1/user" \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'{d[\"login\"]} (admin={d[\"is_admin\"]})')" 2>/dev/null || true)

if [ -z "${ADMIN_CHECK}" ]; then
    echo "ERROR: Cannot authenticate as '${ADMIN_USERNAME}'. Check credentials."
    exit 1
fi
echo "  Admin verified: ${ADMIN_CHECK}"

# Create human user with admin powers
echo "Creating human user '${HUMAN_USERNAME}' (admin)..."
EXISTING_USER=$(curl -sf ${AUTH} "${GITEA_URL}/api/v1/users/${HUMAN_USERNAME}" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('login',''))" 2>/dev/null || true)

if [ "${EXISTING_USER}" = "${HUMAN_USERNAME}" ]; then
    echo "  User '${HUMAN_USERNAME}' already exists. Ensuring admin privileges..."
    curl -sf ${AUTH} -X PATCH "${GITEA_URL}/api/v1/admin/users/${HUMAN_USERNAME}" \
        -H "Content-Type: application/json" \
        -d "{
            \"login_name\": \"${HUMAN_USERNAME}\",
            \"source_id\": 0,
            \"is_admin\": true
        }" >/dev/null
    echo "  Admin privileges ensured."
else
    curl -sf ${AUTH} -X POST "${GITEA_URL}/api/v1/admin/users" \
        -H "Content-Type: application/json" \
        -d "{
            \"username\": \"${HUMAN_USERNAME}\",
            \"password\": \"${HUMAN_PASSWORD}\",
            \"email\": \"${HUMAN_EMAIL}\",
            \"must_change_password\": false,
            \"is_admin\": true
        }" >/dev/null
    echo "  User '${HUMAN_USERNAME}' created with admin privileges."
fi

echo ""
echo "Gitea initialization complete."
echo "  URL:    ${GITEA_URL}"
echo "  Admin:  ${ADMIN_USERNAME} / ${ADMIN_PASSWORD}"
echo "  Human:  ${HUMAN_USERNAME} / ${HUMAN_PASSWORD} (admin)"
