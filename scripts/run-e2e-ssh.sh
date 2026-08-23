#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Live SSH gateway e2e (C5): brings up a DEDICATED, uniquely-namespaced compose
# stack (never the operator's/another job's stack — see PROJECT below), enables
# WARDYN_SSH_LISTEN, launches one interactive claude-code sandbox (the built
# image that carries socat + openssh-sftp-server, see docs/SSH.md's "Image
# contract"), and proves against a REAL `ssh`/`sftp` client:
#
#   - exec exit-code propagation (both a nonzero and a zero exit)
#   - an sftp round-trip: put + get + byte-compare
#   - a `-L` forward against an in-sandbox loopback listener (socat, started
#     over a separate exec)
#   - the audit rows for each: ssh.exec / ssh.sftp / ssh.forward
#   - a saved ssh-<session> recording, discovered via the audit trail's
#     session.recording event and fetched via the recordings API
#   - a foreign-key denial: a SECOND principal's MEMBER-role key (registered
#     directly in this stack's own Postgres — the same operator mechanism
#     docs/SSH.md's "Reclaiming a squatted fingerprint" section uses, since
#     there is no second human/OIDC identity to log in as here) against the
#     first principal's run
#   - concurrency: an SFTP transfer while a second session execs, both on the
#     SAME run
#   - F1 admin override: a THIRD principal's key, registered with role='admin'
#     the same direct-Postgres way, reaches the first principal's (member-
#     owned) run — and the resulting ssh.auth success row carries
#     data.override=true (internal/api/sshgateway.go's sshVerifiedAuth)
#
# NOTE (F1.2, phase 2 of the 0.6 SSH-override lane): the admin-override block
# below is AUTHORED and unit-shape-checked (bash -n / shellcheck) in this
# stage only — it has not been exercised against a live stack yet. The live
# run (WARDYN_TEST_DOCKER=1 against a real compose stack) happens in the
# phase-3 integration sweep alongside the rest of this script.
#
# GUARD: Docker-dependent, like the other live lanes. No-op unless
# WARDYN_TEST_DOCKER=1.
#
# Tears the stack down (compose down --volumes, scoped to PROJECT only) on
# every exit path, success or failure — never touches any other stack.
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "run-e2e-ssh: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent SSH e2e (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
source "${ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

command -v docker >/dev/null 2>&1 || die "docker not found"
command -v ssh >/dev/null 2>&1 || die "ssh client not found"
command -v sftp >/dev/null 2>&1 || die "sftp client not found"
command -v jq >/dev/null 2>&1 || die "jq not found"

# ── dedicated stack identity — NEVER the operator's/another job's stack ──────
PROJECT="wardynv05e2e"
COMPOSE_FILE="${ROOT}/deploy/compose/docker-compose.yaml"
API_PORT=18080
PG_PORT=15432
SSH_PORT=12222
# The registry sidecar publishes a host port too, and nothing overrode it: on
# a shared daemon this stack collided with the operator's own `wardyn-registry`
# on the 5010 default and `compose up` died with "port is already allocated"
# before wardynd ever started (observed live). Namespaced like the other three
# — the one gap docs/ENV.md's WARDYN_REGISTRY_PORT row explicitly warns about.
REGISTRY_PORT=15010

# Project-unique agent image tag/repo (NOT the shared wardyn/agent-*:local
# convention every other e2e script trusts): on a box where wardyn_pick_docker_host
# routes to a native dockerd shared with concurrent agents/jobs, that shared,
# mutable tag can be re-pointed by someone else's build between "docker image
# inspect" and the run actually launching (observed live while developing this
# script: the shared tag briefly resolved to a pre-SSH-gateway image with
# neither socat nor sftp-server, failing every sftp/-L check with a clean but
# confusing "executable file not found in $PATH"). A repository name nobody
# else has any reason to write to removes the collision entirely. wardynd and
# wardyn-proxy now get the same treatment: reusing the shared :local tag meant
# this lane graded whatever another job last built (observed live: 12 checks
# passed against a pre-0043 binary, then the override assertions died on a
# missing `role` column). docker-compose.yaml takes WARDYN_WARDYND_IMAGE /
# WARDYN_PROXY_IMAGE overrides; both default to the :local names, so no other
# caller changes.
AGENT_IMAGE="wardynv05e2e/agent-claude-code:pinned"
WARDYND_IMAGE="wardyn/wardynd:${PROJECT}"
PROXY_IMAGE="wardyn/wardyn-proxy:${PROJECT}"

