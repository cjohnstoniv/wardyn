#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test that a failed image build in the e2e scripts shows docker's own
# error, and that the job's Go module cache reaches the build only when asked.
#
# THE regression this pins: run-e2e-live.sh ran `docker build -q ... >/dev/null ||
# die`, so the nightly's `go mod download` fault inside the proxy image build left
# only "build proxy sidecar image failed". A fake `docker` on PATH fails every
# build with a known line; the real run-e2e-live.sh must print it and exit non-zero.
#
# Usage: scripts/test-image-build-logs.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
E2E_LIVE_SH="${E2E_LIVE_SH:-${REPO_ROOT}/scripts/run-e2e-live.sh}"
fail() { echo "test-image-build-logs: FAIL: $*" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
mkdir "${TMP}/bin" "${TMP}/seed"
ERR_LINE="go: example.com/m@v1.0.0: Get proxy.golang.org: dial tcp: i/o timeout"

# A docker that records its argv and fails (or, FAKE_DOCKER_OK=1, passes) a build.
# Like the real one, `-q` hides the failed step's output (the go error) and leaves
# only the terse summary; without it the step's output is printed.
cat >"${TMP}/bin/docker" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"${TMP}/docker.args"
if [[ "\${1:-}" == build ]]; then
  [[ "\${FAKE_DOCKER_OK:-}" == 1 ]] && exit 0
  [[ " \$* " == *" -q "* ]] || echo "${ERR_LINE}"
  echo "ERROR: failed to solve: process go mod download did not complete successfully" >&2
  exit 1
fi
exit 0
STUB
printf '#!/usr/bin/env bash\nexit 0\n' >"${TMP}/bin/go"
chmod +x "${TMP}/bin/docker" "${TMP}/bin/go"

# 1. The real script's oracle-image build fails: docker's error is on the output
#    and the script exits non-zero.
out="$(PATH="${TMP}/bin:${PATH}" TMPDIR="${TMP}" WARDYN_TEST_DOCKER=1 bash "${E2E_LIVE_SH}" 2>&1)"
rc=$?
[[ ${rc} -ne 0 ]] || fail "run-e2e-live.sh exited 0 although the image build failed"
grep -qF -- "${ERR_LINE}" <<<"${out}" \
  || fail "run-e2e-live.sh dropped the build's error line (output: ${out})"

! grep -qE '^build .* -q( |$)|^build -q ' "${TMP}/docker.args" \
  || fail "run-e2e-live.sh builds with -q, which hides the failed step's output"

# 2. The helper, on the stdin form run-e2e-byoi.sh uses, shows the same line.
# shellcheck source=scripts/lib/image-build.sh
source "${REPO_ROOT}/scripts/lib/image-build.sh"
export PATH="${TMP}/bin:${PATH}"
out="$(wardyn_docker_build -t x - 2>&1 <<<"FROM scratch")"
rc=$?
[[ ${rc} -ne 0 ]] || fail "wardyn_docker_build returned 0 for a failed build"
grep -qF -- "${ERR_LINE}" <<<"${out}" || fail "wardyn_docker_build dropped the error line"

# 3. A clean build prints nothing.
out="$(FAKE_DOCKER_OK=1 wardyn_docker_build -t x . 2>&1)" || fail "a clean build returned non-zero"
[[ -z "${out}" ]] || fail "a clean build printed output: ${out}"

# 4. --build-context gomodcache= is passed only when the variable names a directory.
build_args() { : >"${TMP}/docker.args"; FAKE_DOCKER_OK=1 wardyn_go_image_build -t x . ; cat "${TMP}/docker.args"; }
unset WARDYN_BUILD_GOMODCACHE
grep -q 'gomodcache' <<<"$(build_args)" && fail "build-context passed with the variable unset"
export WARDYN_BUILD_GOMODCACHE="${TMP}/missing"
grep -q 'gomodcache' <<<"$(build_args)" && fail "build-context passed for a directory that does not exist"
export WARDYN_BUILD_GOMODCACHE="${TMP}/seed"
grep -qF -- "--build-context gomodcache=${TMP}/seed" <<<"$(build_args)" \
  || fail "build-context not passed for an existing directory"

# 5. No script still discards a build's output.
for f in run-e2e-live run-e2e-byoi daemon-proxy-secret-kind; do
  ! grep -nE '^[[:space:]]*docker build' "${REPO_ROOT}/scripts/${f}.sh" \
    || fail "scripts/${f}.sh still calls docker build directly"
done

echo "test-image-build-logs: PASS"
