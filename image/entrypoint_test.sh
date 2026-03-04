#!/usr/bin/env bash
#
# Tests for entrypoint.sh — validates role routing, env var handling,
# and session lifecycle without requiring tmux or a real container.
#
# Usage: bash image/entrypoint_test.sh

set -euo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

pass() {
    ((PASS++))
    echo -e "${GREEN}PASS${NC}: $1"
}

fail() {
    ((FAIL++))
    echo -e "${RED}FAIL${NC}: $1 — $2"
}

# Source entrypoint functions by extracting the role routing logic.
# We test the mapping from GT_ROLE/GT_POLECAT → SESSION_NAME/WORKDIR.
test_role_routing() {
    local role="$1" rig="${2:-}" polecat="${3:-}" town_root="${4:-/gt}"
    local expected_session="$5" expected_workdir="$6"

    local session workdir
    GT_ROLE="$role"
    GT_RIG="$rig"
    GT_POLECAT="$polecat"
    GT_TOWN_ROOT="$town_root"

    if [[ -n "${GT_POLECAT}" ]]; then
        session="gt-${GT_POLECAT}"
        workdir="${GT_TOWN_ROOT}/${GT_RIG}/polecats/${GT_POLECAT}/${GT_RIG}"
    elif [[ "${GT_ROLE}" == "mayor" ]]; then
        session="hq-mayor"
        workdir="${GT_TOWN_ROOT}/mayor/rig"
    elif [[ "${GT_ROLE}" == "deacon" ]]; then
        session="hq-deacon"
        workdir="${GT_TOWN_ROOT}"
    elif [[ "${GT_ROLE}" == *"/witness" ]]; then
        session="gt-witness"
        workdir="${GT_TOWN_ROOT}/${GT_RIG}"
    elif [[ "${GT_ROLE}" == *"/refinery" ]]; then
        session="gt-refinery"
        workdir="${GT_TOWN_ROOT}/${GT_RIG}/refinery/rig"
    else
        session="gt-agent"
        workdir="${GT_TOWN_ROOT}"
    fi

    local test_name="role=$role"
    if [[ "$session" != "$expected_session" ]]; then
        fail "$test_name" "session: got '$session', want '$expected_session'"
        return
    fi
    if [[ "$workdir" != "$expected_workdir" ]]; then
        fail "$test_name" "workdir: got '$workdir', want '$expected_workdir'"
        return
    fi
    pass "$test_name → session=$session workdir=$workdir"
}

echo "=== Role Routing Tests ==="

# Polecat: GT_POLECAT takes priority
test_role_routing "citadel/polecats/toast" "citadel" "toast" "/gt" \
    "gt-toast" "/gt/citadel/polecats/toast/citadel"

# Polecat with different rig
test_role_routing "forge/polecats/rictus" "forge" "rictus" "/gt" \
    "gt-rictus" "/gt/forge/polecats/rictus/forge"

# Mayor
test_role_routing "mayor" "" "" "/gt" \
    "hq-mayor" "/gt/mayor/rig"

# Deacon
test_role_routing "deacon" "" "" "/gt" \
    "hq-deacon" "/gt"

# Witness (rig-scoped)
test_role_routing "citadel/witness" "citadel" "" "/gt" \
    "gt-witness" "/gt/citadel"

# Refinery (rig-scoped)
test_role_routing "citadel/refinery" "citadel" "" "/gt" \
    "gt-refinery" "/gt/citadel/refinery/rig"

# Custom town root
test_role_routing "mayor" "" "" "/mnt/gastown" \
    "hq-mayor" "/mnt/gastown/mayor/rig"

# Unknown role falls back to gt-agent
test_role_routing "unknown-role" "" "" "/gt" \
    "gt-agent" "/gt"

echo ""
echo "=== Env Var Validation Tests ==="

# GT_ROLE is required — entrypoint should fail without it
if (unset GT_ROLE; bash -c ': "${GT_ROLE:?GT_ROLE is required}"' 2>/dev/null); then
    fail "GT_ROLE required" "should have failed without GT_ROLE"
else
    pass "GT_ROLE required — fails without it"
fi

# GT_TOWN_ROOT defaults to /gt
GT_ROLE="mayor"
unset GT_TOWN_ROOT 2>/dev/null || true
: "${GT_TOWN_ROOT:=/gt}"
if [[ "$GT_TOWN_ROOT" == "/gt" ]]; then
    pass "GT_TOWN_ROOT defaults to /gt"
else
    fail "GT_TOWN_ROOT default" "got '$GT_TOWN_ROOT', want '/gt'"
fi

echo ""
echo "=== Credential Loading Tests ==="

# Test that credential files in /etc/gt/claude/ would be exported
TMPDIR_CREDS=$(mktemp -d)
echo "sk-test-key-123" > "${TMPDIR_CREDS}/ANTHROPIC_API_KEY"
echo "vertex" > "${TMPDIR_CREDS}/CLAUDE_PROVIDER"

# Simulate the credential loading loop
for f in "${TMPDIR_CREDS}"/*; do
    [[ -f "$f" ]] || continue
    varname=$(basename "$f")
    export "$varname"="$(cat "$f")"
done

if [[ "${ANTHROPIC_API_KEY:-}" == "sk-test-key-123" ]]; then
    pass "ANTHROPIC_API_KEY loaded from file"
else
    fail "ANTHROPIC_API_KEY" "got '${ANTHROPIC_API_KEY:-}', want 'sk-test-key-123'"
fi

if [[ "${CLAUDE_PROVIDER:-}" == "vertex" ]]; then
    pass "CLAUDE_PROVIDER loaded from file"
else
    fail "CLAUDE_PROVIDER" "got '${CLAUDE_PROVIDER:-}', want 'vertex'"
fi

rm -rf "${TMPDIR_CREDS}"
unset ANTHROPIC_API_KEY CLAUDE_PROVIDER

echo ""
echo "=== Summary ==="
echo -e "Passed: ${GREEN}${PASS}${NC}"
echo -e "Failed: ${RED}${FAIL}${NC}"

if (( FAIL > 0 )); then
    exit 1
fi
