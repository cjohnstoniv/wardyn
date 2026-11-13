#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# rollback-drill.sh — the operator drill behind docs/OPERATIONS.md#upgrades: a
# release older than this tree cannot be redeployed over a database this tree
# migrated, and the supported way back is restoring the pre-upgrade backup.
# Proves, with two real built wardynd binaries (this checkout's and the older
# release's) against a real throwaway Postgres, that:
#
#   1. The OLD release boots on a fresh database and serves /healthz; a dump of
#      that database is the "pre-upgrade backup".
#   2. This checkout's wardynd boots over the same database, migrating it
#      forward through every migration the old release does not ship.
#   3. The OLD release pointed back at that database refuses to boot: it never
#      opens its listener, names the newest migration it does not ship, says
#      how many it does not ship, names the break-glass, and leaves
#      schema_migrations untouched.
#   4. Restoring the dump from step 1 and pointing the OLD release at it boots
#      clean again: the documented rollback works.
#
# Usage: scripts/rollback-drill.sh
# Env:   WARDYN_DRILL_OLD_REF  git ref of the older release (default v0.9.9,
#        the last release before the 0.10 line); it must be fetchable here
#        (CI: fetch tags first).
# Needs: docker (default daemon), go, git, python3, curl.
# Runs entirely against a throwaway postgres:17 container this script starts
# and removes by the exact id it captured: never a shared name, never a
# lane's own database. The older release is exported with `git archive` into
# the scratch directory; no worktree or ref in this repository changes.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

old_ref="${WARDYN_DRILL_OLD_REF:-v0.9.9}"
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

# psql wrapper: -v ON_ERROR_STOP=1 everywhere, so a failing statement (a
# restore that didn't take) exits non-zero instead of being swallowed and the
# drill carrying on over a half-applied database.
pg() { docker exec -i "$pg_cid" psql -v ON_ERROR_STOP=1 -U postgres -d wardyn "$@"; }

git rev-parse --verify -q "${old_ref}^{commit}" >/dev/null ||
  fail "git ref $old_ref is not available here (set WARDYN_DRILL_OLD_REF, or fetch tags)"

log "starting a throwaway postgres:17"
pg_cid="$(docker run -d -e POSTGRES_PASSWORD=wardyn -e POSTGRES_DB=wardyn -p 127.0.0.1:0:5432 postgres:17)"
pg_port="$(docker port "$pg_cid" 5432/tcp | head -1 | cut -d: -f2)"
dsn="postgres://postgres:wardyn@127.0.0.1:${pg_port}/wardyn?sslmode=disable"

log "waiting for postgres to accept connections"
# -h 127.0.0.1: without it, pg_isready checks the container's unix socket,
# which the entrypoint's initdb temp server also answers on, a false-ready in
# the temp server's stop/restart window. -h forces the TCP check, which only
# the real server binds.
for _ in $(seq 1 30); do
  docker exec "$pg_cid" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$pg_cid" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 || fail "postgres never became ready"

log "building this checkout's wardynd and $old_ref's"
go build -o "$work/wardynd-new" ./cmd/wardynd
mkdir "$work/old-src"
git archive "$old_ref" | tar -x -C "$work/old-src"
(cd "$work/old-src" && go build -o "$work/wardynd-old" ./cmd/wardynd)

# Migration names each tree ships, in byte order (the order schema_migrations
# is compared in), so the drill checks the refusal against what the two trees
# really differ by instead of a hard-coded number.
LC_ALL=C ls "$here/internal/db/migrations" | grep '\.sql$' | LC_ALL=C sort >"$work/new-migrations"
LC_ALL=C ls "$work/old-src/internal/db/migrations" | grep '\.sql$' | LC_ALL=C sort >"$work/old-migrations"
unknown_to_old="$(LC_ALL=C comm -13 "$work/old-migrations" "$work/new-migrations")"
[ -n "$unknown_to_old" ] || fail "this checkout ships no migration $old_ref lacks; there is nothing to drill"
want_count="$(echo "$unknown_to_old" | wc -l | tr -d ' ')"
want_newest="$(echo "$unknown_to_old" | tail -1)"
log "  this checkout ships $want_count migration(s) $old_ref lacks, newest $want_newest"

