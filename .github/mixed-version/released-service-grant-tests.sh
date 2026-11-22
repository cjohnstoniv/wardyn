#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# released-service-grant-tests.sh: the released-artifact half of .github/workflows/mixed-version.yml's service
# grant checks. It takes the published v0.10.1 wardynd and wardyn CLI, and the default policy the image ships
# beside them, out of the v0.10.1 wardynd image, pinned by the index digest the v0.10.1 release record lists,
# proves each binary reports 0.10.1, and runs the two internal/api tests that need them against the job's
# Postgres (svc/postgres, port-forwarded). Each test makes its own throwaway database there. Both wardynd
# launches, the probe here and the tests' own, get that policy by absolute path (WARDYN_DEFAULT_POLICY, which
# the tests pass through) and a working directory of their own: the test binary runs from an empty one, so
# nothing resolves against this tree. Gated on the test binary's exit code; a skip of either test, or a test
# that did not run, fails the step.
# Needs NS and RUNNER_TEMP (the job's env), docker, kubectl on the kind cluster, curl, jq, openssl and go.
set -euo pipefail

: "${NS:?}" "${RUNNER_TEMP:?}"
# ghcr.io/cjohnstoniv/wardynd:0.10.1, the multi-platform index the v0.10.1 release published.
IMAGE=ghcr.io/cjohnstoniv/wardynd@sha256:4435df7c2f2aca67f161b085fcb7b4db54bae8ea6dbb0ffadcf15df92354c079
TESTS=(TestMixedVersion_OldServerSaveKeepsServiceGrant TestMixedVersion_ReleasedCLIGetSetKeepsServiceGrant)
BIN="$RUNNER_TEMP/v0101"
POLICY="$BIN/default-policy.json"
LOG="$RUNNER_TEMP/released-service-grant-tests.log"
PG=postgres://wardyn:wardyn@127.0.0.1:15432
mkdir -p "$BIN"
pids=()

cleanup() {
	local rc=$?
	[ "${#pids[@]}" -eq 0 ] || kill "${pids[@]}" 2>/dev/null || :
	if [ "$rc" -ne 0 ]; then
		set +eu
		echo "::group::diagnostics"
		for f in "$RUNNER_TEMP"/pf-postgres.log "$RUNNER_TEMP"/v0101-wardynd.log; do [ -f "$f" ] && { echo "--- $f"; tail -n 80 "$f"; }; done
		echo "::endgroup::"
	fi
	exit "$rc"
}
trap cleanup EXIT
fail() { echo "::error::$*"; exit 1; }
# phase prints when each part starts, so a run that stalls shows where.
# Each phase is also a notice annotation, so a runner that is lost mid-step still shows the last one reached.
phase() { echo "== $(date -u +%H:%M:%SZ) $*"; echo "::notice::mixed-version phase: $*"; }

phase pull the released image
docker pull --platform linux/amd64 "$IMAGE"
cid=$(docker create --platform linux/amd64 "$IMAGE")
docker cp "$cid:/wardynd" "$BIN/wardynd"
docker cp "$cid:/usr/local/bin/wardyn" "$BIN/wardyn"
docker cp "$cid:/examples/policies/default.json" "$POLICY"
docker rm "$cid" >/dev/null
jq -e 'type == "object"' "$POLICY" >/dev/null || fail "the image's /examples/policies/default.json is not a JSON object"
export WARDYN_DEFAULT_POLICY="$POLICY"

v=$("$BIN/wardyn" version)
[ "$v" = 0.10.1 ] || fail "the extracted wardyn CLI reports '$v', want 0.10.1"
echo "ok: wardyn version -> $v"

kubectl -n "$NS" port-forward svc/postgres 15432:5432 >"$RUNNER_TEMP/pf-postgres.log" 2>&1 &
pids+=("$!")
up=0
for _ in $(seq 1 30); do
	if (exec 3<>/dev/tcp/127.0.0.1/15432) 2>/dev/null; then up=1; break; fi
	sleep 1
done
[ "$up" = 1 ] || fail "the port-forward to svc/postgres never accepted a connection"

# wardynd has no version flag; it reports its build on /healthz. Boot it once on a database of its own.
phase create the probe database
timeout 120 kubectl -n "$NS" exec deployment/postgres -- psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -qc 'CREATE DATABASE mv_v0101_probe' ||
	fail "creating the probe database failed or took over 120 s"
phase boot the released wardynd
age=$("$BIN/wardynd" -gen-age-key | grep '^AGE-SECRET-KEY-')
probe_dir=$(mktemp -d "$RUNNER_TEMP/v0101-probe.XXXXXX")
(cd "$probe_dir" && WARDYN_PG_DSN="$PG/mv_v0101_probe?sslmode=disable" WARDYN_LISTEN=127.0.0.1:18090 \
	WARDYN_INTERNAL_LISTEN=127.0.0.1:18091 WARDYN_RUNNER=none WARDYN_AGE_KEY="$age" WARDYN_ADMIN_TOKEN="$(openssl rand -hex 32)" \
	exec "$BIN/wardynd") >"$RUNNER_TEMP/v0101-wardynd.log" 2>&1 &
probe=$!
pids+=("$probe")
v=""
for _ in $(seq 1 60); do
	if v=$(curl -fsS http://127.0.0.1:18090/healthz 2>/dev/null | jq -r .version); then break; fi
	kill -0 "$probe" 2>/dev/null || fail "the extracted wardynd exited before serving /healthz"
	sleep 1
done
[ "$v" = 0.10.1 ] || fail "the extracted wardynd reports '$v' on /healthz, want 0.10.1"
echo "ok: wardynd /healthz version -> $v"
kill "$probe"
for _ in $(seq 1 30); do kill -0 "$probe" 2>/dev/null || break; sleep 1; done
kill -0 "$probe" 2>/dev/null && { kill -9 "$probe"; fail "the probe wardynd did not stop within 30 s of SIGTERM"; }
wait "$probe" 2>/dev/null || [ $? -gt 128 ] || fail "the probe wardynd did not stop on SIGTERM"

names=$(IFS='|'; echo "${TESTS[*]}")
phase build the api test binary
# The compile is the job's memory peak: two workers, a soft heap limit and internal/api without DWARF, as ci.yml's
# prebuild sets them (scripts/ci-api-prebuild.sh has the measured figures).
GOMAXPROCS=2 GOMEMLIMIT=10GiB GOFLAGS="-gcflags=github.com/cjohnstoniv/wardyn/internal/api=-dwarf=false -ldflags=-w" \
	go test -race -c -p 2 -o "$BIN/api.test" ./internal/api/
phase run ${TESTS[*]}
test_dir=$(mktemp -d "$RUNNER_TEMP/v0101-tests.XXXXXX")
(cd "$test_dir" && WARDYN_TEST_PG="$PG/wardyn?sslmode=disable" WARDYN_TEST_V0101_WARDYND="$BIN/wardynd" WARDYN_TEST_V0101_CLI="$BIN/wardyn" \
	exec "$BIN/api.test" -test.count=1 -test.v -test.timeout=15m -test.run "^(${names})\$") 2>&1 | tee "$LOG"
! grep -qE -- '--- SKIP: TestMixedVersion_' "$LOG" || fail "a released-artifact test skipped"
for t in "${TESTS[@]}"; do
	grep -qE -- "^--- PASS: $t " "$LOG" || fail "$t did not run and pass"
done
echo "ok: ${TESTS[*]} ran and passed against the released v0.10.1 artifacts"
