#!/usr/bin/env bash
# Initializes Taiga: creates superuser, human user (with admin powers),
# and configures the project with required statuses and swimlanes.
# Must be run after Taiga pods are ready.

set -euo pipefail

TAIGA_URL="${TAIGA_URL:-http://localhost:9000}"
ADMIN_USERNAME="${TAIGA_ADMIN_USERNAME:-admin}"
ADMIN_PASSWORD="${TAIGA_ADMIN_PASSWORD:-password}"
ADMIN_EMAIL="${TAIGA_ADMIN_EMAIL:-admin@dev-env.local}"
HUMAN_USERNAME="${HUMAN_USERNAME:-wistefan}"
HUMAN_PASSWORD="${HUMAN_PASSWORD:-password}"
HUMAN_EMAIL="${HUMAN_EMAIL:-${HUMAN_USERNAME}@dev-env.local}"
PROJECT_NAME="${TAIGA_PROJECT_NAME:-Dev Environment}"
PROJECT_SLUG="${TAIGA_PROJECT_SLUG:-dev-environment}"

READY_STATUS_NAME="ready"
IN_PROGRESS_STATUS_NAME="in progress"
READY_FOR_TEST_STATUS_NAME="ready for test"

# --- Create superuser via kubectl exec ---

echo "Creating Taiga superuser '${ADMIN_USERNAME}'..."

TAIGA_BACK_POD=$(kubectl get pods -n taiga -l app=taiga-back --no-headers -o custom-columns=":metadata.name" | head -1)

if [ -z "${TAIGA_BACK_POD}" ]; then
    echo "ERROR: No taiga-back pod found"
    exit 1
fi

# Create superuser (ignore error if already exists)
kubectl exec -n taiga "${TAIGA_BACK_POD}" -- python manage.py shell -c "
from django.contrib.auth import get_user_model
User = get_user_model()
if not User.objects.filter(username='${ADMIN_USERNAME}').exists():
    User.objects.create_superuser('${ADMIN_USERNAME}', '${ADMIN_EMAIL}', '${ADMIN_PASSWORD}')
    print('Superuser created.')
else:
    print('Superuser already exists.')
" 2>/dev/null || echo "WARNING: Could not create superuser via kubectl exec."

# --- Create human user with admin powers ---

echo "Creating human user '${HUMAN_USERNAME}' (superuser)..."
kubectl exec -n taiga "${TAIGA_BACK_POD}" -- python manage.py shell -c "
from django.contrib.auth import get_user_model
User = get_user_model()
if not User.objects.filter(username='${HUMAN_USERNAME}').exists():
    User.objects.create_superuser('${HUMAN_USERNAME}', '${HUMAN_EMAIL}', '${HUMAN_PASSWORD}')
    print('Human user created as superuser.')
else:
    u = User.objects.get(username='${HUMAN_USERNAME}')
    if not u.is_superuser:
        u.is_superuser = True
        u.is_staff = True
        u.save()
        print('Human user promoted to superuser.')
    else:
        print('Human user already exists as superuser.')
" 2>/dev/null || echo "WARNING: Could not create human user via kubectl exec."

# Wait for API to be reachable
echo "Waiting for Taiga API at ${TAIGA_URL}..."
TIMEOUT=60
ELAPSED=0
until curl -sf "${TAIGA_URL}/api/v1/" >/dev/null 2>&1; do
    if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
        echo "ERROR: Taiga API not reachable at ${TAIGA_URL} after ${TIMEOUT}s"
        exit 1
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done
echo "Taiga API is reachable."

# --- Authenticate ---

echo "Authenticating as '${ADMIN_USERNAME}'..."
AUTH_RESPONSE=$(curl -sf -X POST "${TAIGA_URL}/api/v1/auth" \
    -H "Content-Type: application/json" \
    -d "{\"type\": \"normal\", \"username\": \"${ADMIN_USERNAME}\", \"password\": \"${ADMIN_PASSWORD}\"}")

AUTH_TOKEN=$(echo "${AUTH_RESPONSE}" | python3 -c "import sys,json; print(json.load(sys.stdin)['auth_token'])")

if [ -z "${AUTH_TOKEN}" ]; then
    echo "ERROR: Failed to authenticate with Taiga"
    exit 1
fi
echo "Authenticated."

AUTH_HEADER="Authorization: Bearer ${AUTH_TOKEN}"

# --- Create project ---

echo "Creating project '${PROJECT_NAME}'..."

