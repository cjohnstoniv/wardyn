#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# rollback-drill.sh — the #1002 operator drill: 0.8 -> 0.7.13 (this branch's
# own version line). Proves, against a real built wardynd binary and a real
# throwaway Postgres, that:
#
#   1. A fresh install migrates cleanly to what this branch ships (0.7.13),
#      and wardynd boots and serves /healthz on it.
#   2. Applying real 0.8-only migrations (the same three fixtures
#      internal/db/migrate_rollback_drill_pg_test.go uses: devices/federation,
#      user_drives.object_scheme, aws_sso_spent_tokens — copied verbatim from
#      main's internal/db/migrations) models an operator who upgraded to 0.8.
#   3. Pointing THIS branch's wardynd back at that database refuses to boot —
#      it never opens its listener — names the newest migration it does not
#      ship, and leaves schema_migrations untouched.
#   4. docs/OPERATIONS.md's only supported recovery — restore the pre-upgrade
#      dump — actually works: restoring the dump taken in step 1 and pointing
#      the same binary at it boots clean again.
#
# Usage: scripts/rollback-drill.sh
# Needs: docker (default daemon), go, python3 (to pick a free loopback port).
# Runs entirely against a throwaway postgres:17 container this script starts
# and removes by the exact id it captured — never a shared name, never a
# lane's own database.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

work="$(mktemp -d)"
trap 'cleanup' EXIT

pg_cid=""
wardynd_pid=""

cleanup() {
  if [ -n "$wardynd_pid" ] && kill -0 "$wardynd_pid" 2>/dev/null; then
    kill -9 "$wardynd_pid" 2>/dev/null || true
    wait "$wardynd_pid" 2>/dev/null || true
  fi
  if [ -n "$pg_cid" ]; then
    docker rm -f "$pg_cid" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}

log() { echo "[rollback-drill] $*"; }
fail() { echo "[rollback-drill] FAIL: $*" >&2; exit 1; }

# psql wrapper: -v ON_ERROR_STOP=1 everywhere, so a failing statement (a typo'd
# fixture, a restore that didn't take) exits non-zero instead of being
# silently swallowed and the drill carrying on over a half-applied database.
pg() { docker exec -i "$pg_cid" psql -v ON_ERROR_STOP=1 -U postgres -d wardyn "$@"; }

log "starting a throwaway postgres:17"
pg_cid="$(docker run -d -e POSTGRES_PASSWORD=wardyn -e POSTGRES_DB=wardyn -p 127.0.0.1:0:5432 postgres:17)"
pg_port="$(docker port "$pg_cid" 5432/tcp | head -1 | cut -d: -f2)"
dsn="postgres://postgres:wardyn@127.0.0.1:${pg_port}/wardyn?sslmode=disable"

log "waiting for postgres to accept connections"
# -h 127.0.0.1: without it, pg_isready checks the container's unix socket,
# which the entrypoint's initdb temp server also answers on — a false-ready
# in the temp server's own stop/restart window, right before the real server
# is listening. -h forces the TCP check, which only the real server binds.
for _ in $(seq 1 30); do
  docker exec "$pg_cid" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$pg_cid" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 || fail "postgres never became ready"

log "building this branch's wardynd (0.7.13 line)"
go build -o "$work/wardynd" ./cmd/wardynd

# A stable age key across every boot in this drill, so restoring the
# pre-upgrade dump is refused (or not) for the reason this drill is testing —
# the migration-recognition refusal — never for an unrelated ephemeral-key
# mismatch against secrets sealed by an earlier boot in the same run.
age_key="$("$work/wardynd" -gen-age-key)"

# A free loopback port, not a fixed one: a fixed port already bound by
# something else on the host would make wait_healthz's curl a false PASS
# (answering for that other process, not wardynd) and make a genuine refusal
# look like a hang instead. There is a race between picking it here and
# wardynd binding it below, same as any "ask the OS, then reuse" allocation;
# acceptable for a one-shot local drill.
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"

start_wardynd() {
  WARDYN_PG_DSN="$dsn" WARDYN_LISTEN="127.0.0.1:$port" WARDYN_RUNNER=none WARDYN_AGE_KEY="$age_key" \
    "$work/wardynd" >"$work/wardynd.log" 2>&1 &
  wardynd_pid=$!
}

# wait_healthz waits up to timeout_s for EITHER wardynd to serve /healthz
# (returns 0) or wardynd's own process to exit (returns 1) — checking the pid
# FIRST each iteration, so a stray process already answering on $port cannot
# be mistaken for our wardynd, and bounding the loop so a wardynd that neither
# serves nor exits (hung) is a timeout, not an infinite wait.
wait_healthz() {
  local timeout_s="$1" waited=0
  while [ "$waited" -lt "$timeout_s" ]; do
    if ! kill -0 "$wardynd_pid" 2>/dev/null; then
      return 1 # exited already — never reached serving
    fi
    if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}

