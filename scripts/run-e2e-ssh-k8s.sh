#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Live SSH gateway e2e on the KUBERNETES substrate (the compose lane's
# counterpart: scripts/run-e2e-ssh.sh). Same gateway, same audit rows, but the
# sandbox on the other side of the channel is a POD in a real cluster, reached
# through Runner.Attach/ExecStream's k8s implementation
# (internal/runner/k8s/session.go) instead of the Docker driver's.
#
# What it proves, against a REAL `ssh` client and the cluster's own API:
#
#   - the run's sandbox is genuinely a Pod in this cluster (kubectl, by the
#     sandbox_ref the API returned) — not a container on some daemon
#   - `ssh <run-id>@<advertise-host> -p <port> id` exits 0 and runs as the
#     image's agent user, over the k8s exec lane
#   - a nonzero exit code survives that lane exactly (k8s carries exec exit
#     status out-of-band, in the SPDY/websocket error stream — a different
#     mechanism from Docker's exec inspect, so it gets its own check)
#   - the interactive shell lane: a pty session that reaches tmux and emits
#     the session.attach/session.detach pair carrying transport:ssh
#   - the audit rows for both: ssh.exec (with the exact exit codes) and
#     session.attach{transport:ssh}
#   - authorization, both arms (the compose lane's counterpart checks): a
#     second principal's MEMBER key is refused on a run it does not own and
#     the refusal is audited, while a third principal's ADMIN-role key reaches
#     that same run with data.override=true on the ssh.auth row
#
# NOT RUN BY CI: no workflow invokes `make test-e2e-ssh-k8s` — it needs a kind
# cluster this script deliberately does not create. It is a MANUAL proof, run
# by hand against `make kind-quickstart`; treat a green result as evidence
# only for the tip someone actually ran it on.
#
# DELIBERATELY NOT PROVEN HERE — sftp and `-L`. Both are an IMAGE contract,
# not a substrate one (docs/SSH.md "Image contract (BYOI)": the gateway execs
# /usr/lib/openssh/sftp-server and socat inside the sandbox). On a cluster the
# image is whatever the operator pointed WARDYN_AGENT_IMAGES at, so a green
# result here would say nothing about the substrate and everything about one
# operator's image. The compose lane proves both against Wardyn's own image,
# and the code path underneath them is the shared Runner.ExecStream this
# script already exercises. The script prints an explicit skip line for each
# rather than silently covering less than its compose sibling.
#
# This script does NOT create or delete the cluster: it runs against the one
# `make kind-quickstart` leaves behind (deploy/kind/quickstart.sh), and cleans
# up only what it created (its run, its key). GUARD: self-skips unless
# WARDYN_TEST_K8S=1, the same knob the other cluster-dependent lanes use.
set -uo pipefail

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  echo "run-e2e-ssh-k8s: set WARDYN_TEST_K8S=1 to run the cluster-dependent SSH e2e (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
source "${ROOT}/scripts/lib/common.sh"

command -v kubectl >/dev/null 2>&1 || die "kubectl not found"
command -v ssh >/dev/null 2>&1 || die "ssh client not found"
command -v jq >/dev/null 2>&1 || die "jq not found"
command -v curl >/dev/null 2>&1 || die "curl not found"

# Exactly what deploy/kind/quickstart.sh installs and prints — this lane
# targets THAT cluster, so these are constants, not knobs (a second install
# would need its own token/port discovery anyway, which is a different script).
CONTEXT="kind-wardyn-quickstart"
NAMESPACE="wardyn"
BASE="http://127.0.0.1:8080"

kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get deployment wardyn >/dev/null 2>&1 || \
  die "no wardyn install in context ${CONTEXT}, namespace ${NAMESPACE} — run 'make kind-quickstart' first (this script never creates a cluster)"

# The install's own admin token, read the way quickstart.sh re-reads it.
ADMIN_TOKEN="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth in ${NAMESPACE}"

