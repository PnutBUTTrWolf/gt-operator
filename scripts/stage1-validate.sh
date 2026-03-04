#!/usr/bin/env bash
# Stage 1 validation: verify Dolt StatefulSet is running in k8s and accessible
# via port-forward. Tests connectivity, basic SQL operations, and PVC durability.
#
# Usage:
#   ./scripts/stage1-validate.sh [namespace] [port]
#
# Prerequisites:
#   - kubectl configured and cluster accessible
#   - Dolt deployed via: make deploy-stage1
#   - Port-forward active: make port-forward-dolt (in another terminal)
#   - mysql client OR dolt CLI available locally

set -euo pipefail

NAMESPACE="${1:-gastown}"
PORT="${2:-3307}"
HOST="127.0.0.1"
PASS=0
FAIL=0
TOTAL=0

check() {
    local name="$1"
    shift
    TOTAL=$((TOTAL + 1))
    if eval "$@" >/dev/null 2>&1; then
        echo "  PASS  $name"
        PASS=$((PASS + 1))
    else
        echo "  FAIL  $name"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== Stage 1 Validation: Dolt StatefulSet in k8s ==="
echo ""

# --- Section 1: Kubernetes resources ---
echo "--- K8s Resources ---"

check "StatefulSet exists" \
    "kubectl get statefulset dolt -n $NAMESPACE"

check "StatefulSet ready (1/1)" \
    "kubectl wait --for=condition=ready pod/dolt-0 -n $NAMESPACE --timeout=60s"

check "ClusterIP Service exists" \
    "kubectl get service dolt-svc -n $NAMESPACE"

check "PVC bound" \
    "kubectl get pvc dolt-data-dolt-0 -n $NAMESPACE -o jsonpath='{.status.phase}' | grep -q Bound"

echo ""

# --- Section 2: Pod health ---
echo "--- Pod Health ---"

check "Pod running" \
    "kubectl get pod dolt-0 -n $NAMESPACE -o jsonpath='{.status.phase}' | grep -q Running"

check "Readiness probe passing" \
    'kubectl get pod dolt-0 -n '"$NAMESPACE"' -o jsonpath='"'"'{.status.conditions[?(@.type=="Ready")].status}'"'"' | grep -q True'

check "No restarts" \
    '[ "$(kubectl get pod dolt-0 -n '"$NAMESPACE"' -o jsonpath='"'"'{.status.containerStatuses[0].restartCount}'"'"')" = "0" ]'

echo ""

# --- Section 3: Connectivity (requires port-forward) ---
echo "--- Connectivity (via port-forward on $HOST:$PORT) ---"

# Detect SQL client
SQL_CLIENT=""
if command -v mysql >/dev/null 2>&1; then
    SQL_CLIENT="mysql"
elif command -v dolt >/dev/null 2>&1; then
    SQL_CLIENT="dolt"
fi

run_sql() {
    local query="$1"
    if [ "$SQL_CLIENT" = "mysql" ]; then
        mysql -h "$HOST" -P "$PORT" -u root --skip-ssl -e "$query" 2>&1
    elif [ "$SQL_CLIENT" = "dolt" ]; then
        dolt sql -q "$query" --host "$HOST" --port "$PORT" --user root 2>&1
    else
        return 1
    fi
}

if [ -z "$SQL_CLIENT" ]; then
    echo "  SKIP  SQL connectivity tests (no mysql or dolt CLI found)"
    echo "        Install mysql client or dolt CLI to run connectivity tests."
elif ! nc -z "$HOST" "$PORT" 2>/dev/null; then
    echo "  SKIP  SQL connectivity tests (port-forward not active on $HOST:$PORT)"
    echo "        Run 'make port-forward-dolt' in another terminal first."
else
    check "TCP connection to $HOST:$PORT" \
        "nc -z $HOST $PORT"

    check "SQL: SELECT 1" \
        "run_sql 'SELECT 1'"

    check "SQL: CREATE DATABASE" \
        "run_sql 'CREATE DATABASE IF NOT EXISTS gt_validate_test'"

    check "SQL: CREATE TABLE + INSERT" \
        "run_sql 'USE gt_validate_test; CREATE TABLE IF NOT EXISTS test_durability (id INT PRIMARY KEY, val VARCHAR(64)); INSERT INTO test_durability VALUES (1, \"stage1-check\")'"

    check "SQL: SELECT back" \
        "run_sql 'SELECT * FROM gt_validate_test.test_durability WHERE id=1'"

    check "SQL: DROP test database" \
        "run_sql 'DROP DATABASE IF EXISTS gt_validate_test'"
fi

echo ""

# --- Section 4: PVC durability ---
echo "--- PVC Durability ---"

PVC_SIZE=$(kubectl get pvc dolt-data-dolt-0 -n "$NAMESPACE" -o jsonpath='{.spec.resources.requests.storage}' 2>/dev/null || echo "unknown")
PVC_SC=$(kubectl get pvc dolt-data-dolt-0 -n "$NAMESPACE" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || echo "default")
PVC_POLICY=$(kubectl get pv "$(kubectl get pvc dolt-data-dolt-0 -n "$NAMESPACE" -o jsonpath='{.spec.volumeName}' 2>/dev/null)" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}' 2>/dev/null || echo "unknown")

echo "  INFO  PVC size: $PVC_SIZE"
echo "  INFO  Storage class: ${PVC_SC:-default}"
echo "  INFO  Reclaim policy: $PVC_POLICY"

if [ "$PVC_POLICY" = "Delete" ]; then
    echo "  WARN  Reclaim policy is 'Delete' — PV will be destroyed if PVC is deleted."
    echo "        Consider patching to 'Retain' for production use."
fi

echo ""

# --- Summary ---
echo "=== Results ==="
echo "  Total: $TOTAL  Passed: $PASS  Failed: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "  Stage 1 validation FAILED. Fix issues above before proceeding."
    exit 1
else
    echo ""
    echo "  Stage 1 validation PASSED."
    exit 0
fi
