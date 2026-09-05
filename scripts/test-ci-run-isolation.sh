#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# R5 F020: ci-run.sh's per-job isolation, proven daemon-free with a PATH-stubbed
# docker. Two claims, both of which had already drifted from docs/CI.md's
# "one job's `down --volumes` never tears down another's":
#
#   1. The default project name is namespace-independent. `wardyn-ci-$$` is the
#      PID in THIS shell's PID namespace — two containerized CI jobs on one
#      shared host docker socket can both be PID 42, land on the same project
#      name, and the second job's unconditional `down --volumes` destroys the
#      first's live stack.
#   2. The script REFUSES when that project already has running containers,
#      instead of tearing them down. This is what makes a collision (from any
#      cause, including a hand-pinned WARDYN_CI_PROJECT) non-destructive.
#   3. (R5 F199) The admin bearer never reaches the host `docker` process argv.
#      The shim passed `-e WARDYN_ADMIN_TOKEN=<value>` on every CLI call, so a
#      real fleet token sat in /proc/<pid>/cmdline and in `ps` output for every
#      other user on the runner — redundantly, since the wardynd container the
#      same script started already carries it from the compose interpolation.
#
# Usage: scripts/test-ci-run-isolation.sh   (exit 0 = PASS)
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ── 1. the default project name is not PID-derived ───────────────────────────
default_line="$(grep -n '^CI_PROJECT=' scripts/ci-run.sh || true)"
[ -n "$default_line" ] || bad "scripts/ci-run.sh: no CI_PROJECT= assignment found"
case "$default_line" in
    *'$$'*) bad "scripts/ci-run.sh: CI_PROJECT default is PID-derived ($default_line) — \$\$ repeats across PID namespaces, so two containerized jobs on one host share a project name and one job's 'down --volumes' destroys the other's stack. Draw the suffix from /dev/urandom." ;;
    *) ok "CI_PROJECT default is not PID-derived" ;;
esac

# ── 2. a live stack under this project name is refused, not torn down ────────
STUB="$(mktemp -d)"
trap 'rm -rf "$STUB"' EXIT
CALLS="$STUB/calls.log"

# docker stub: records every call; `docker ps` answers with whatever
# $STUB/running holds (a container id = the project is live, empty = free).
cat > "$STUB/docker" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CALLS"
case "$1" in
  ps) cat "$STUB/running" 2>/dev/null || true ;;
  context) exit 1 ;;
esac
exit 0
STUBEOF
chmod +x "$STUB/docker"

# run_ciphase PROJECT -> stdout+stderr of ci-run.sh, with docker stubbed.
run_ci() {
    env PATH="$STUB:$PATH" STUB="$STUB" CALLS="$CALLS" \
        WARDYN_CI_PROJECT="wardyn-ci-selftest" \
        WARDYN_CI_TASK="echo hi" \
        WARDYN_CI_SKIP_BUILD=1 \
        DOCKER_HOST="unix:///nonexistent.sock" \
        ./scripts/ci-run.sh 2>&1
}

# 2a. project already live -> must refuse, naming the project.
: > "$CALLS"
echo "deadbeefcafe" > "$STUB/running"
out_live="$(run_ci)"; rc_live=$?
if [ "$rc_live" -eq 0 ]; then
    bad "ci-run.sh exited 0 with a live stack under its own project name"
fi
case "$out_live" in
    *"wardyn-ci-selftest"*already*) ok "refuses a project name that already has running containers" ;;
    *) bad "ci-run.sh did not refuse a live 'wardyn-ci-selftest' stack — it would 'down --volumes' another job's containers. Got: $(printf '%s' "$out_live" | tail -3 | tr '\n' ' ')" ;;
esac
# The refusal must come from an actual query, not a guess.
if ! grep -q 'com.docker.compose.project=wardyn-ci-selftest' "$CALLS"; then
    bad "ci-run.sh never asked docker whether project 'wardyn-ci-selftest' was in use"
else
    ok "queries docker for containers already in the project"
fi
# ...and it must refuse BEFORE any teardown of that stack.
if grep -q 'down --volumes' "$CALLS"; then
    bad "ci-run.sh ran 'down --volumes' against a project that was already live"
else
    ok "no teardown issued against the live project"
fi

# 2b. project free -> must NOT refuse (a guard that always refuses is no guard).
: > "$CALLS"
: > "$STUB/running"
out_free="$(run_ci)"
case "$out_free" in
    *"wardyn-ci-selftest"*already*) bad "ci-run.sh refused an UNUSED project name" ;;
    *) ok "an unused project name proceeds" ;;
esac

# ── 3. the admin bearer is not on the host docker argv (F199) ────────────────
# Behavioural: the REAL shim is extracted from the live script and called with a
# `docker` on PATH that dumps its own argv. `ps`/`/proc/<pid>/cmdline` is world-
# readable on a shared runner, and this fires on every single CLI call the job
# makes — while docker-compose.yaml already sets WARDYN_ADMIN_TOKEN inside
# wardynd from the same exported variable, so the container has it either way.
shim="$(sed -n '/^wardyn() {/,/^}/p' scripts/ci-run.sh)"
[ -n "$shim" ] || bad "scripts/ci-run.sh has no wardyn() shim to extract"
ARGV="$STUB/argv.log"
cat > "$STUB/argv-docker" <<'ARGVEOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$ARGV"
exit 0
ARGVEOF
chmod +x "$STUB/argv-docker"
(
  set +u
  eval "$shim"
  COMPOSE=("$STUB/argv-docker" compose -p wardyn-ci-selftest -f /dev/null)
  WARDYN_ADMIN_TOKEN='s3cr3t-real-fleet-admin-token'
  ARGV="$ARGV" wardyn runs list
) >/dev/null 2>&1 || true
if grep -qF 's3cr3t-real-fleet-admin-token' "$ARGV" 2>/dev/null; then
    bad "scripts/ci-run.sh's wardyn() puts the admin bearer on the host docker argv — world-readable in ps/proc on a shared runner, on every CLI call: $(tr '\n' ' ' < "$ARGV")"
else
    ok "the admin bearer never reaches the host docker argv"
fi
# The counterweight: a shim that stopped invoking the CLI at all would also pass.
if grep -qxF '/usr/local/bin/wardyn' "$ARGV" 2>/dev/null && grep -qxF 'runs' "$ARGV" 2>/dev/null; then
    ok "the shim still execs the in-container CLI with its arguments"
else
    bad "the extracted wardyn() no longer invokes /usr/local/bin/wardyn with its arguments — the case above would pass vacuously. Got: $(tr '\n' ' ' < "$ARGV" 2>/dev/null)"
fi
# ...and the container must still be TOLD which URL to talk to.
if grep -qxF 'WARDYN_URL=http://localhost:8080' "$ARGV" 2>/dev/null; then
    ok "the shim still passes WARDYN_URL (not a credential)"
else
    bad "the shim no longer passes WARDYN_URL — dropping the credential must not drop the endpoint"
fi

[ "$fail" -eq 0 ] && echo "PASS: scripts/test-ci-run-isolation.sh" || echo "FAIL: scripts/test-ci-run-isolation.sh" >&2
exit "$fail"