# A stable age key and admin token across every boot in this drill, so the
# restore is refused (or not) for the reason this drill tests, never for an
# unrelated key mismatch against secrets sealed by an earlier boot.
age_key="$("$work/wardynd-new" -gen-age-key | grep -E '^AGE-SECRET-KEY-' | head -1)"
[ -n "$age_key" ] || fail "wardynd -gen-age-key produced no key"
admin_token="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"

# Free loopback ports, not fixed ones: a port already bound by something else
# on the host would make wait_healthz's curl a false PASS (answering for that
# other process) and make a genuine refusal look like a hang. A picked port is
# free until wardynd binds it, so another process can take it first: wardynd
# then exits on its bind error, and wait_healthz starts it again on new ports.
# free_port [TAKEN...] never prints one of TAKEN.
free_port() {
  python3 -c 'import socket, sys
taken, held = set(sys.argv[1:]), []
while True:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = str(s.getsockname()[1])
    if port not in taken:
        break
    held.append(s)  # kept bound, so the next bind cannot return it
print(port)' "$@"
}
port="$(free_port)"
internal_port="$(free_port "$port")"

# start_wardynd <binary>
start_wardynd() {
  wardynd_bin="$1"
  WARDYN_PG_DSN="$dsn" WARDYN_LISTEN="127.0.0.1:$port" WARDYN_INTERNAL_LISTEN="127.0.0.1:$internal_port" \
    WARDYN_RUNNER=none WARDYN_AGE_KEY="$age_key" WARDYN_ADMIN_TOKEN="$admin_token" \
    "$1" >"$work/wardynd.log" 2>&1 &
  wardynd_pid=$!
}

stop_wardynd() {
  kill "$wardynd_pid" 2>/dev/null || true
  wait "$wardynd_pid" 2>/dev/null || true
  wardynd_pid=""
}

# wait_healthz waits up to timeout_s for EITHER wardynd to serve /healthz
# (returns 0) or wardynd's own process to exit (returns 1), checking the pid
# FIRST each iteration, so a stray process already answering on $port cannot
# be mistaken for our wardynd, and a wardynd that neither serves nor exits
# (hung) is a timeout, not an infinite wait.
# A wardynd that exits on "address already in use" lost a picked port to
# another process before it bound it: it is started again on new ports, at most
# twice. Any other exit (the refusal step 3 expects) still returns 1.
wait_healthz() {
  local timeout_s="$1" waited=0 restarts=0 old
  while [ "$waited" -lt "$timeout_s" ]; do
    if ! kill -0 "$wardynd_pid" 2>/dev/null; then
      if [ "$restarts" -lt 2 ] && grep -q 'address already in use' "$work/wardynd.log"; then
        wait "$wardynd_pid" 2>/dev/null || true
        old="$port $internal_port"
        # shellcheck disable=SC2086 # one word per port
        port="$(free_port $old)"
        # shellcheck disable=SC2086
        internal_port="$(free_port $old "$port")"
        log "  a port was taken before wardynd bound it; starting it again on $port and $internal_port"
        restarts=$((restarts + 1))
        start_wardynd "$wardynd_bin"
        continue
      fi
      return 1
    fi
    # -m 3: a process that took the port and accepts without answering must not hang the drill.
    if curl -fsS -m 3 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}

# wait_exited waits up to timeout_s for wardynd's process to exit on its own
# (the refusal path: no listener ever opens, the process returns an error and
# exits) and returns its exit code. A wardynd still alive after the timeout is
# killed and fails the drill: a refusal that neither serves nor exits must not
# block this script forever.
wait_exited() {
  local timeout_s="$1" waited=0
  while [ "$waited" -lt "$timeout_s" ]; do
    if ! kill -0 "$wardynd_pid" 2>/dev/null; then
      # wait still holds this background job's status after kill -0 sees it gone.
      wait "$wardynd_pid" 2>/dev/null
      return $?
    fi
    sleep 1
    waited=$((waited + 1))
  done
  kill -9 "$wardynd_pid" 2>/dev/null || true
  wait "$wardynd_pid" 2>/dev/null || true
  wardynd_pid=""
  fail "$old_ref wardynd neither served /healthz nor exited within ${timeout_s}s against the migrated database"
}