compose() {
  COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT="${API_PORT}" WARDYN_PG_PORT="${PG_PORT}" WARDYN_SSH_PORT="${SSH_PORT}" \
    WARDYN_REGISTRY_PORT="${REGISTRY_PORT}" \
    WARDYN_WARDYND_IMAGE="${WARDYND_IMAGE}" WARDYN_PROXY_IMAGE="${PROXY_IMAGE}" \
    WARDYN_SSH_LISTEN=":2222" WARDYN_SSH_ADVERTISE="127.0.0.1:${SSH_PORT}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg img "${AGENT_IMAGE}" '{"claude-code":$img}')" \
    docker compose -p "${PROJECT}" -f "${COMPOSE_FILE}" "$@"
}

BASE="http://127.0.0.1:${API_PORT}"
ADMIN_TOKEN="demo-admin-token" # compose's own default (WARDYN_LOCAL_MODE left off)
TMPDIR="$(mktemp -d /tmp/wardyn-e2e-ssh.XXXXXX)"
FAILED=0
note() { printf '  %s %s\n' "$1" "$2"; }
pass() { note "[pass]" "$1"; }
fail() { note "[FAIL]" "$1"; FAILED=1; }

teardown() {
  [[ -n "${FWD_PID:-}" ]] && kill "${FWD_PID}" >/dev/null 2>&1 || true
  # Kill the run FIRST (best-effort) so wardynd stops+removes the per-run
  # agent+proxy sidecar containers itself, THEN tear down the compose stack.
  # `compose down` alone only knows about compose-managed resources (postgres,
  # wardynd) -- the docker driver creates wardyn-agent-<id>/wardyn-proxy-<id>
  # dynamically, outside compose's bookkeeping, and killing wardynd out from
  # under a still-running sandbox orphans them (observed live: two prior runs
  # of this script both left their sandbox + proxy containers running after a
  # clean pass/fail exit, until this fix).
  if [[ -n "${RUN_ID:-}" ]]; then
    curl -sS -X POST "${BASE}/api/v1/runs/${RUN_ID}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
    sleep 3
  fi
  log "tearing down ${PROJECT} (compose down --volumes; this project only)"
  compose down --volumes >/dev/null 2>&1 || true
  rm -rf "${TMPDIR}"
}
trap teardown EXIT

# Clean any stragglers from a prior aborted run of THIS script (same pattern
# as scripts/test-concurrent.sh) — never touches a differently-named project.
compose down --volumes >/dev/null 2>&1 || true

# ── build what we need ────────────────────────────────────────────────────────
# wardynd/wardyn-proxy: always built, to this project's OWN tags — never the
# shared, mutable wardyn/*:local, which on a shared daemon holds whatever
# another job last built. The layer cache makes the rebuild near-free.
compose build wardynd >/dev/null || die "build ${WARDYND_IMAGE} failed"
compose --profile build-only build proxy-image >/dev/null || die "build ${PROXY_IMAGE} failed"
# agent-claude-code: build straight to AGENT_IMAGE's project-unique repo (see
# its definition above) instead of make agent-images-core's shared :local tag.
docker image inspect "${AGENT_IMAGE}" >/dev/null 2>&1 || \
  docker build -f deploy/images/claude-code/Dockerfile -t "${AGENT_IMAGE}" "${ROOT}"

# ── bring up the dedicated stack ─────────────────────────────────────────────
log "bringing up ${PROJECT} (api :${API_PORT}, pg :${PG_PORT}, ssh :${SSH_PORT})"
compose up -d postgres wardynd || die "compose up failed for ${PROJECT}"
wait_healthy "${BASE}" 60 2 || { compose logs wardynd | tail -80; die "wardynd did not become healthy"; }
# Retry a few times (own curl, own connection each attempt) rather than trusting
# wait_healthy's last successful probe to still hold for the very next request.
SSH_ENABLED="false"
HEALTHZ_RAW=""
for _ in $(seq 1 10); do
  HEALTHZ_RAW="$(curl -sS "${BASE}/healthz" 2>&1)"
  SSH_ENABLED="$(printf '%s' "${HEALTHZ_RAW}" | jq -r '.ssh.enabled // false' 2>/dev/null || echo false)"
  [[ "${SSH_ENABLED}" == "true" ]] && break
  sleep 1
