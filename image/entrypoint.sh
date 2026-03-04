#!/usr/bin/env bash
#
# Universal agent entrypoint for Gas Town Kubernetes pods.
#
# Expected env vars:
#   GT_ROLE       - Agent role (e.g. citadel/polecats/toast, mayor, citadel/witness)
#   GT_RIG        - Rig name (e.g. citadel)
#   GT_POLECAT    - Polecat name (e.g. toast) — polecats only
#   GT_BRANCH     - Git branch (e.g. polecat/toast)
#   GT_TOWN_ROOT  - Town root directory (mounted PVC, e.g. /gt)
#   GT_DOLT_HOST  - Dolt service hostname (e.g. dolt-svc)
#   GT_DOLT_PORT  - Dolt service port (default 3307)
#
# The entrypoint:
#   1. Validates required env vars
#   2. Loads Claude credentials from mounted secret (if available)
#   3. Sets up the working directory (worktree for polecats)
#   4. Starts a tmux session
#   5. Launches the agent (claude) inside tmux
#   6. Waits for the tmux session to exit

set -euo pipefail

: "${GT_ROLE:?GT_ROLE is required}"
: "${GT_TOWN_ROOT:=/gt}"

# Load Claude credentials from mounted secret (if available).
# The secret is mounted as files at /etc/gt/claude/ by the pod spec.
CLAUDE_CREDS_DIR="/etc/gt/claude"
if [[ -d "${CLAUDE_CREDS_DIR}" ]]; then
    for f in "${CLAUDE_CREDS_DIR}"/*; do
        [[ -f "$f" ]] || continue
        varname=$(basename "$f")
        export "$varname"="$(cat "$f")"
    done
fi

# Determine session name and working directory based on role
if [[ -n "${GT_POLECAT:-}" ]]; then
    SESSION_NAME="gt-${GT_POLECAT}"
    WORKDIR="${GT_TOWN_ROOT}/${GT_RIG}/polecats/${GT_POLECAT}/${GT_RIG}"
elif [[ "${GT_ROLE}" == "mayor" ]]; then
    SESSION_NAME="hq-mayor"
    WORKDIR="${GT_TOWN_ROOT}/mayor/rig"
elif [[ "${GT_ROLE}" == "deacon" ]]; then
    SESSION_NAME="hq-deacon"
    WORKDIR="${GT_TOWN_ROOT}"
elif [[ "${GT_ROLE}" == *"/witness" ]]; then
    SESSION_NAME="gt-witness"
    WORKDIR="${GT_TOWN_ROOT}/${GT_RIG}"
elif [[ "${GT_ROLE}" == *"/refinery" ]]; then
    SESSION_NAME="gt-refinery"
    WORKDIR="${GT_TOWN_ROOT}/${GT_RIG}/refinery/rig"
else
    SESSION_NAME="gt-agent"
    WORKDIR="${GT_TOWN_ROOT}"
fi

echo "[entrypoint] Role: ${GT_ROLE}"
echo "[entrypoint] Session: ${SESSION_NAME}"
echo "[entrypoint] Workdir: ${WORKDIR}"

# Wait for workdir to be available (PVC mount)
WAIT_SECONDS=0
while [[ ! -d "${WORKDIR}" ]] && (( WAIT_SECONDS < 30 )); do
    echo "[entrypoint] Waiting for ${WORKDIR}..."
    sleep 1
    (( WAIT_SECONDS++ ))
done

if [[ ! -d "${WORKDIR}" ]]; then
    echo "[entrypoint] ERROR: ${WORKDIR} not available after 30s"
    exit 1
fi

# Ensure the nudge queue directory exists on the shared PVC
mkdir -p "${GT_TOWN_ROOT}/.runtime/nudge-queue"

# Use the real tmux binary for session management in the entrypoint.
# The shim at /usr/local/bin/tmux is for agent/operator cross-pod routing;
# here we're creating local sessions, so we call the real binary directly.
TMUX_BIN="${GT_REAL_TMUX:-/usr/bin/tmux}"

# Start tmux session with the agent
"${TMUX_BIN}" new-session -d -s "${SESSION_NAME}" -c "${WORKDIR}"

# Send the agent startup command into the tmux session.
# gt prime loads full context for the role and starts the agent loop.
"${TMUX_BIN}" send-keys -t "${SESSION_NAME}" "gt prime --hook" C-m

echo "[entrypoint] Agent started in tmux session ${SESSION_NAME}"

# Keep the container alive as long as the tmux session exists.
# When the agent exits, the container exits, and k8s handles restart.
while "${TMUX_BIN}" has-session -t "${SESSION_NAME}" 2>/dev/null; do
    sleep 5
done

echo "[entrypoint] tmux session ${SESSION_NAME} ended, container exiting"
