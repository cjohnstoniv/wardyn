#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# released-service-grant-tests.sh: the released-artifact half of .github/workflows/mixed-version.yml's service
# grant checks. It takes the published v0.10.1 wardynd and wardyn CLI out of the v0.10.1 wardynd image, pinned
# by the index digest the v0.10.1 release record lists, proves each reports 0.10.1, and runs the two internal/api
# tests that need them against the job's Postgres (svc/postgres, port-forwarded). Each test makes its own
# throwaway database there. Gated on go test's exit code; a skip of either test, or a test that did not run,
# fails the step.
# Needs NS and RUNNER_TEMP (the job's env), docker, kubectl on the kind cluster, curl, jq, openssl and go.
set -euo pipefail

: "${NS:?}" "${RUNNER_TEMP:?}"
# ghcr.io/cjohnstoniv/wardynd:0.10.1, the multi-platform index the v0.10.1 release published.
IMAGE=ghcr.io/cjohnstoniv/wardynd@sha256:4435df7c2f2aca67f161b085fcb7b4db54bae8ea6dbb0ffadcf15df92354c079
TESTS=(TestMixedVersion_OldServerSaveKeepsServiceGrant TestMixedVersion_ReleasedCLIGetSetKeepsServiceGrant)
BIN="$RUNNER_TEMP/v0101"
LOG="$RUNNER_TEMP/released-service-grant-tests.log"
PG=postgres://wardyn:wardyn@127.0.0.1:15432
mkdir -p "$BIN"
pids=()

cleanup() {
	local rc=$?
	[ "${#pids[@]}" -eq 0 ] || kill "${pids[@]}" 2>/dev/null
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

docker pull --platform linux/amd64 "$IMAGE"
cid=$(docker create --platform linux/amd64 "$IMAGE")
docker cp "$cid:/wardynd" "$BIN/wardynd"
docker cp "$cid:/usr/local/bin/wardyn" "$BIN/wardyn"
docker rm "$cid" >/dev/null

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
kubectl -n "$NS" exec deployment/postgres -- psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -qc 'CREATE DATABASE mv_v0101_probe'
age=$("$BIN/wardynd" -gen-age-key | grep '^AGE-SECRET-KEY-')
WARDYN_PG_DSN="$PG/mv_v0101_probe?sslmode=disable" WARDYN_LISTEN=127.0.0.1:18090 WARDYN_INTERNAL_LISTEN=127.0.0.1:18091 \
	WARDYN_RUNNER=none WARDYN_AGE_KEY="$age" WARDYN_ADMIN_TOKEN="$(openssl rand -hex 32)" \
	"$BIN/wardynd" >"$RUNNER_TEMP/v0101-wardynd.log" 2>&1 &
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
wait "$probe" 2>/dev/null || [ $? -gt 128 ] || fail "the probe wardynd did not stop on SIGTERM"

names=$(IFS='|'; echo "${TESTS[*]}")
WARDYN_TEST_PG="$PG/wardyn?sslmode=disable" WARDYN_TEST_V0101_WARDYND="$BIN/wardynd" WARDYN_TEST_V0101_CLI="$BIN/wardyn" \
	go test -count=1 -v -timeout 15m -run "^(${names})\$" ./internal/api/ 2>&1 | tee "$LOG"
! grep -qE -- '--- SKIP: TestMixedVersion_' "$LOG" || fail "a released-artifact test skipped"
for t in "${TESTS[@]}"; do
	grep -qE -- "^--- PASS: $t " "$LOG" || fail "$t did not run and pass"
done
echo "ok: ${TESTS[*]} ran and passed against the released v0.10.1 artifacts"
