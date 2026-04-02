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

# --- Wait for Taiga API to be reachable ---

echo "Waiting for Taiga API at ${TAIGA_URL}..."
TIMEOUT=120
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

# --- Create superuser via kubectl exec ---

TAIGA_BACK_POD=$(kubectl get pods -n taiga -l app=taiga-back --no-headers -o custom-columns=":metadata.name" | head -1)

if [ -z "${TAIGA_BACK_POD}" ]; then
    echo "ERROR: No taiga-back pod found"
    exit 1
fi

echo "Creating Taiga superuser '${ADMIN_USERNAME}'..."
kubectl exec -n taiga "${TAIGA_BACK_POD}" -- python manage.py shell -c "
from django.contrib.auth import get_user_model
User = get_user_model()
if not User.objects.filter(username='${ADMIN_USERNAME}').exists():
    User.objects.create_superuser('${ADMIN_USERNAME}', '${ADMIN_EMAIL}', '${ADMIN_PASSWORD}')
    print('Superuser created.')
else:
    print('Superuser already exists.')
"
echo "  Done."

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
"
echo "  Done."

# --- Authenticate ---

echo "Authenticating as '${ADMIN_USERNAME}'..."
AUTH_RESPONSE=$(curl -s -X POST "${TAIGA_URL}/api/v1/auth" \
    -H "Content-Type: application/json" \
    -d "{\"type\": \"normal\", \"username\": \"${ADMIN_USERNAME}\", \"password\": \"${ADMIN_PASSWORD}\"}")

AUTH_TOKEN=$(echo "${AUTH_RESPONSE}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('auth_token',''))" 2>/dev/null)

if [ -z "${AUTH_TOKEN}" ]; then
    echo "ERROR: Failed to authenticate with Taiga."
    echo "  Response: ${AUTH_RESPONSE}"
    exit 1
fi
echo "  Authenticated."

AUTH_HEADER="Authorization: Bearer ${AUTH_TOKEN}"

# --- Create project ---

echo "Creating project '${PROJECT_NAME}'..."

# Check if project already exists
EXISTING_PROJECT=$(curl -s -H "${AUTH_HEADER}" "${TAIGA_URL}/api/v1/projects" \
    | python3 -c "
import sys, json
projects = json.load(sys.stdin)
for p in projects:
    if p.get('slug') == '${PROJECT_SLUG}':
        print(p['id'])
        break
" 2>/dev/null || true)

# Resolve Kanban template ID
KANBAN_TEMPLATE_ID=$(curl -s -H "${AUTH_HEADER}" "${TAIGA_URL}/api/v1/project-templates" \
    | python3 -c "
import sys, json
for t in json.load(sys.stdin):
    if t.get('slug') == 'kanban':
        print(t['id'])
        break
" 2>/dev/null || true)
echo "  Kanban template ID: ${KANBAN_TEMPLATE_ID:-not found}"

if [ -n "${EXISTING_PROJECT}" ]; then
    PROJECT_ID="${EXISTING_PROJECT}"
    echo "  Project already exists (ID: ${PROJECT_ID}), ensuring Kanban mode..."
else
    PROJECT_RESPONSE=$(curl -s -X POST "${TAIGA_URL}/api/v1/projects" \
        -H "${AUTH_HEADER}" \
        -H "Content-Type: application/json" \
        -d "{
            \"name\": \"${PROJECT_NAME}\",
            \"description\": \"AI agent development orchestration project\",
            \"creation_template\": ${KANBAN_TEMPLATE_ID:-2},
            \"is_backlog_activated\": false,
            \"is_kanban_activated\": true,
            \"is_wiki_activated\": false,
            \"is_issues_activated\": false
        }")
    PROJECT_ID=$(echo "${PROJECT_RESPONSE}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
    if [ -z "${PROJECT_ID}" ]; then
        echo "ERROR: Failed to create project."
        echo "  Response: ${PROJECT_RESPONSE}"
        exit 1
    fi
    echo "  Project created (ID: ${PROJECT_ID})"
fi

# Ensure Kanban mode is active (handles both new and existing projects)
curl -s -X PATCH "${TAIGA_URL}/api/v1/projects/${PROJECT_ID}" \
    -H "${AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    -d '{"is_backlog_activated": false, "is_kanban_activated": true}' >/dev/null
echo "  Kanban mode enabled."

# --- Configure user story statuses ---

echo "Configuring user story statuses..."

# Get existing statuses
EXISTING_STATUSES=$(curl -s -H "${AUTH_HEADER}" \
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
        curl -s -X PATCH "${TAIGA_URL}/api/v1/userstory-statuses/${existing_id}" \
            -H "${AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"name\": \"${name}\", \"order\": ${order}, \"is_closed\": ${is_closed}, \"color\": \"${color}\"}" \
            >/dev/null
        echo "  Updated status: ${name} (ID: ${existing_id})"
    else
        local response
        response=$(curl -s -X POST "${TAIGA_URL}/api/v1/userstory-statuses" \
            -H "${AUTH_HEADER}" \
            -H "Content-Type: application/json" \
            -d "{\"name\": \"${name}\", \"project\": ${PROJECT_ID}, \"order\": ${order}, \"is_closed\": ${is_closed}, \"color\": \"${color}\"}")
        local new_id
        new_id=$(echo "${response}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || true)
        if [ -n "${new_id}" ]; then
            echo "  Created status: ${name} (ID: ${new_id})"
        else
            echo "  WARNING: Could not create status '${name}': ${response}"
        fi
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
    existing_swimlane=$(curl -s -H "${AUTH_HEADER}" \
        "${TAIGA_URL}/api/v1/swimlanes?project=${PROJECT_ID}" \
        | python3 -c "
import sys, json
for s in json.load(sys.stdin):
    if s['name'] == '${spec}':
        print(s['id'])
        break
" 2>/dev/null || true)

    if [ -z "${existing_swimlane}" ]; then
        curl -s -X POST "${TAIGA_URL}/api/v1/swimlanes" \
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

# --- Add human user as project member (via Django ORM) ---
# The Taiga REST API requires users to be "contacts" before they can be added
# as project members. Since this is a fresh local setup with no social graph,
# we bypass the API and create the membership directly via Django ORM.

echo "Adding '${HUMAN_USERNAME}' to project as admin member..."
kubectl exec -n taiga "${TAIGA_BACK_POD}" -- python manage.py shell -c "
from taiga.projects.models import Project, Membership
from taiga.users.models import User

project = Project.objects.get(id=${PROJECT_ID})
user = User.objects.get(username='${HUMAN_USERNAME}')
role = project.roles.order_by('-order').first()

if not Membership.objects.filter(project=project, user=user).exists():
    Membership.objects.create(project=project, user=user, role=role, is_admin=True)
    print('Membership created with admin privileges.')
else:
    m = Membership.objects.get(project=project, user=user)
    m.is_admin = True
    m.save()
    print('Membership updated with admin privileges.')
"
echo "  Done."

echo ""
echo "Taiga initialization complete."
echo "  URL:      ${TAIGA_URL}"
echo "  Admin:    ${ADMIN_USERNAME} / ${ADMIN_PASSWORD}"
echo "  Human:    ${HUMAN_USERNAME} / ${HUMAN_PASSWORD} (superuser)"
echo "  Project:  ${PROJECT_NAME} (ID: ${PROJECT_ID})"
