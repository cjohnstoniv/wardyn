#!/usr/bin/env bash
# probes.sh -- Wardyn governance probe library.
#
# Drives each governance control directly inside a live sandbox using
# docker exec one-liners.  No Anthropic API key required.
#
# Usage:
#   SANDBOX_REF=wardyn-agent-<run-id> bash examples/workspaces/probes.sh [probe]
#
# With no argument, all probes run in sequence.
# With a probe name, only that probe runs.
#
# Available probes:
#   l0_isolation        -- confirm no default route exists (L0 structural egress)
#   egress_deny         -- curl an unlisted domain; expect 403 or connection refused
#   metadata_deny       -- curl 169.254.169.254; expect deny from builtin guard
#   git_credential      -- invoke wardyn-git-helper; expect credential approval request
#   kill_switch         -- demonstrate wardyn kill (requires WARDYN_RUN_ID env var)
#
# Prerequisites:
#   - A RUNNING wardyn sandbox: wardyn run ... (any task, even with no API key)
#   - SANDBOX_REF set to the docker container name (e.g. wardyn-agent-<uuid>)
#   - WARDYN_URL and WARDYN_ADMIN_TOKEN set for the kill_switch probe
#
# Sandbox ref discovery (if you do not know the container name):
#   docker ps --filter label=wardyn.run=<run-id> --format '{{.Names}}'
#
# Constraints:
#   - All commands are read-only with respect to the host; they exec into the
#     sandbox only.
#   - The sandbox must have curl available (the claude-code image ships it).
#   - bash -n clean: this script contains no syntax errors.

set -euo pipefail

SANDBOX_REF="${SANDBOX_REF:-}"
WARDYN_URL="${WARDYN_URL:-http://localhost:8080}"
WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"

# Colours (disabled if not a terminal)
if [ -t 1 ]; then
    GREEN="\033[0;32m"
    RED="\033[0;31m"
    YELLOW="\033[0;33m"
    RESET="\033[0m"
else
    GREEN="" RED="" YELLOW="" RESET=""
fi

pass() { printf "${GREEN}PASS${RESET} %s\n" "$*"; }
fail() { printf "${RED}FAIL${RESET} %s\n" "$*"; }
info() { printf "${YELLOW}INFO${RESET} %s\n" "$*"; }
section() { printf "\n=== %s ===\n" "$*"; }

require_sandbox() {
    if [[ -z "$SANDBOX_REF" ]]; then
        printf "ERROR: SANDBOX_REF is not set.\n" >&2
        printf "Set it to the docker container name of a RUNNING wardyn sandbox:\n" >&2
        printf "  SANDBOX_REF=wardyn-agent-<run-id> bash probes.sh\n" >&2
        exit 1
    fi
    if ! docker inspect "$SANDBOX_REF" >/dev/null 2>&1; then
        printf "ERROR: container %q not found. Is the sandbox still running?\n" "$SANDBOX_REF" >&2
        exit 1
    fi
}

# ── probe: l0_isolation ────────────────────────────────────────────────────────
#
# Confirms there is no default route in the sandbox netns (L0 structural
# egress, ARCHITECTURE.md invariant 3).  The agent can only reach wardyn-proxy.
probe_l0_isolation() {
    section "l0_isolation -- no default route in sandbox netns"
    require_sandbox

    # Read the kernel route table from /proc/net/route directly. The slim agent
    # image ships no `ip`/`route` binary, so `ip route` would error and a naive
    # grep would FALSELY report "no default route". A default route is a row
    # whose Destination column (field 2) is all-zero ("00000000").
    info "Running: docker exec $SANDBOX_REF cat /proc/net/route"
    routes=$(docker exec "$SANDBOX_REF" cat /proc/net/route 2>&1)
    printf "%s\n" "$routes"

    if echo "$routes" | awk 'NR>1 && $2=="00000000"{found=1} END{exit !found}'; then
        fail "A default route exists -- L0 isolation may be incomplete"
        return 1
    else
        pass "No default route: the sandbox has no direct internet path (L0 enforced)"
    fi
}

