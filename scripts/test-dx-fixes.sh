#!/usr/bin/env bash
# Counterfactual regression checks for the dx-slice fixes in this directory
# (WSL/Windows detection ordering in setup.sh, the confinement-tier readout in
# run-host.sh, and the --json run_id extraction in ci-run.sh). Plain asserts,
# no framework, no fixtures — extracts the REAL logic out of the sibling
# scripts (via sed/grep anchors) so this fails if any of it regresses. Run
# directly: ./scripts/test-dx-fixes.sh
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
check() { [ "$2" -eq "$3" ] 2>/dev/null && echo "ok   - $1" || { echo "FAIL - $1 (exit ${2:-?}, want $3)"; fail=1; }; }
check_eq() { [ "$2" = "$3" ] && echo "ok   - $1" || { echo "FAIL - $1 (got '$2', want '$3')"; fail=1; }; }

# ── setup.sh: WSL must be detected BEFORE the native-Windows guard, so a WSL2
# shell that inherits $OS=Windows_NT (WSLENV/interop) is never falsely
# rejected. Extracted separately as `detect` (the real /proc/version +
# WSL_DISTRO_NAME check) and `guard_only` (the exit-1 logic, which trusts
# whatever $IS_WSL is already set) so the guard can be exercised in isolation
# with a forced IS_WSL, AND the real detection can be exercised end to end.
detect="$(sed -n '/^IS_WSL=false$/,/WSL_DISTRO_NAME/p' "$ROOT/scripts/setup.sh")"
guard_only="$(sed -n '/^if ! \$IS_WSL; then$/,/^fi$/p' "$ROOT/scripts/setup.sh")"
[ -n "$detect" ] && [ -n "$guard_only" ] || { echo "FAIL - could not extract the WSL/Windows guard from scripts/setup.sh (markers moved?)"; exit 1; }

bash -c "IS_WSL=false; OS=Windows_NT; ${guard_only}" >/dev/null 2>&1
check "non-WSL + OS=Windows_NT still rejects"                        "$?" 1
bash -c "IS_WSL=false; unset OS; uname() { echo MINGW64_NT-10.0; }; ${guard_only}" >/dev/null 2>&1
check "non-WSL + MINGW uname still rejects"                          "$?" 1
# The actual regression: run the REAL detection on THIS box (a genuine WSL2
# shell) with $OS forced to Windows_NT, then the guard — must NOT reject.
bash -c "OS=Windows_NT; ${detect}; ${guard_only}" >/dev/null 2>&1
check "real WSL2 box + OS=Windows_NT proceeds (the bug this fixes)"  "$?" 0

# ── run-host.sh: confinement-tier readout is pure string logic over docker's
# Runtimes JSON — exercise the live classification lines directly.
tier_lines="$(sed -n '/^_classes="CC1 (runc, always)"$/,/CC3 (kata)/p' "$ROOT/scripts/run-host.sh")"
[ -n "$tier_lines" ] || { echo "FAIL - could not extract the tier readout from scripts/run-host.sh (markers moved?)"; exit 1; }

_runtimes='{}'; eval "$tier_lines"
check_eq "tier readout: CC1 only when no extra runtimes"    "$_classes" "CC1 (runc, always)"
_runtimes='{"runc":{},"runsc":{}}'; eval "$tier_lines"
check_eq "tier readout: CC2 when runsc is registered"       "$_classes" "CC1 (runc, always), CC2 (gVisor/runsc)"
_runtimes='{"runc":{},"runsc":{},"kata":{}}'; eval "$tier_lines"
check_eq "tier readout: CC3 when kata is registered"        "$_classes" "CC1 (runc, always), CC2 (gVisor/runsc), CC3 (kata)"

# ── ci-run.sh: run_id must parse from --json output (jq happy path, POSIX-sed
# fallback if jq is absent) regardless of key order/spacing.
run_id_line="$(grep -F 'run_id="$(jq' "$ROOT/scripts/ci-run.sh")"
[ -n "$run_id_line" ] || { echo "FAIL - could not find the run_id extraction line in scripts/ci-run.sh"; exit 1; }
tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT
printf '{"state":"COMPLETED","id": "abcd1234-ef56-7890-abcd-ef1234567890"}' > "$tmp"
run_json="$tmp"; eval "$run_id_line"
check_eq "ci-run.sh run_id parses from --json output"       "$run_id" "abcd1234-ef56-7890-abcd-ef1234567890"

# ── e2e-backend.sh: BASE_URL must be a valid URL for EVERY documented
# WARDYN_E2E_ADDR shape (':PORT', 'host:PORT', '0.0.0.0:PORT'), not just ':PORT'.
# The old ${ADDR#*:} produced 'http://localhost9000' (missing ':') for
# non-':PORT' shapes. Extract the real derivation line and drive it.
base_url_line="$(grep -F 'BASE_URL="http://localhost:' "$ROOT/scripts/e2e-backend.sh")"
[ -n "$base_url_line" ] || { echo "FAIL - could not find the BASE_URL derivation in scripts/e2e-backend.sh"; exit 1; }
ADDR=":8088";       eval "$base_url_line"; check_eq "BASE_URL from ':8088'"       "$BASE_URL" "http://localhost:8088"
ADDR="0.0.0.0:9000"; eval "$base_url_line"; check_eq "BASE_URL from '0.0.0.0:9000'" "$BASE_URL" "http://localhost:9000"
ADDR="host:80";     eval "$base_url_line"; check_eq "BASE_URL from 'host:80'"     "$BASE_URL" "http://localhost:80"

exit "$fail"
