#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# rollback-drill.sh — the #1002 operator drill: 0.8 -> 0.7.13 -> 0.7.11 (this
# branch's own version line). Proves, against a real built wardynd binary and
# a real throwaway Postgres, that:
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
# Needs: docker (default daemon), go. Runs entirely against a throwaway
# postgres:17 container this script starts and removes by the exact id it
# captured — never a shared name, never a lane's own database.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

work="$(mktemp -d)"
trap 'cleanup' EXIT

pg_cid=""
wardynd_pid=""

cleanup() {
  if [ -n "$wardynd_pid" ] && kill -0 "$wardynd_pid" 2>/dev/null; then
    kill "$wardynd_pid" 2>/dev/null || true
    wait "$wardynd_pid" 2>/dev/null || true
  fi
  if [ -n "$pg_cid" ]; then
    docker rm -f "$pg_cid" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}

log() { echo "[rollback-drill] $*"; }
fail() { echo "[rollback-drill] FAIL: $*" >&2; exit 1; }

log "starting a throwaway postgres:17"
pg_cid="$(docker run -d -e POSTGRES_PASSWORD=wardyn -e POSTGRES_DB=wardyn -p 127.0.0.1:0:5432 postgres:17)"
pg_port="$(docker port "$pg_cid" 5432/tcp | head -1 | cut -d: -f2)"
dsn="postgres://postgres:wardyn@127.0.0.1:${pg_port}/wardyn?sslmode=disable"

log "waiting for postgres to accept connections"
for _ in $(seq 1 30); do
  docker exec "$pg_cid" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$pg_cid" pg_isready -U postgres >/dev/null 2>&1 || fail "postgres never became ready"

log "building this branch's wardynd (0.7.13 line)"
go build -o "$work/wardynd" ./cmd/wardynd

# A stable age key across every boot in this drill, so restoring the
# pre-upgrade dump is refused (or not) for the reason this drill is testing —
# the migration-recognition refusal — never for an unrelated ephemeral-key
# mismatch against secrets sealed by an earlier boot in the same run.
age_key="$("$work/wardynd" -gen-age-key)"

port=18080
start_wardynd() {
  WARDYN_PG_DSN="$dsn" WARDYN_LISTEN=":$port" WARDYN_RUNNER=none WARDYN_AGE_KEY="$age_key" \
    "$work/wardynd" >"$work/wardynd.log" 2>&1 &
  wardynd_pid=$!
}
wait_healthz() {
  for _ in $(seq 1 20); do
    if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
      return 0
    fi
    kill -0 "$wardynd_pid" 2>/dev/null || return 1 # exited already
    sleep 1
  done
  return 1
}

log "step 1: fresh install migrates to 0.7.13 and boots"
start_wardynd
if ! wait_healthz; then
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

log "step 2: applying real 0.8-only migrations (simulating an 0.8 upgrade)"
for f in 0066_devices_and_federation.sql 0067_user_drives_object_scheme.sql 0068_aws_sso_spent_tokens.sql; do
  docker exec -i "$pg_cid" psql -q -U postgres -d wardyn \
    <"internal/db/testdata/rollback_drill_08_migrations/$f"
  docker exec "$pg_cid" psql -q -U postgres -d wardyn \
    -c "INSERT INTO schema_migrations (filename) VALUES ('$f')"
done
before_rows="$(docker exec "$pg_cid" psql -tA -U postgres -d wardyn -c 'SELECT count(*) FROM schema_migrations')"
log "  schema_migrations has $before_rows rows carrying the simulated 0.8 upgrade"

log "step 3: this branch's wardynd against the 0.8-migrated database must refuse"
start_wardynd
if wait_healthz; then
  kill "$wardynd_pid" 2>/dev/null || true
  fail "wardynd served /healthz against a database an 0.8 wardynd migrated — the downgrade refusal did not fire"
fi
rc=0
wait "$wardynd_pid" 2>/dev/null || rc=$?
wardynd_pid=""
[ "$rc" -ne 0 ] || fail "wardynd exited 0 against an 0.8-migrated database; want a refusal"
grep -q "downgrade is unsupported" "$work/wardynd.log" || { cat "$work/wardynd.log" >&2; fail "refusal message missing"; }
grep -q "0068_aws_sso_spent_tokens.sql" "$work/wardynd.log" || { cat "$work/wardynd.log" >&2; fail "refusal did not name the newest unknown migration"; }
grep -q "WARDYN_ALLOW_UNKNOWN_MIGRATIONS" "$work/wardynd.log" || { cat "$work/wardynd.log" >&2; fail "refusal did not name the break-glass remedy"; }
log "  refused (exit $rc), named the newest unknown migration, named the remedy"

after_rows="$(docker exec "$pg_cid" psql -tA -U postgres -d wardyn -c 'SELECT count(*) FROM schema_migrations')"
[ "$before_rows" = "$after_rows" ] || fail "the refusal changed schema_migrations ($before_rows -> $after_rows rows); it must write nothing"
log "  schema_migrations unchanged ($after_rows rows) — the refusal wrote nothing"

log "step 4: the documented rollback — restore the pre-upgrade dump — and reboot"
docker exec "$pg_cid" psql -q -U postgres -d wardyn -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
docker exec -i "$pg_cid" psql -q -U postgres -d wardyn <"$work/pre-upgrade.sql"

start_wardynd
if ! wait_healthz; then
  cat "$work/wardynd.log" >&2
  fail "wardynd did not serve /healthz after restoring the pre-upgrade dump — the documented rollback did not work"
fi
kill "$wardynd_pid"
wait "$wardynd_pid" 2>/dev/null || true
wardynd_pid=""
log "  boots clean on the restored pre-upgrade dump, /healthz OK"

log "PASS: 0.8 -> 0.7.13 refusal fires against real 0.8 migrations, writes nothing, and the documented restore-the-dump rollback boots clean"