# wait_exited waits up to timeout_s for wardynd's process to exit on its own
# (the refusal path: no listener ever opens, the process just returns an
# error and exits) and captures its exit code. A wardynd that is still alive
# after the timeout is killed and treated as a failure — bounding step 3
# against the hang scripts/rollback-drill.sh's own review flagged: a refusal
# that neither serves nor exits must not block this script forever.
wait_exited() {
  local timeout_s="$1" waited=0
  while [ "$waited" -lt "$timeout_s" ]; do
    if ! kill -0 "$wardynd_pid" 2>/dev/null; then
      wait "$wardynd_pid" 2>/dev/null
      return $?
    fi
    sleep 1
    waited=$((waited + 1))
  done
  kill -9 "$wardynd_pid" 2>/dev/null || true
  wait "$wardynd_pid" 2>/dev/null || true
  fail "wardynd neither served /healthz nor exited within ${timeout_s}s against the 0.8-migrated database"
}

log "step 1: fresh install migrates to 0.7.13 and boots"
start_wardynd
if ! wait_healthz 20; then
  cat "$work/wardynd.log" >&2
  fail "wardynd did not serve /healthz on a fresh database"
fi
log "  boots clean, /healthz OK"

log "step 1b: pre-upgrade dump (docs/OPERATIONS.md's rollback recipe)"
docker exec "$pg_cid" pg_dump -U postgres wardyn >"$work/pre-upgrade.sql"
[ -s "$work/pre-upgrade.sql" ] || fail "pre-upgrade dump is empty"

kill "$wardynd_pid"
wait "$wardynd_pid" 2>/dev/null || true
wardynd_pid=""

# Step 2 models what a REAL 0.8 upgrade leaves behind, not just three extra
# filenames on top of an otherwise-untouched 0.7.13 schema: main's own
# secret-envelope migration is 0069_secret_envelope_v1.sql, byte-identical to
# this branch's 0065_secret_envelope_v1.sql (same envelope, renumbered — see
# a84f21681's commit message). A genuine 0.8-migrated database therefore does
# NOT carry a "0065_secret_envelope_v1.sql" row at all — it carries "0069_…"
# instead — which leaves this branch's OWN 0065 file looking unapplied to
# this branch's Migrate(). That is exactly the condition that makes "the
# refusal wrote nothing" a real assertion: a write-before-refuse bug would
# apply that 0065 file (idempotent DDL, so it would not fail) and record it,
# changing schema_migrations's row count, which step 3 below checks for.
log "step 2: applying real 0.8-only migrations (simulating an 0.8 upgrade)"
pg -q -c "UPDATE schema_migrations SET filename = '0069_secret_envelope_v1.sql' WHERE filename = '0065_secret_envelope_v1.sql'"
for f in 0066_devices_and_federation.sql 0067_user_drives_object_scheme.sql 0068_aws_sso_spent_tokens.sql; do
  pg -q <"internal/db/testdata/rollback_drill_08_migrations/$f"
  pg -q -c "INSERT INTO schema_migrations (filename) VALUES ('$f')"
done
before_rows="$(pg -tA -c 'SELECT count(*) FROM schema_migrations')"
log "  schema_migrations has $before_rows rows carrying the simulated 0.8 upgrade (0065_secret_envelope_v1.sql renamed to 0069_, so it reads as pending to this branch)"

log "step 3: this branch's wardynd against the 0.8-migrated database must refuse"
start_wardynd
if wait_healthz 20; then
  kill "$wardynd_pid" 2>/dev/null || true
  fail "wardynd served /healthz against a database an 0.8 wardynd migrated — the downgrade refusal did not fire"
fi
rc=0
wait_exited 20 || rc=$?
wardynd_pid=""
[ "$rc" -ne 0 ] || fail "wardynd exited 0 against an 0.8-migrated database; want a refusal"
grep -q "downgrade is unsupported" "$work/wardynd.log" || { cat "$work/wardynd.log" >&2; fail "refusal message missing"; }
grep -q "0069_secret_envelope_v1.sql" "$work/wardynd.log" || { cat "$work/wardynd.log" >&2; fail "refusal did not name the newest unknown migration"; }
grep -q "WARDYN_ALLOW_UNKNOWN_MIGRATIONS" "$work/wardynd.log" || { cat "$work/wardynd.log" >&2; fail "refusal did not name the break-glass remedy"; }
log "  refused (exit $rc), named the newest unknown migration, named the remedy"

after_rows="$(pg -tA -c 'SELECT count(*) FROM schema_migrations')"
[ "$before_rows" = "$after_rows" ] || fail "the refusal changed schema_migrations ($before_rows -> $after_rows rows); it must write nothing"
log "  schema_migrations unchanged ($after_rows rows) — the refusal wrote nothing, including its own 0065_secret_envelope_v1.sql which it saw as pending"

log "step 4: the documented rollback — restore the pre-upgrade dump — and reboot"
pg -q -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
pg -q <"$work/pre-upgrade.sql"

start_wardynd
if ! wait_healthz 20; then
  cat "$work/wardynd.log" >&2
  fail "wardynd did not serve /healthz after restoring the pre-upgrade dump — the documented rollback did not work"
fi
kill "$wardynd_pid"
wait "$wardynd_pid" 2>/dev/null || true
wardynd_pid=""
log "  boots clean on the restored pre-upgrade dump, /healthz OK"

log "PASS: 0.8 -> 0.7.13 refusal fires against a real 0.8 install, writes nothing, and the documented restore-the-dump rollback boots clean"