TMPDIR="$(mktemp -d /tmp/wardyn-e2e-ssh-k8s.XXXXXX)"
FAILED=0
note() { printf '  %s %s\n' "$1" "$2"; }
pass() { note "[pass]" "$1"; }
fail() { note "[FAIL]" "$1"; FAILED=1; }
skip() { note "[skip]" "$1"; }

teardown() {
  # Kill the run so the cluster reaps its pod; drop the key we registered.
  # The CLUSTER is never touched — it is the caller's, not ours.
  if [[ -n "${RUN_ID:-}" ]]; then
    curl -sS -X POST "${BASE}/api/v1/runs/${RUN_ID}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${OWNER_FP:-}" ]]; then
    # Percent-encode the WHOLE fingerprint, not just the colon: it is base64,
    # so it carries '/' about half the time, and a raw '/' ends the path
    # segment early -- the DELETE 404s and the key we registered outlives the
    # run, in a cluster that outlives this script. (The compose lane can drop
    # its key because `compose down --volumes` takes the whole database with
    # it; here there is nothing to take it away.)
    curl -sS -X DELETE "${BASE}/api/v1/me/ssh-keys/$(printf '%s' "${OWNER_FP}" | jq -sRr @uri)" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  fi
  # The two extra principals' keys went straight into Postgres (see the
  # member-denial step) — they are not the admin token's, so the API cannot
  # delete them. Out the way they came in, and never leave a key behind in a
  # cluster that outlives this script.
  for fp in "${FOREIGN_FP:-}" "${ADMIN_FP:-}"; do
    [[ -n "${fp}" ]] || continue
    psql_exec "DELETE FROM ssh_public_keys WHERE fingerprint = '${fp}';" || true
  done
  rm -rf "${TMPDIR}"
}
trap teardown EXIT

# api METHOD PATH [JSON-BODY] -- body to $TMPDIR/resp.json, status on stdout.
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

# psql_exec SQL -- run one statement in the Postgres quickstart.sh deployed into
# this namespace (same wardyn/wardyn credentials it wrote into the DSN
# Secret). The API cannot create a SECOND principal: POST /me/ssh-keys always
# stamps the CALLER's, and this install has exactly one credential, the admin
# token. The compose lane reaches its database the same way, for the same
# reason (scripts/run-e2e-ssh.sh sections 9-10).
psql_exec() {
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec deploy/postgres -- \
    psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -c "$1" >"${TMPDIR}/psql.log" 2>&1
}

# ── 0. it is the k8s install answering, and the gateway is on ───────────────
# A 200 is not proof of WHO answered: quickstart.sh publishes 127.0.0.1:8080,
# and so does the operator's compose stack. Assert runner=k8s for the same
# reason quickstart.sh does — otherwise this whole script could pass against
# the Docker substrate and claim the cluster.
HEALTHZ="$(curl -fsS "${BASE}/healthz" 2>/dev/null || true)"
[[ -n "${HEALTHZ}" ]] || die "${BASE}/healthz did not answer"
RUNNER="$(printf '%s' "${HEALTHZ}" | jq -r '.runner // empty')"
[[ "${RUNNER}" == "k8s" ]] || die "${BASE}/healthz reports runner=${RUNNER:-<none>}, not k8s — something other than the cluster owns that port (a compose stack?). Body: ${HEALTHZ}"
[[ "$(printf '%s' "${HEALTHZ}" | jq -r '.ssh.enabled // false')" == "true" ]] || \
  die "the k8s install's SSH gateway is off (/healthz .ssh.enabled=false) — the chart needs ssh.enabled=true (deploy/kind/quickstart.sh sets it)"
# The address the daemon itself advertises, not one this script assumes: it is
# what `wardyn ssh` and the console card hand an operator, so it is what must
# actually work.
ADVERTISE="$(printf '%s' "${HEALTHZ}" | jq -r '.ssh.advertise_addr // empty')"
SSH_HOST="${ADVERTISE%:*}"
SSH_PORT="${ADVERTISE##*:}"
[[ -n "${SSH_HOST}" && -n "${SSH_PORT}" ]] || die "unparsable ssh.advertise_addr in /healthz: '${ADVERTISE}'"
pass "k8s install answering on ${BASE} (runner=k8s), ssh gateway enabled, advertising ${ADVERTISE}"