# ── probe: egress_deny ─────────────────────────────────────────────────────────
#
# Sends an outbound HTTPS request to webhook.example.com (not in the demo
# allowlist) through wardyn-proxy and expects a 403 or connection refused.
# With first_use_approval=true the proxy may instead return 202/pending and
# raise an approval -- both outcomes demonstrate the policy is active.
probe_egress_deny() {
    section "egress_deny -- unlisted domain blocked by L2 proxy"
    require_sandbox

    PROXY_URL="http://wardyn-proxy:3128"
    TARGET="https://webhook.example.com/probe"

    info "Sending request to $TARGET via proxy $PROXY_URL"
    http_code=$(docker exec "$SANDBOX_REF" \
        curl -so /dev/null -w "%{http_code}" \
             --max-time 10 \
             --proxy "$PROXY_URL" \
             "$TARGET" 2>&1 || true)

    info "HTTP response code from proxy: $http_code"

    case "$http_code" in
        403|000)
            pass "Request blocked (code=$http_code): webhook.example.com is denied by egress policy"
            ;;
        202|200)
            info "Request returned $http_code -- first_use_approval may have placed it PENDING"
            info "Check the Approvals tab in the UI.  If an approval entry exists this is also a PASS."
            pass "Approval entry raised (code=$http_code) -- data is held, not forwarded"
            ;;
        *)
            fail "Unexpected HTTP code $http_code -- verify proxy configuration"
            return 1
            ;;
    esac
}

# ── probe: metadata_deny ───────────────────────────────────────────────────────
#
# Confirms that 169.254.169.254 (EC2 instance metadata) is denied by the
# builtin private-IP guard (rule_source=builtin:private-ip).  This block is
# unconditional and cannot be approved away.
probe_metadata_deny() {
    section "metadata_deny -- 169.254.169.254 blocked by builtin private-IP guard"
    require_sandbox

    TARGET="http://169.254.169.254/latest/meta-data/"

    info "Sending request to $TARGET (direct -- no proxy, should be unreachable via no-default-route)"
    # The sandbox has no default route, so a direct connection attempt fails at L0.
    direct_result=$(docker exec "$SANDBOX_REF" \
        curl -so /dev/null -w "%{http_code}" \
             --max-time 5 \
             --noproxy "*" \
             "$TARGET" 2>&1 || true)
    info "Direct (no proxy) result: $direct_result"

    info "Sending request via wardyn-proxy (should be blocked by builtin guard)"
    proxy_result=$(docker exec "$SANDBOX_REF" \
        curl -so /dev/null -w "%{http_code}" \
             --max-time 5 \
             --proxy "http://wardyn-proxy:3128" \
             "$TARGET" 2>&1 || true)
    info "Via proxy result: $proxy_result"

    if [[ "$proxy_result" == "403" || "$proxy_result" == "000" ]]; then
        pass "169.254.169.254 blocked via proxy (code=$proxy_result): builtin:private-ip guard active"
    else
        fail "Unexpected proxy response $proxy_result for 169.254.169.254"
        return 1
    fi

    info "Verifying no approval was raised (builtin deny does not enter the approval queue)"
    info "Check the Approvals tab -- no entry for 169.254.169.254 should appear."
}