# Check if project already exists
EXISTING_PROJECT=$(curl -sf -H "${AUTH_HEADER}" "${TAIGA_URL}/api/v1/projects?member=${ADMIN_USERNAME}" \
    | python3 -c "
import sys, json
projects = json.load(sys.stdin)
for p in projects:
    if p['slug'] == '${PROJECT_SLUG}':
        print(p['id'])
        break
" 2>/dev/null || true)

if [ -n "${EXISTING_PROJECT}" ]; then
    PROJECT_ID="${EXISTING_PROJECT}"
    echo "Project already exists (ID: ${PROJECT_ID})"
else
    PROJECT_RESPONSE=$(curl -sf -X POST "${TAIGA_URL}/api/v1/projects" \
        -H "${AUTH_HEADER}" \
        -H "Content-Type: application/json" \
        -d "{
            \"name\": \"${PROJECT_NAME}\",
            \"description\": \"AI agent development orchestration project\",
            \"is_backlog_activated\": true,
            \"is_kanban_activated\": true,
            \"is_wiki_activated\": false,
            \"is_issues_activated\": false
        }")
    PROJECT_ID=$(echo "${PROJECT_RESPONSE}" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
    echo "Project created (ID: ${PROJECT_ID})"
fi

# --- Configure user story statuses ---

echo "Configuring user story statuses..."

# Get existing statuses
EXISTING_STATUSES=$(curl -sf -H "${AUTH_HEADER}" \
    "${TAIGA_URL}/api/v1/userstory-statuses?project=${PROJECT_ID}")

# Helper: create or update a status
create_or_update_status() {
    local name="$1"
    local order="$2"
    local is_closed="$3"
    local color="$4"

    local existing_id
    existing_id=$(echo "${EXISTING_STATUSES}" | python3 -c "
import sys, json
statuses = json.load(sys.stdin)
for s in statuses:
    if s['name'].lower() == '${name}'.lower():
        print(s['id'])
        break
" 2>/dev/null || true)

    if [ -n "${existing_id}" ]; then
        curl -sf -X PATCH "${TAIGA_URL}/api/v1/userstory-statuses/${existing_id}" \
            -H "${AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"name\": \"${name}\", \"order\": ${order}, \"is_closed\": ${is_closed}, \"color\": \"${color}\"}" \
            >/dev/null
        echo "  Updated status: ${name} (ID: ${existing_id})"
    else
        local response
        response=$(curl -sf -X POST "${TAIGA_URL}/api/v1/userstory-statuses" \
            -H "${AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"name\": \"${name}\", \"project\": ${PROJECT_ID}, \"order\": ${order}, \"is_closed\": ${is_closed}, \"color\": \"${color}\"}")
        local new_id
        new_id=$(echo "${response}" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
        echo "  Created status: ${name} (ID: ${new_id})"
    fi
}

create_or_update_status "${READY_STATUS_NAME}" 1 false "#70728F"
create_or_update_status "${IN_PROGRESS_STATUS_NAME}" 2 false "#E47C40"
create_or_update_status "${READY_FOR_TEST_STATUS_NAME}" 3 false "#A8E440"

# --- Configure swimlanes for specializations ---

echo "Configuring swimlanes..."

SPECIALIZATIONS=("General" "Frontend" "Backend" "Test" "Documentation" "Operations")
SWIMLANE_ORDER=1

for spec in "${SPECIALIZATIONS[@]}"; do
    existing_swimlane=$(curl -sf -H "${AUTH_HEADER}" \
        "${TAIGA_URL}/api/v1/swimlanes?project=${PROJECT_ID}" \
        | python3 -c "
import sys, json
for s in json.load(sys.stdin):
    if s['name'] == '${spec}':
        print(s['id'])
        break
" 2>/dev/null || true)

    if [ -z "${existing_swimlane}" ]; then
        curl -sf -X POST "${TAIGA_URL}/api/v1/swimlanes" \
            -H "${AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"name\": \"${spec}\", \"project\": ${PROJECT_ID}, \"order\": ${SWIMLANE_ORDER}}" \
            >/dev/null
        echo "  Created swimlane: ${spec}"
    else
        echo "  Swimlane already exists: ${spec}"
    fi
    SWIMLANE_ORDER=$((SWIMLANE_ORDER + 1))
done

# --- Add human user as project member ---

echo "Adding '${HUMAN_USERNAME}' to project..."

# Get the admin role (highest privilege) for membership
ADMIN_ROLE_ID=$(curl -sf -H "${AUTH_HEADER}" "${TAIGA_URL}/api/v1/roles?project=${PROJECT_ID}" \
    | python3 -c "
import sys, json
roles = json.load(sys.stdin)
# Pick the role with the most permissions (typically the first/admin role)
best = roles[0]
for r in roles:
    if len(r.get('permissions', [])) > len(best.get('permissions', [])):
        best = r
print(best['id'])
" 2>/dev/null || true)

if [ -n "${ADMIN_ROLE_ID}" ]; then
    curl -sf -X POST "${TAIGA_URL}/api/v1/memberships" \
        -H "${AUTH_HEADER}" \
        -H "Content-Type: application/json" \
        -d "{\"project\": ${PROJECT_ID}, \"role\": ${ADMIN_ROLE_ID}, \"username\": \"${HUMAN_USERNAME}\"}" \
        >/dev/null 2>&1 || true
    echo "  ${HUMAN_USERNAME} added to project with admin role."
fi

echo ""
echo "Taiga initialization complete."
echo "  URL:      ${TAIGA_URL}"
echo "  Admin:    ${ADMIN_USERNAME} / ${ADMIN_PASSWORD}"
echo "  Human:    ${HUMAN_USERNAME} / ${HUMAN_PASSWORD} (superuser)"
echo "  Project:  ${PROJECT_NAME}"