# ── 1. register this run's principal key ────────────────────────────────────
ssh-keygen -t ed25519 -N "" -q -f "${TMPDIR}/owner_key" -C "wardyn-e2e-k8s"
status=$(api POST /api/v1/me/ssh-keys "$(jq -n --arg pk "$(cat "${TMPDIR}/owner_key.pub")" '{name:"e2e-k8s",public_key:$pk}')")
if [[ "${status}" == "201" ]]; then
  OWNER_FP="$(jq -r '.fingerprint' "${TMPDIR}/resp.json")"
  pass "registered the e2e key via POST /api/v1/me/ssh-keys (201, ${OWNER_FP})"
else
  die "register ssh key: status ${status}: $(cat "${TMPDIR}/resp.json")"
fi

# ── 2. one interactive sandbox — a Pod, in this cluster ─────────────────────
# Idle by construction (interactive => no agent task is exec'd), so no model
# credential is involved; same convention as the compose lane.
RUN_BODY='{"agent":"claude-code","repo":"local:e2e","confinement_class":"CC1","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny",
  "min_confinement_class":"CC1","auto_stop_after_sec":-1}}'
status=$(api POST /api/v1/runs "${RUN_BODY}")
[[ "${status}" == "201" || "${status}" == "200" ]] || die "create run: status ${status}: $(cat "${TMPDIR}/resp.json")"
RUN_ID="$(jq -r '.id' "${TMPDIR}/resp.json")"
SANDBOX_REF="$(jq -r '.sandbox_ref // empty' "${TMPDIR}/resp.json")"
[[ -n "${RUN_ID}" && "${RUN_ID}" != "null" ]] || die "create run: no id in response"
log "run ${RUN_ID} created (sandbox_ref ${SANDBOX_REF}), waiting for RUNNING"

state=""
for _ in $(seq 1 60); do
  status=$(api GET "/api/v1/runs/${RUN_ID}")
  [[ "${status}" == "200" ]] || { sleep 2; continue; }
  state="$(jq -r '.state' "${TMPDIR}/resp.json")"
  SANDBOX_REF="$(jq -r '.sandbox_ref // empty' "${TMPDIR}/resp.json")"
  [[ "${state}" == "RUNNING" ]] && break
  [[ "${state}" =~ ^(FAILED|KILLED|STOPPED)$ ]] && { cat "${TMPDIR}/resp.json"; die "run ${RUN_ID} reached ${state} instead of RUNNING"; }
  sleep 2
done
[[ "${state}" == "RUNNING" ]] || die "run ${RUN_ID} did not reach RUNNING in time (last state=${state})"

# The substrate claim, checked at the substrate: a Pod by that name, Running.
POD_PHASE="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod "${SANDBOX_REF}" \
  -o jsonpath='{.status.phase}' 2>/dev/null || true)"
if [[ "${POD_PHASE}" == "Running" ]]; then
  pass "run ${RUN_ID} is RUNNING and its sandbox is Pod ${SANDBOX_REF} (phase Running) in ${CONTEXT}/${NAMESPACE}"
else
  fail "sandbox_ref ${SANDBOX_REF} is not a Running Pod in ${CONTEXT}/${NAMESPACE} (phase='${POD_PHASE}')"