done
if [[ "${SSH_ENABLED}" != "true" ]]; then
  compose logs wardynd | tail -80
  die "ssh gateway not enabled per /healthz (WARDYN_SSH_LISTEN wiring broken, OR ${WARDYND_IMAGE} was built from a tree without it) -- last /healthz body: ${HEALTHZ_RAW}"
fi
pass "stack up, healthy, ssh gateway enabled (${BASE}, /healthz reports ssh.enabled=true)"

# ── api helper ────────────────────────────────────────────────────────────────
# api METHOD PATH [JSON-BODY] -- writes the response body to $TMPDIR/resp.json,
# prints the HTTP status code on stdout.
api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -sS -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}

# ── 1. register the e2e principal's SSH key via the API ─────────────────────
ssh-keygen -t ed25519 -N "" -q -f "${TMPDIR}/owner_key" -C "wardyn-e2e-owner"
OWNER_PUB="$(cat "${TMPDIR}/owner_key.pub")"
status=$(api POST /api/v1/me/ssh-keys "$(jq -n --arg pk "${OWNER_PUB}" '{name:"e2e-owner",public_key:$pk}')")
if [[ "${status}" == "201" ]]; then
  pass "registered the e2e principal's SSH key via POST /api/v1/me/ssh-keys (201)"
else
  fail "register ssh key: status ${status}: $(cat "${TMPDIR}/resp.json")"
fi

# ── 2. launch one interactive claude-code sandbox (idle: agent-run never
#    execs, so no model credential is needed — see docs/SSH.md + the
#    interactive-repl task's own task.yaml for this exact, already-proven
#    convention) ────────────────────────────────────────────────────────────
RUN_BODY='{"agent":"claude-code","repo":"local:e2e","confinement_class":"CC1","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny",
  "min_confinement_class":"CC1","auto_stop_after_sec":-1}}'
status=$(api POST /api/v1/runs "${RUN_BODY}")
if [[ "${status}" != "201" && "${status}" != "200" ]]; then
  cat "${TMPDIR}/resp.json"
  die "create run: status ${status}"
fi
RUN_ID="$(jq -r '.id' "${TMPDIR}/resp.json")"
[[ -n "${RUN_ID}" && "${RUN_ID}" != "null" ]] || die "create run: no id in response: $(cat "${TMPDIR}/resp.json")"
log "run ${RUN_ID} created, waiting for RUNNING"

state=""
for _ in $(seq 1 60); do
  status=$(api GET "/api/v1/runs/${RUN_ID}")
  [[ "${status}" == "200" ]] || { sleep 2; continue; }
  state="$(jq -r '.state' "${TMPDIR}/resp.json")"
  [[ "${state}" == "RUNNING" ]] && break
  [[ "${state}" =~ ^(FAILED|KILLED|STOPPED)$ ]] && { cat "${TMPDIR}/resp.json"; die "run ${RUN_ID} reached terminal state ${state} instead of RUNNING"; }
  sleep 2
