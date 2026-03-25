#!/usr/bin/env bash
# Waits for all pods in a namespace to be ready.
# Usage: wait-for-ready.sh <namespace> [timeout_seconds]

set -euo pipefail

NAMESPACE="${1:?Usage: wait-for-ready.sh <namespace> [timeout_seconds]}"
TIMEOUT="${2:-300}"
INTERVAL=5
ELAPSED=0

echo "Waiting for all pods in namespace '${NAMESPACE}' to be ready (timeout: ${TIMEOUT}s)..."

while true; do
    if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
        echo "ERROR: Timeout after ${TIMEOUT}s waiting for pods in '${NAMESPACE}'"
        kubectl get pods -n "${NAMESPACE}"
        exit 1
    fi

    # Count pods that are not ready
    NOT_READY=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null \
        | grep -v "Completed" \
        | grep -v "Running" \
        | wc -l || true)

    TOTAL=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null | wc -l || true)

    if [ "${TOTAL}" -eq 0 ]; then
        echo "  No pods found yet in '${NAMESPACE}'... (${ELAPSED}s)"
        sleep "${INTERVAL}"
        ELAPSED=$((ELAPSED + INTERVAL))
        continue
    fi

    # Check that all Running pods have all containers ready
    NOT_READY_CONTAINERS=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null \
        | grep "Running" \
        | awk '{split($2,a,"/"); if (a[1] != a[2]) print $1}' \
        | wc -l || true)

    PENDING=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null \
        | grep -E "Pending|ContainerCreating|Init:|PodInitializing" \
        | wc -l || true)

    FAILED=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null \
        | grep -E "Error|CrashLoopBackOff|ImagePullBackOff|ErrImagePull" \
        | wc -l || true)

    if [ "${FAILED}" -gt 0 ]; then
        echo "WARNING: ${FAILED} pod(s) in error state in '${NAMESPACE}':"
        kubectl get pods -n "${NAMESPACE}" --no-headers | grep -E "Error|CrashLoopBackOff|ImagePullBackOff|ErrImagePull"
    fi

    if [ "${PENDING}" -eq 0 ] && [ "${NOT_READY_CONTAINERS}" -eq 0 ] && [ "${FAILED}" -eq 0 ]; then
        echo "All pods in '${NAMESPACE}' are ready."
        kubectl get pods -n "${NAMESPACE}"
        exit 0
    fi

    echo "  ${PENDING} pending, ${NOT_READY_CONTAINERS} not fully ready, ${FAILED} failed (${ELAPSED}s)"
    sleep "${INTERVAL}"
    ELAPSED=$((ELAPSED + INTERVAL))
done