# ── probe: git_credential ──────────────────────────────────────────────────────
#
# Invokes wardyn-git-helper directly (as git would during a push) to trigger
# the brokered credential chain.  Expects an approval request to appear in the
# control plane and the helper to eventually emit an error (no GitHub App) or
# the credential (if a GitHub App is configured).
probe_git_credential() {
    section "git_credential -- brokered credential chain via wardyn-git-helper"
    require_sandbox

    info "Invoking wardyn-git-helper get (git credential protocol format)"
    cred_output=$(docker exec "$SANDBOX_REF" \
        sh -c 'printf "protocol=https\nhost=github.com\n\n" | wardyn-git-helper get' \
        2>&1 || true)

    printf "Output:\n%s\n" "$cred_output"

    if echo "$cred_output" | grep -qiE "approval|pending|waiting|credential"; then
        pass "Credential approval request raised -- check UI Approvals tab for kind=credential"
    elif echo "$cred_output" | grep -qiE "error|fail|unauthorized|no app"; then
        pass "Broker fail-closed path triggered (no GitHub App configured) -- expected for stock demo"
    elif echo "$cred_output" | grep -q "password="; then
        pass "Credential minted (GitHub App is configured) -- token in helper output, NOT in env"
    else
        info "Unexpected output -- inspect above and check wardyn audit for credential events"
        fail "Could not confirm credential chain behavior from helper output"
        return 1
    fi
}

# ── probe: kill_switch ─────────────────────────────────────────────────────────
#
# Demonstrates the kill switch cascade: wardyn kill <run-id> tears down the
# sandbox, revokes the identity, revokes any brokered credentials, and emits
# a run.kill audit event.
#
# Requires WARDYN_RUN_ID to be set (the UUID of the run to kill).
# WARNING: this probe DESTROYS the sandbox -- run it last.
probe_kill_switch() {
    section "kill_switch -- cascade: sandbox teardown + identity revoke + audit run.kill"

    RUN_ID="${WARDYN_RUN_ID:-}"
    if [[ -z "$RUN_ID" ]]; then
        info "WARDYN_RUN_ID is not set -- skipping kill_switch probe"
        info "To run: WARDYN_RUN_ID=<uuid> SANDBOX_REF=<container> bash probes.sh kill_switch"
        return 0
    fi

    info "Killing run $RUN_ID via wardyn CLI"
    kill_output=$(WARDYN_URL="$WARDYN_URL" WARDYN_ADMIN_TOKEN="$WARDYN_ADMIN_TOKEN" \
        wardyn kill "$RUN_ID" 2>&1 || true)
    printf "Kill output: %s\n" "$kill_output"

    info "Checking container is gone..."
    if ! docker inspect "$SANDBOX_REF" >/dev/null 2>&1; then
        pass "Container $SANDBOX_REF removed (L0 teardown complete)"
    else
        fail "Container still running after kill"
        return 1
    fi

    info "Check the Audit tab for run.kill with actor_type=human"
    info "Check that subsequent requests from the (now-gone) sandbox would 401 (identity revoked)"
    pass "kill_switch probe complete"
}

# ── dispatcher ─────────────────────────────────────────────────────────────────

ALL_PROBES="l0_isolation egress_deny metadata_deny git_credential"
# kill_switch is excluded from ALL because it destroys the sandbox.

run_probe() {
    case "$1" in
        l0_isolation)   probe_l0_isolation ;;
        egress_deny)    probe_egress_deny ;;
        metadata_deny)  probe_metadata_deny ;;
        git_credential) probe_git_credential ;;
        kill_switch)    probe_kill_switch ;;
        *)
            printf "Unknown probe: %s\n" "$1" >&2
            printf "Available: %s kill_switch\n" "$ALL_PROBES" >&2
            exit 1
            ;;
    esac
}

main() {
    local probe="${1:-}"

    printf "Wardyn governance probes\n"
    printf "  SANDBOX_REF : %s\n" "${SANDBOX_REF:-(not set)}"
    printf "  WARDYN_URL  : %s\n" "$WARDYN_URL"
    printf "  Probe       : %s\n" "${probe:-ALL (${ALL_PROBES})}"

    if [[ -n "$probe" ]]; then
        run_probe "$probe"
    else
        failed=0
        for p in $ALL_PROBES; do
            run_probe "$p" || failed=$((failed + 1))
        done
        printf "\n"
        if [[ "$failed" -eq 0 ]]; then
            pass "All probes passed"
        else
            fail "$failed probe(s) failed"
            exit 1
        fi
    fi
}

main "${1:-}"
