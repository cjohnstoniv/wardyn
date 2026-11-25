#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test that scripts/fetch-gate-tools.sh pins the same tool versions
# the gates run (Makefile, license-headers.sh), refuses a name it does not know,
# and retries a failed fetch twice (a stub go and sleep stand in).
# Usage: scripts/test-fetch-gate-tools.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "test-fetch-gate-tools: FAIL: $*" >&2; exit 1; }

list="$("${REPO_ROOT}/scripts/fetch-gate-tools.sh" --list)" || fail "--list failed"
for pin in "honnef.co/go/tools/cmd/staticcheck@$(grep -oP '^STATICCHECK_VERSION\s*\?=\s*\K\S+' "${REPO_ROOT}/Makefile")" \
           "github.com/google/go-licenses@$(grep -oP '^GO_LICENSES_VERSION\s*\?=\s*\K\S+' "${REPO_ROOT}/Makefile")" \
           "github.com/google/addlicense@v1.1.1"; do
  grep -qF -- " ${pin}" <<<"${list}" || fail "--list lacks ${pin}: ${list}"
done
[[ "$(wc -l <<<"${list}")" -eq 7 ]] || fail "want 7 tools, got: ${list}"

"${REPO_ROOT}/scripts/fetch-gate-tools.sh" no-such-tool >/dev/null 2>&1
[[ $? -eq 2 ]] || fail "an unknown tool name did not exit 2"

# Each fetch is tried three times, 5 s then 10 s apart (#3469). A stub go whose `go get` fails the first GET_FAILS
# times, and whose `go mod verify` fails when VERIFY_FAIL=1; a stub sleep records each wait instead of waiting.
STUB="$(mktemp -d)"
trap 'rm -rf "${STUB}"' EXIT
cat >"${STUB}/go" <<'EOF'
#!/usr/bin/env bash
case "$1 ${2:-}" in
  "mod init") exit 0 ;;
  "mod verify") [[ "${VERIFY_FAIL:-0}" == 1 ]] && exit 7; exit 0 ;;
esac
if [[ "$1" == get ]]; then
  echo get >>"${STUB_LOG}"
  [[ "$(grep -c '^get$' "${STUB_LOG}")" -le "${GET_FAILS:-0}" ]] && exit 9
fi
exit 0
EOF
printf '#!/usr/bin/env bash\necho "sleep $1" >>"${STUB_LOG}"\n' >"${STUB}/sleep"
chmod +x "${STUB}/go" "${STUB}/sleep"
# fetch VAR=VALUE... runs the script for one tool with the stubs first on PATH; the log is fresh each time.
fetch() { : >"${STUB}/log"; env PATH="${STUB}:${PATH}" STUB_LOG="${STUB}/log" "$@" "${REPO_ROOT}/scripts/fetch-gate-tools.sh" staticcheck >"${STUB}/out" 2>&1; }
gets() { grep -c '^get$' "${STUB}/log"; }

fetch GET_FAILS=2 || fail "two failed fetches and then a good one did not pass: $(cat "${STUB}/out")"
[[ "$(gets)" -eq 3 ]] || fail "want 3 fetches, got $(gets)"
{ grep -qx 'sleep 5' "${STUB}/log" && grep -qx 'sleep 10' "${STUB}/log"; } || fail "the waits were not 5 s then 10 s: $(cat "${STUB}/log")"

fetch GET_FAILS=3 && fail "three failed fetches passed"
[[ "$(gets)" -eq 3 ]] || fail "want 3 fetches before giving up, got $(gets)"
grep -q 'fetch failed 3 times' "${STUB}/out" || fail "giving up did not say so: $(cat "${STUB}/out")"

# Inside the retried subshell set -e does not apply, so each step is chained with &&: a failed verify must fail the try.
fetch VERIFY_FAIL=1 && fail "a failed go mod verify passed"
[[ "$(gets)" -eq 3 ]] || fail "a failed verify was not retried: $(gets) fetches"

echo "test-fetch-gate-tools: PASS"