fi

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8)
ssh_run() { ssh "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -p "${SSH_PORT}" "${RUN_ID}@${SSH_HOST}" "$@"; }

# ── 3. the shell lane: `ssh <run-id>@host -p port id` ───────────────────────
id_out="$(ssh_run 'id' 2>"${TMPDIR}/id.err")"
id_rc=$?
if [[ "${id_rc}" -eq 0 && "${id_out}" == *"uid="* ]]; then
  pass "exec: 'id' over the k8s exec lane exited 0 — ${id_out}"
else
  fail "exec 'id': rc=${id_rc} out='${id_out}' err='$(cat "${TMPDIR}/id.err")'"
fi

ssh_run 'exit 37'
rc=$?
if [[ "${rc}" -eq 37 ]]; then
  pass "exec: nonzero exit code (37) survives the k8s exec lane exactly"
else
  fail "exec exit-code propagation on k8s: got rc=${rc}, want 37"
fi

# ── 4. the interactive pty lane (Runner.Attach on k8s → tmux) ───────────────
# -tt forces a pty-req even though stdin is a pipe; the sleeps let the tmux
# attach establish before the first keystroke and settle before `exit` (same
# shape, same reasons, as the compose lane's recording check).
(sleep 2; printf 'echo wardyn-k8s-shell-marker\n'; sleep 3; printf 'exit\n') \
  | timeout 30 ssh -tt "${SSH_OPTS[@]}" -i "${TMPDIR}/owner_key" -p "${SSH_PORT}" "${RUN_ID}@${SSH_HOST}" \
  >"${TMPDIR}/shell.log" 2>&1
if grep -q "wardyn-k8s-shell-marker" "${TMPDIR}/shell.log"; then
  pass "shell: pty session reached the pod's tmux and echoed its marker back"
else
  fail "shell: marker never came back (log: $(tr -d '\0' <"${TMPDIR}/shell.log" | tail -5))"
fi

# ── 5. the audit rows both lanes are supposed to leave ──────────────────────
audit_probe() { # audit_probe JQ-FILTER -> count, retried (writes settle async)
  local filter="$1" count=0
  for _ in $(seq 1 10); do
    api GET "/api/v1/audit?run_id=${RUN_ID}" >/dev/null
    count="$(jq "${filter} | length" "${TMPDIR}/resp.json" 2>/dev/null || echo 0)"
    [[ "${count}" -ge 1 ]] && break
    sleep 1
  done
  printf '%s' "${count}"
}

n="$(audit_probe '[.[] | select(.action=="ssh.exec" and .outcome=="success" and .data.argv=="id" and .data.exit==0)]')"
if [[ "${n}" -ge 1 ]]; then
  pass "audit: ssh.exec success row for argv 'id' with exit 0 (${n})"
else
  fail "audit: no ssh.exec row for argv 'id' exit 0"
fi

n="$(audit_probe '[.[] | select(.action=="ssh.exec" and .data.exit==37)]')"
if [[ "${n}" -ge 1 ]]; then
  pass "audit: ssh.exec row records the exact exit code 37 the client saw"
else
  fail "audit: no ssh.exec row recorded exit=37"
fi

n="$(audit_probe '[.[] | select(.action=="session.attach" and .outcome=="success" and .data.transport=="ssh")]')"
if [[ "${n}" -ge 1 ]]; then
  pass "audit: session.attach success carrying transport:ssh (${n}) — the SSH pty lane is auditable as an attach, on k8s"
else
  fail "audit: no session.attach row with transport:ssh"
fi

n="$(audit_probe '[.[] | select(.action=="session.detach" and .data.transport=="ssh")]')"
if [[ "${n}" -ge 1 ]]; then
  pass "audit: matching session.detach transport:ssh (${n})"
else
  fail "audit: no session.detach row with transport:ssh"
fi

# ── 6. member-key denial: a non-owner, non-admin key is refused ─────────────
# A SECOND principal, inserted the direct-Postgres way (see psql() above). The
# run is owned by the admin token's principal, so this key is neither the
# owner's nor an admin's — sshAuth must refuse it and audit the refusal.
# Single-quoted interpolation is safe for exactly the reason the compose lane
# gives: both values are ssh-keygen's OWN output (a "SHA256:<base64>"
# fingerprint and an "ssh-ed25519 <base64> <comment>" line), neither of which
# can contain a single quote.
ssh-keygen -t ed25519 -N "" -q -f "${TMPDIR}/foreign_key" -C "wardyn-e2e-k8s-foreign"
FOREIGN_FP="$(ssh-keygen -lf "${TMPDIR}/foreign_key.pub" | awk '{print $2}')"
FOREIGN_PUB="$(cat "${TMPDIR}/foreign_key.pub")"
if ! psql_exec "INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, created_at) VALUES ('${FOREIGN_FP}', 'e2e-k8s-foreign-principal', 'foreign-e2e-k8s-key', '${FOREIGN_PUB}', now());"; then
  fail "member-key denial setup: could not insert the second principal's key: $(cat "${TMPDIR}/psql.log")"
else
  denial_out="$(ssh "${SSH_OPTS[@]}" -i "${TMPDIR}/foreign_key" -p "${SSH_PORT}" "${RUN_ID}@${SSH_HOST}" "echo should-never-run" 2>&1)"
  denial_rc=$?
  n="$(audit_probe "[.[] | select(.action==\"ssh.auth\" and .outcome==\"failure\" and .target==\"${FOREIGN_FP}\" and .data.reason==\"not the run owner\")]")"
  if [[ "${denial_rc}" -ne 0 && "${denial_out}" != *"should-never-run"* && "${n}" -ge 1 ]]; then
    pass "member-key denial: non-owner, non-admin key refused on k8s (rc=${denial_rc}) and audited ssh.auth failure reason=\"not the run owner\""
  else
    fail "member-key denial: rc=${denial_rc} out='${denial_out}' audited_denial_rows=${n}"
  fi
fi

# ── 7. admin override: an ADMIN-role key reaches a run it does not own ──────
# A THIRD principal, same route, but with role='admin' (the column migration
# 0043 adds). The run belongs to the admin token's principal, not this key's,
# so a success here can only come from sshAuth's admin arm
# (run.CreatedBy != rec.Principal but rec.Role == oidc.RoleAdmin,
# internal/api/sshgateway.go) — and that arm is what stamps data.override.
ssh-keygen -t ed25519 -N "" -q -f "${TMPDIR}/admin_key" -C "wardyn-e2e-k8s-admin"
ADMIN_FP="$(ssh-keygen -lf "${TMPDIR}/admin_key.pub" | awk '{print $2}')"
ADMIN_PUB="$(cat "${TMPDIR}/admin_key.pub")"
if ! psql_exec "INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, role, created_at) VALUES ('${ADMIN_FP}', 'e2e-k8s-admin-principal', 'admin-e2e-k8s-key', '${ADMIN_PUB}', 'admin', now());"; then
  fail "admin-override setup: could not insert the admin-role key: $(cat "${TMPDIR}/psql.log")"
else
  override_out="$(ssh "${SSH_OPTS[@]}" -i "${TMPDIR}/admin_key" -p "${SSH_PORT}" "${RUN_ID}@${SSH_HOST}" "echo wardyn-override-ok" 2>&1)"
  override_rc=$?
  n="$(audit_probe "[.[] | select(.action==\"ssh.auth\" and .outcome==\"success\" and .target==\"${ADMIN_FP}\" and .data.override==true)]")"
  if [[ "${override_rc}" -eq 0 && "${override_out}" == *"wardyn-override-ok"* && "${n}" -ge 1 ]]; then
    pass "admin override: admin-role key reached a run it does not own on k8s, ssh.auth success audited with data.override=true"
  else
    fail "admin override: rc=${override_rc} out='${override_out}' audited_override_rows=${n}"
  fi
fi

# ── 8. the two deliberate skips, named, not silent ──────────────────────────
skip "sftp: an IMAGE contract, not a substrate one — the gateway execs /usr/lib/openssh/sftp-server INSIDE the sandbox (docs/SSH.md 'Image contract (BYOI)'), and on a cluster that image is whatever WARDYN_AGENT_IMAGES points at. Proven against Wardyn's own image by 'make test-e2e-ssh'."
skip "-L forwarding: same contract, same binary convention (socat inside the sandbox). The substrate code underneath it is the Runner.ExecStream lane sections 3-4 above already exercise on k8s. Proven end-to-end by 'make test-e2e-ssh'."

# ── summary ─────────────────────────────────────────────────────────────────
if [[ "${FAILED}" -eq 0 ]]; then
  log "SSH E2E (k8s): PASS (all checks above passed; 2 documented skips)"
else
  printf '\033[1;31m[error]\033[0m %s\n' "SSH E2E (k8s): FAIL (see [FAIL] lines above)" >&2
fi
exit "${FAILED}"