log "step 1: $old_ref boots on a fresh database"
start_wardynd "$work/wardynd-old"
if ! wait_healthz 30; then
  cat "$work/wardynd.log" >&2
  fail "$old_ref wardynd did not serve /healthz on a fresh database"
fi
log "  boots clean, /healthz OK"

log "step 1b: pre-upgrade dump (docs/OPERATIONS.md's backup recipe)"
docker exec "$pg_cid" pg_dump -U postgres wardyn >"$work/pre-upgrade.sql"
[ -s "$work/pre-upgrade.sql" ] || fail "pre-upgrade dump is empty"
stop_wardynd

log "step 2: this checkout's wardynd migrates the same database forward"
start_wardynd "$work/wardynd-new"
if ! wait_healthz 60; then
  cat "$work/wardynd.log" >&2
  fail "this checkout's wardynd did not serve /healthz over the $old_ref database"
fi
stop_wardynd
applied_newest="$(pg -tA -c 'SELECT filename FROM schema_migrations ORDER BY filename COLLATE "C" DESC LIMIT 1')"
[ "$applied_newest" = "$want_newest" ] || fail "after the upgrade the newest recorded migration is $applied_newest, want $want_newest"
before_rows="$(pg -tA -c 'SELECT count(*) FROM schema_migrations')"
log "  migrated forward, newest $applied_newest, $before_rows rows in schema_migrations"

log "step 3: $old_ref against the migrated database must refuse"
start_wardynd "$work/wardynd-old"
if wait_healthz 20; then
  stop_wardynd
  fail "$old_ref wardynd served /healthz against a database this checkout migrated; the downgrade refusal did not fire"
fi
rc=0
wait_exited 20 || rc=$?
wardynd_pid=""
[ "$rc" -ne 0 ] || fail "$old_ref wardynd exited 0 against a migrated database; want a refusal"
grep -F "records $want_count migration(s) this wardynd does not ship" "$work/wardynd.log" >/dev/null ||
  { cat "$work/wardynd.log" >&2; fail "refusal did not count the $want_count unknown migration(s)"; }
grep -F "\"$want_newest\"" "$work/wardynd.log" >/dev/null ||
  { cat "$work/wardynd.log" >&2; fail "refusal did not name the newest unknown migration $want_newest"; }
grep -F "restore the pre-upgrade dump" "$work/wardynd.log" >/dev/null ||
  { cat "$work/wardynd.log" >&2; fail "refusal did not name the restore"; }
grep -F "WARDYN_ALLOW_UNKNOWN_MIGRATIONS" "$work/wardynd.log" >/dev/null ||
  { cat "$work/wardynd.log" >&2; fail "refusal did not name the break-glass remedy"; }
log "  refused (exit $rc), counted $want_count, named $want_newest and the remedies"

after_rows="$(pg -tA -c 'SELECT count(*) FROM schema_migrations')"
[ "$before_rows" = "$after_rows" ] || fail "the refusal changed schema_migrations ($before_rows -> $after_rows rows); it must write nothing"
log "  schema_migrations unchanged ($after_rows rows): the refusal wrote nothing"

log "step 4: the documented rollback, restore the pre-upgrade dump, then reboot $old_ref"
pg -q -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
pg -q <"$work/pre-upgrade.sql"

start_wardynd "$work/wardynd-old"
if ! wait_healthz 30; then
  cat "$work/wardynd.log" >&2
  fail "$old_ref wardynd did not serve /healthz after restoring the pre-upgrade dump; the documented rollback did not work"
fi
stop_wardynd
log "  boots clean on the restored dump, /healthz OK"

log "PASS: $old_ref refuses a database this checkout migrated, writes nothing, and restoring the pre-upgrade dump boots it clean"