done
[[ "${state}" == "RUNNING" ]] || die "run ${RUN_ID} did not reach RUNNING in time (last state=${state})"
pass "run ${RUN_ID} is RUNNING"

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8)
ssh_run() { ssh "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -p "${SSH_PORT}" "${RUN_ID}@127.0.0.1" "$@"; }

# ── 3. exec exit-code propagation ────────────────────────────────────────────
if out=$(ssh_run "echo wardyn-exec-ok"); [[ $? -eq 0 && "${out}" == *"wardyn-exec-ok"* ]]; then
  pass "exec: zero-exit command propagates exit 0 and stdout"
else
  fail "exec zero-exit: rc=$? out=${out}"
fi
ssh_run "exit 37"
rc=$?
if [[ "${rc}" -eq 37 ]]; then
  pass "exec: nonzero exit code (37) propagated exactly"
else
  fail "exec exit-code propagation: got rc=${rc}, want 37"
fi

# ── 4. sftp round-trip: put + get + byte-compare ─────────────────────────────
head -c 65536 /dev/urandom > "${TMPDIR}/sftp_src.bin"
sftp -b - "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -P "${SSH_PORT}" "${RUN_ID}@127.0.0.1" >"${TMPDIR}/sftp.log" 2>&1 <<SFTP
put ${TMPDIR}/sftp_src.bin /home/agent/e2e-sftp.bin
get /home/agent/e2e-sftp.bin ${TMPDIR}/sftp_dst.bin
SFTP
if [[ $? -eq 0 ]] && cmp -s "${TMPDIR}/sftp_src.bin" "${TMPDIR}/sftp_dst.bin"; then
  pass "sftp round-trip: put + get, byte-identical (64 KiB)"
else
  fail "sftp round-trip: $(cat "${TMPDIR}/sftp.log")"
fi

# ── 5. -L forward against an in-sandbox loopback listener ───────────────────
# Start an in-sandbox echo listener (socat, the SAME binary the gateway itself
# execs for a real -L forward) detached from this exec's own stdout/stderr so
# the launching `sh -c` returns immediately instead of blocking on a still-open
# pipe (see internal/api/sshgateway_channels.go's ExecStream contract).
ssh_run "nohup socat TCP-LISTEN:9191,reuseaddr,fork EXEC:'/bin/cat' >/tmp/e2e-socat.log 2>&1 </dev/null & sleep 1; echo listener-started"
FWD_LOCAL_PORT=19191
ssh -N -L "${FWD_LOCAL_PORT}:127.0.0.1:9191" "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -p "${SSH_PORT}" "${RUN_ID}@127.0.0.1" &
FWD_PID=$!
fwd_up=0
for _ in $(seq 1 15); do
  (exec 9<>"/dev/tcp/127.0.0.1/${FWD_LOCAL_PORT}") 2>/dev/null && { fwd_up=1; exec 9>&- 9<&-; break; }
  sleep 1
done
if [[ "${fwd_up}" -eq 1 ]]; then
  exec 9<>"/dev/tcp/127.0.0.1/${FWD_LOCAL_PORT}"
  printf 'wardyn-forward-probe\n' >&9
  reply=""
  read -r -t 5 reply <&9 || true
  exec 9>&- 9<&-
  # Closing fd 9 above ends ONLY this direct-tcpip channel -- the ssh -N -L
  # session itself stays up (it can serve another forwarded connection), so
  # handleSSHConn's per-connection context is untouched and the server-side
  # goroutine finishes its own recordAudit write cleanly. Give that a moment
  # BEFORE killing the whole session below: killing the session tears down
  # handleSSHConn's connCtx, and if that happens while the (already-ending)
  # channel goroutine is still mid-cleanup, the two race and the audit write
  # can lose (observed live: a same-instant kill silently dropped the
  # ssh.forward row even though the forward itself demonstrably worked).
  sleep 2
  if [[ "${reply}" == "wardyn-forward-probe" ]]; then
    pass "-L forward: byte round-trip through the sandbox's own loopback (socat echo)"
  else
    fail "-L forward: probe echoed back '${reply}', want 'wardyn-forward-probe'"
  fi
else
  fail "-L forward: local forwarded port never became reachable"
fi
kill "${FWD_PID}" >/dev/null 2>&1 || true
wait "${FWD_PID}" 2>/dev/null || true
FWD_PID=""

# ── 6. a saved ssh-<session> recording, via the audit trail + recordings API ─
# -tt forces a pty-req (real Cols/Rows) even though stdin is piped, not a real
# terminal -- bridgeSSHShell's Attach needs a real size for tmux to render into.
# The leading sleep gives the pty/tmux attach time to finish establishing
# before the first keystroke arrives; the trailing `exit` ends the session
# deliberately rather than relying on stdin EOF against a persistent tmux
# session (which does not necessarily close the channel — see docs/SSH.md).
(sleep 2; printf 'echo wardyn-recording-marker\n'; sleep 2; printf 'exit\n') \
  | timeout 15 ssh -tt "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -p "${SSH_PORT}" "${RUN_ID}@127.0.0.1" >"${TMPDIR}/shell.log" 2>&1 || true
REC_KEY=""
for _ in $(seq 1 10); do
  status=$(api GET "/api/v1/audit?run_id=${RUN_ID}")
  REC_KEY="$(jq -r '[.[] | select(.action=="session.recording")][0].data.key // empty' "${TMPDIR}/resp.json" 2>/dev/null)"
  [[ -n "${REC_KEY}" ]] && break
  sleep 1
done
if [[ -n "${REC_KEY}" ]]; then
  status=$(api GET "/api/v1/runs/${RUN_ID}/recording/${REC_KEY}")
  if [[ "${status}" == "200" ]] && grep -q "wardyn-recording-marker" "${TMPDIR}/resp.json"; then
    pass "ssh-<session> recording saved (key=${REC_KEY}) and fetchable via the recordings API, contains the session's own output"
  else
    fail "recording fetch: status ${status} for key ${REC_KEY}"
  fi
else
  fail "no session.recording audit event appeared for the ssh shell session (log: $(cat "${TMPDIR}/shell.log"))"
fi

# ── 7. audit rows for ssh.exec / ssh.sftp / ssh.forward ─────────────────────
for action in ssh.exec ssh.sftp ssh.forward; do
  count=0
  for _ in $(seq 1 5); do
    status=$(api GET "/api/v1/audit?run_id=${RUN_ID}")
    count="$(jq --arg a "${action}" '[.[] | select(.action==$a and .outcome=="success")] | length' "${TMPDIR}/resp.json")"
    [[ "${count}" -ge 1 ]] && break
    sleep 1
  done
  if [[ "${count}" -ge 1 ]]; then
    pass "audit: ${count} successful ${action} row(s)"
  else
    fail "audit: no successful ${action} row found"
  fi
done
status=$(api GET "/api/v1/audit?run_id=${RUN_ID}")
exec37_count="$(jq '[.[] | select(.action=="ssh.exec" and .outcome=="success" and (.data.exit==37))] | length' "${TMPDIR}/resp.json")"
if [[ "${exec37_count}" -ge 1 ]]; then
  pass "audit: ssh.exec row records the exact exit code (37) exec propagated above"
else
  fail "audit: no ssh.exec row recorded exit=37"
fi

# ── 8. concurrency: an SFTP transfer while a second session execs ───────────
head -c 33554432 /dev/urandom > "${TMPDIR}/concurrent_src.bin" # 32 MiB, so the transfer overlaps the exec
(
  sftp -b - "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -P "${SSH_PORT}" "${RUN_ID}@127.0.0.1" >"${TMPDIR}/concurrent_sftp.log" 2>&1 <<SFTP
put ${TMPDIR}/concurrent_src.bin /home/agent/e2e-concurrent.bin
SFTP
  echo $? > "${TMPDIR}/concurrent_sftp.rc"
) &
SFTP_BG_PID=$!
concurrent_out=$(ssh_run "echo wardyn-concurrent-exec-ok")
concurrent_rc=$?
wait "${SFTP_BG_PID}"
sftp_rc="$(cat "${TMPDIR}/concurrent_sftp.rc" 2>/dev/null || echo 1)"
if [[ "${concurrent_rc}" -eq 0 && "${concurrent_out}" == *"wardyn-concurrent-exec-ok"* && "${sftp_rc}" -eq 0 ]]; then
  pass "concurrency: exec completed correctly WHILE a 32 MiB sftp put was in flight on the same run"
else
  fail "concurrency: exec_rc=${concurrent_rc} out=${concurrent_out} sftp_rc=${sftp_rc} ($(cat "${TMPDIR}/concurrent_sftp.log" 2>/dev/null))"
fi

# ── 9. foreign-key denial: a second principal's MEMBER key on someone else's
#    run (F1: owner-OR-admin authorization, and this key is neither) ────────
# No second human/OIDC identity exists in this stack to log in as, so the
# second principal's key is registered the same way an operator would per
# docs/SSH.md's "Reclaiming a squatted fingerprint" — directly in this
# project's OWN Postgres, with the 0043 role column left at its DEFAULT
# ('member' — see the migration's own doc on why 'admin' is never a safe
# default). run.CreatedBy is the admin-bearer principal ("admin-token",
# internal/api/runs_policy.go's adminTokenPrincipal) that created RUN_ID
# above; this key is registered under a DIFFERENT, non-admin principal, so
# sshAuth's owner-OR-admin check (run.CreatedBy == key.Principal, OR
# key.Role == oidc.RoleAdmin) must refuse it on BOTH arms.
ssh-keygen -t ed25519 -N "" -q -f "${TMPDIR}/foreign_key" -C "wardyn-e2e-foreign"
FOREIGN_FP="$(ssh-keygen -lf "${TMPDIR}/foreign_key.pub" | awk '{print $2}')"
FOREIGN_PUB="$(cat "${TMPDIR}/foreign_key.pub")"
# Direct single-quoted interpolation (not psql's -v/:'var' substitution, which
# needs `docker compose exec`'s own arg parsing to leave ":'name'" untouched
# and did not) -- safe here because both values are ssh-keygen's OWN output:
# a "SHA256:<base64>" fingerprint and an "ssh-ed25519 <base64> <comment>"
# authorized_keys line, neither of which can ever contain a single quote.
INSERT_SQL="INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, created_at) VALUES ('${FOREIGN_FP}', 'e2e-foreign-principal', 'foreign-e2e-key', '${FOREIGN_PUB}', now());"
compose exec -T postgres psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -c "${INSERT_SQL}" \
  >"${TMPDIR}/foreign_insert.log" 2>&1
if [[ $? -ne 0 ]]; then
  fail "foreign-key setup: could not insert the second principal's key: $(cat "${TMPDIR}/foreign_insert.log")"
else
  denial_out=$(ssh "${SSH_OPTS[@]}" -i "${TMPDIR}/foreign_key" -p "${SSH_PORT}" "${RUN_ID}@127.0.0.1" "echo should-never-run" 2>&1)
  denial_rc=$?
  status=$(api GET "/api/v1/audit?run_id=${RUN_ID}")
  denied_row="$(jq --arg fp "${FOREIGN_FP}" \
    '[.[] | select(.action=="ssh.auth" and .outcome=="failure" and .target==$fp and .data.reason=="not the run owner")] | length' \
    "${TMPDIR}/resp.json")"
  if [[ "${denial_rc}" -ne 0 && "${denial_out}" != *"should-never-run"* && "${denied_row}" -ge 1 ]]; then
    pass "member-key denial: non-owner, non-admin key refused (rc=${denial_rc}) and audited ssh.auth failure reason=\"not the run owner\""
  else
    fail "member-key denial: rc=${denial_rc} out=${denial_out} audited_denial_rows=${denied_row}"
  fi
fi

# ── 10. F1 admin override: an ADMIN-role key reaches a run it does not own,
#    audited with data.override=true ──────────────────────────────────────
# THIRD principal, registered the same direct-Postgres way as step 9's foreign
# key, but this time with role='admin' explicitly (the column 0043 adds —
# internal/db/migrations/0043_ssh_key_role.sql). RUN_ID is owned by
# "admin-token" (see step 9's comment), a DIFFERENT principal than this key's
# — so success here can only come from sshAuth's admin arm
# (run.CreatedBy != rec.Principal but rec.Role == oidc.RoleAdmin,
# internal/api/sshgateway.go), never the owner arm.
ssh-keygen -t ed25519 -N "" -q -f "${TMPDIR}/admin_key" -C "wardyn-e2e-admin"
ADMIN_KEY_FP="$(ssh-keygen -lf "${TMPDIR}/admin_key.pub" | awk '{print $2}')"
ADMIN_KEY_PUB="$(cat "${TMPDIR}/admin_key.pub")"
# Same single-quoted-interpolation safety note as step 9's INSERT_SQL: both
# values are ssh-keygen's own output and cannot contain a single quote.
ADMIN_INSERT_SQL="INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, role, created_at) VALUES ('${ADMIN_KEY_FP}', 'e2e-admin-principal', 'admin-e2e-key', '${ADMIN_KEY_PUB}', 'admin', now());"
compose exec -T postgres psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -c "${ADMIN_INSERT_SQL}" \
  >"${TMPDIR}/admin_insert.log" 2>&1
if [[ $? -ne 0 ]]; then
  fail "admin-override setup: could not insert the admin-role key: $(cat "${TMPDIR}/admin_insert.log")"
else
  override_out=$(ssh "${SSH_OPTS[@]}" -i "${TMPDIR}/admin_key" -p "${SSH_PORT}" "${RUN_ID}@127.0.0.1" "echo wardyn-override-ok" 2>&1)
  override_rc=$?
  status=$(api GET "/api/v1/audit?run_id=${RUN_ID}")
  override_row="$(jq --arg fp "${ADMIN_KEY_FP}" \
    '[.[] | select(.action=="ssh.auth" and .outcome=="success" and .target==$fp and .data.override==true)] | length' \
    "${TMPDIR}/resp.json")"
  if [[ "${override_rc}" -eq 0 && "${override_out}" == *"wardyn-override-ok"* && "${override_row}" -ge 1 ]]; then
    pass "admin override: admin-role key reached a run it does not own, and ssh.auth success is audited with data.override=true"
  else
    fail "admin override: rc=${override_rc} out=${override_out} audited_override_rows=${override_row}"
  fi
fi

# ── summary ───────────────────────────────────────────────────────────────────
if [[ "${FAILED}" -eq 0 ]]; then
  log "SSH E2E: PASS (all checks above passed)"
else
  printf '\033[1;31m[error]\033[0m %s\n' "SSH E2E: FAIL (see [FAIL] lines above)" >&2
fi
exit "${FAILED}"
