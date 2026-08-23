#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# V13 — Your terminal, our cluster. A TERMINAL-ONLY video: there is no
# ui/e2e/demo/13-*.spec.ts and there is not meant to be one.
#
#   scripts/record-demo.sh --video 13 \
#     --terminal-script scripts/demo-beats/13-terminal-to-the-cluster.sh
#
# WHAT THIS FILMS. One shell, five beats, against a REAL Kubernetes cluster —
# the one `make kind-quickstart` leaves behind (deploy/kind/quickstart.sh):
#
#   1. `kubectl get pods -n wardyn`     the control plane is a Deployment; the
#                                       run on the bench is two Pods, sandbox
#                                       and proxy
#   2. `wardyn ssh --print <run-id>`    the connect string, read off the
#                                       daemon's own /healthz — byte-identical
#                                       to the console's "Attach from your
#                                       terminal" card
#   3. `ssh <run-id>@... hostname`      the Pod answers with the name kubectl
#                                       just listed
#   4. `ssh <run-id>@... 'exit 37'`     a nonzero status survives the k8s exec
#                                       lane, which carries it out of band
#   5. `wardyn audit <run-id>`          every connection and every exec, on the
#                                       trail, with the exit code it really had
#
# WHY IT IS A SINGLE PANE. V12 needs three terminals because its subject is
# three identities; this one's subject is one operator and one cluster, so it
# is V11's shape — the driver types into the terminal record-demo.sh is already
# filming, and nothing here needs tmux.
#
# NO NEW CLAIM IS MADE ABOUT SSH ITSELF. What is new is the substrate: the same
# gateway, the same audit rows, with a Pod on the far side of the channel
# instead of a container (internal/runner/k8s/session.go). The machine-checked
# version of this video is scripts/run-e2e-ssh-k8s.sh, which this beat script
# deliberately mirrors — same cluster, same checks, one of them on camera.
#
# ═══ OPERATOR STAGING — every item is checked by preflight() below ═══
#
#  1. A `make kind-quickstart` CLUSTER IS UP and its kubeconfig context is the
#     CURRENT one. Beat 1 types a bare `kubectl get pods -n wardyn`, with no
#     --context, because that is what an operator types — so the ambient
#     context has to be the cluster the rest of the video is about.
#  2. /healthz REPORTS runner=k8s. quickstart publishes 127.0.0.1:8080 and so
#     does the operator's compose stack; a 200 says nothing about who answered,
#     and this whole video's title is a claim about the substrate.
#  3. THE GATEWAY IS ON (`ssh.enabled`), which for the chart means
#     ssh.enabled=true (quickstart.sh sets it). Off means beat 2 has no string
#     to print and beat 3 nothing to dial.
#  4. THE HOST KEY ON THE WIRE IS THE ONE /healthz DISCLOSES, and the one the
#     operator's known_hosts already trusts (DA15). A mid-series cluster
#     rebuild regenerates it, and beat 3 would then film the full REMOTE HOST
#     IDENTIFICATION HAS CHANGED banner in a governance demo. Remedy is the
#     documented one: ssh-keygen -R '[127.0.0.1]:2222'.
#  5. A RUNNING RUN EXISTS, owned by this caller. sshAuth is a plain
#     `run.CreatedBy == key.Principal` (internal/api/sshgateway.go), so a run
#     someone else created refuses this key and beat 3 films a denial. This
#     script never launches, kills or reconfigures a run — preflight names the
#     one curl that creates a suitable one and stops.
#  6. THE OPERATOR'S OWN PUBLIC KEY IS REGISTERED. Registration is a silent
#     POST /api/v1/me/ssh-keys here (201, or 409 on a retake — both fine);
#     nothing about key management is filmed, that is V12's subject.
#  7. NON-INTERACTIVE AUTH REALLY WORKS. Preflight opens a real connection with
#     `ssh -N` and no exec, so it proves the key, the agent, known_hosts and
#     the gateway in one go and leaves an ssh.auth row but NO ssh.exec row —
#     the two exec rows on the trail are then exactly the two beats 3-4 filmed.
#     A passphrase-locked key with no agent hangs the take otherwise, on camera.
#  8. THE ADMIN TOKEN IS NEVER TYPED. `wardyn audit` reads it from
#     WARDYN_ADMIN_TOKEN, which preflight exports from the install's own
#     Secret. Nothing in the beats prints it, and nothing here runs a command
#     whose --help would (cobra renders a flag default, and that default is the
#     token).
#  9. THE OPERATOR HAS TO BE IN THE ROOM (V11/V12's lesson, same lane). This is
#     a gdigrab of a fixed desktop rectangle (WARDYN_DEMO_CAPTURE, default
#     1920x1080+0+0). Nothing here can see inside it, so nothing here can fail
#     a take that filmed a notification or a stray window. Clear the top-left
#     1920x1080, put one terminal there, silence notifications, and watch it —
#     there is no fast-forward on this lane (record-demo.sh skips ffwd whenever
#     a terminal segment exists), so it ships in real time.
#
#     scripts/demo-beats/13-terminal-to-the-cluster.sh --preflight
#
# stages and checks all of the above and films nothing. Run it the morning of
# the shoot; every failure it can print is one that would otherwise happen with
# the camera rolling.

set -uo pipefail

_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${_HERE}/../.." && pwd)"
cd "${REPO_ROOT}" || exit 1

# Captions, typing, chapter cards and the narration timeline — the terminal
# lane's answer to ui/e2e/demo/overlay.ts. Sourced at top level like V11/V12;
# nothing runs until a verb is called. Its timeline lands in
# ${WARDYN_DEMO_WORK_DIR}/narration-terminal.json, which is the per-video
# directory record-demo.sh exports and merges back from.
# shellcheck source=../demo-typist.sh
. "${_HERE}/../demo-typist.sh"

# The cluster deploy/kind/quickstart.sh installs, and the port it publishes.
# Constants rather than knobs for run-e2e-ssh-k8s.sh's reason: a second install
# would need its own token and port discovery, which is a different script.
CONTEXT="${WARDYN_V13_CONTEXT:-kind-wardyn-quickstart}"
NAMESPACE="${WARDYN_V13_NAMESPACE:-wardyn}"
WARDYN_URL="${WARDYN_URL:-http://127.0.0.1:8080}"
TAKE_DIR="${WARDYN_V13_DIR:-/tmp/wardyn-v13}"
LOG="${TAKE_DIR}/preflight.log"
# Where the verifier reads this take's run id back from (check_video_13).
HANDOFF="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-13}/v13-run-id.txt"

die()  { printf '\n\033[1;31mv13: %s\033[0m\n' "$*" >&2; exit 1; }
note() { printf '[v13] %s\n' "$*" >>"${LOG}" 2>/dev/null; }

kc()  { kubectl --context "${CONTEXT}" -n "${NAMESPACE}" "$@"; }
api() { curl -fsS -H "Authorization: Bearer ${WARDYN_ADMIN_TOKEN}" "$@"; }
# Same call reporting the HTTP STATUS instead of failing on it: registering an
# already-registered key is a legitimate 409 on every retake, and `curl -f`
# makes that indistinguishable from a dead server.
api_code() { curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${WARDYN_ADMIN_TOKEN}" "$@"; }

# ---------------------------------------------------------------------------
# Preflight — the staging list above, checked before a frame of it is filmed.
# Chatty output goes to ${LOG}: record-demo.sh has the camera rolling by the
# time this script starts, so anything printed here is the first thing in the
# finished video.
# ---------------------------------------------------------------------------
preflight() {
  local c
  for c in jq kubectl ssh curl; do command -v "${c}" >/dev/null || die "${c} is required"; done
  mkdir -p "${TAKE_DIR}" || die "cannot create ${TAKE_DIR}"
  : >"${LOG}"

  [[ -x "${REPO_ROOT}/wardyn" ]] || die "no ./wardyn binary at ${REPO_ROOT} — beats 2 and 5 type it (make build)"
  [[ "$(kubectl config current-context 2>/dev/null)" == "${CONTEXT}" ]] \
    || die "kubectl's current context is not ${CONTEXT} — beat 1 types a bare 'kubectl get pods' and would film the wrong cluster. Fix: kubectl config use-context ${CONTEXT}"
  kc get deployment wardyn >/dev/null 2>&1 \
    || die "no wardyn install in ${CONTEXT}/${NAMESPACE} — run 'make kind-quickstart' first (this script never creates a cluster)"

  # The install's own admin token, read the way quickstart.sh re-reads it. It
  # is EXPORTED, never typed: `wardyn audit` picks it up from the environment.
  if [[ -z "${WARDYN_ADMIN_TOKEN:-}" ]]; then
    WARDYN_ADMIN_TOKEN="$(kc get secret wardyn-auth -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  fi
  [[ -n "${WARDYN_ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth in ${NAMESPACE}"
  export WARDYN_ADMIN_TOKEN WARDYN_URL

  local health
  health="$(curl -fsS "${WARDYN_URL}/healthz" 2>>"${LOG}")" || die "no control plane at ${WARDYN_URL} — is the quickstart port-forward up?"
  # WHO answered, not merely that something did: quickstart and the operator's
  # compose stack both publish 127.0.0.1:8080, and "our cluster" is this
  # video's entire claim.
  [[ "$(jq -r '.runner // ""' <<<"${health}")" == "k8s" ]] \
    || die "${WARDYN_URL}/healthz reports runner=$(jq -r '.runner // "<none>"' <<<"${health}"), not k8s — something other than the cluster owns that port (a compose stack?)"
  [[ "$(jq -r '.ssh.enabled // false' <<<"${health}")" == "true" ]] \
    || die "the SSH gateway is OFF on this install — the chart needs ssh.enabled=true (deploy/kind/quickstart.sh sets it)"
  ADVERTISE="$(jq -r '.ssh.advertise_addr // ""' <<<"${health}")"
  FP_CARD="$(jq -r '.ssh.host_key_fingerprint // ""' <<<"${health}")"
  [[ -n "${ADVERTISE}" && -n "${FP_CARD}" ]] \
    || die "/healthz reports the gateway on but discloses no advertise address / host key"
  # host:port. IPv6 is not a demo concern — quickstart advertises 127.0.0.1:2222.
  SSH_HOST="${ADVERTISE%:*}"
  SSH_PORT="${ADVERTISE##*:}"

  # DA15, both halves: what the wire offers must be what /healthz (and so the
  # console card, and so `wardyn ssh`) discloses, AND what this operator's
  # known_hosts already trusts. The second half is what keeps the HOST
  # IDENTIFICATION HAS CHANGED banner off camera after a cluster rebuild.
  local scanned fp_wire fp_known
  scanned="$(ssh-keyscan -T 10 -p "${SSH_PORT}" "${SSH_HOST}" 2>>"${LOG}")"
  [[ -n "${scanned}" ]] || die "nothing answered ssh on ${SSH_HOST}:${SSH_PORT} — is the gateway's port published?"
  fp_wire="$(ssh-keygen -lf - <<<"${scanned}" | awk '$NF == "(ED25519)" {print $2; exit}')"
  [[ "${fp_wire}" == "${FP_CARD}" ]] \
    || die "the gateway's host key (${fp_wire}) is NOT the one /healthz discloses (${FP_CARD})"
  fp_known="$(ssh-keygen -F "[${SSH_HOST}]:${SSH_PORT}" -l 2>/dev/null | awk '$NF == "(ED25519)" {print $2; exit}')"
  [[ -z "${fp_known}" || "${fp_known}" == "${FP_CARD}" ]] \
    || die "known_hosts trusts ${fp_known} for [${SSH_HOST}]:${SSH_PORT} but the gateway now offers ${FP_CARD} — beat 3 would film REMOTE HOST IDENTIFICATION HAS CHANGED. Fix: ssh-keygen -R '[${SSH_HOST}]:${SSH_PORT}'"

  # The operator's own key, registered the ordinary way (the same POST the
  # console's SSH keys page makes — it binds the CALLER's principal).
  local pub code
  pub="${WARDYN_V13_PUBKEY:-${HOME}/.ssh/id_ed25519.pub}"
  [[ -s "${pub}" ]] || die "no public key at ${pub} — beat 3 authenticates with the operator's own key (ssh-keygen -t ed25519), or point WARDYN_V13_PUBKEY at one"
  code="$(api_code -X POST "${WARDYN_URL}/api/v1/me/ssh-keys" -H 'Content-Type: application/json' \
    --data "$(jq -nc --arg n 'wardyn v13' --arg k "$(<"${pub}")" '{name:$n, public_key:$k}')" 2>>"${LOG}")" || code=000
  case "${code}" in
    201|409) note "key ${pub}: ${code}" ;;
    *) die "registering ${pub} returned ${code} — see ${LOG}" ;;
  esac

  # WHICH run: named outright, or the newest RUNNING one. Never created here —
  # this script only ever attaches to what an operator staged (V12's rule).
  RUN_ID="${WARDYN_DEMO_RUN_ID:-}"
  if [[ -z "${RUN_ID}" ]]; then
    RUN_ID="$(api "${WARDYN_URL}/api/v1/runs" 2>>"${LOG}" \
      | jq -r '[ (.items? // .)[] | select(.state == "RUNNING") ] | sort_by(.created_at) | last | .id // ""')"
  fi
  [[ -n "${RUN_ID}" ]] || die "no RUNNING run on this install. Stage one (idle by construction — interactive means no agent task is exec'd):
  curl -sS -X POST ${WARDYN_URL}/api/v1/runs -H \"Authorization: Bearer \${WARDYN_ADMIN_TOKEN}\" \\
    -H 'Content-Type: application/json' -d '{\"agent\":\"claude-code\",\"repo\":\"local:demo\",\"confinement_class\":\"CC1\",\"interactive\":true,\"inline_policy\":{\"allowed_domains\":[],\"first_use_approval\":\"always_deny\",\"min_confinement_class\":\"CC1\",\"auto_stop_after_sec\":-1}}'"

  local run owner me
  run="$(api "${WARDYN_URL}/api/v1/runs/${RUN_ID}" 2>>"${LOG}")" || die "run ${RUN_ID} not found on ${WARDYN_URL}"
  [[ "$(jq -r .state <<<"${run}")" == "RUNNING" ]] || die "run ${RUN_ID} is not RUNNING — there is nothing to reach into"
  SANDBOX_REF="$(jq -r '.sandbox_ref // ""' <<<"${run}")"
  [[ -n "${SANDBOX_REF}" ]] || die "run ${RUN_ID} reports no sandbox_ref — beat 3 compares it against the hostname the Pod answers with"
  owner="$(jq -r '.created_by // ""' <<<"${run}")"
  me="$(api "${WARDYN_URL}/api/v1/me" 2>>"${LOG}" | jq -r '.principal // ""')"
  [[ -n "${me}" && "${owner}" == "${me}" ]] \
    || die "run ${RUN_ID} was created by '${owner}', not by this caller ('${me}') — sshAuth is owner-only, so beat 3 would film a denial"

  # The substrate claim, checked at the substrate — and BOTH pods, because beat
  # 1 narrates "one run, two pods".
  [[ "$(kc get pod "${SANDBOX_REF}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]] \
    || die "sandbox_ref ${SANDBOX_REF} is not a Running Pod in ${CONTEXT}/${NAMESPACE}"
  [[ "$(kc get pod "${SANDBOX_REF/wardyn-agent-/wardyn-proxy-}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]] \
    || die "no Running proxy Pod beside ${SANDBOX_REF} — beat 1 says 'one run, two pods'"

  # The string beat 2 films, from the command that films it. It has to be the
  # one beat 3 types, character for character, or the video's "I only copied
  # it" is false.
  SSH_CMD="ssh ${RUN_ID}@${SSH_HOST} -p ${SSH_PORT}"
  local printed
  printed="$("${REPO_ROOT}/wardyn" ssh --print "${RUN_ID}" 2>>"${LOG}")"
  [[ "${printed}" == "${SSH_CMD}" ]] \
    || die "'wardyn ssh --print' emits '${printed}' but beat 3 types '${SSH_CMD}' — the two beats disagree"

  # Auth, proved without an exec: -N opens the connection and requests no
  # command, so this leaves an ssh.auth row and NO ssh.exec row, and the two
  # exec rows beat 5 films are exactly the two beats 3-4 ran. A key that would
  # prompt for a passphrase, or an agent that is not running, dies HERE
  # instead of hanging the take with the camera on it. rc 124 is `timeout`
  # killing an idle, AUTHENTICATED session — anything else is a refusal.
  timeout 6 ssh -o BatchMode=yes -o ConnectTimeout=8 -N "${RUN_ID}@${SSH_HOST}" -p "${SSH_PORT}" >>"${LOG}" 2>&1
  [[ $? -eq 124 ]] \
    || die "the operator's key did not authenticate non-interactively against ${SSH_CMD} — see ${LOG} (a passphrase-locked key with no ssh-agent would prompt on camera)"

  mkdir -p "$(dirname "${HANDOFF}")" && printf '%s\n' "${RUN_ID}" >"${HANDOFF}"
  note "run ${RUN_ID} (${SANDBOX_REF}) owned by ${me}; gateway ${SSH_HOST}:${SSH_PORT} ${FP_CARD}"
}

# ---------------------------------------------------------------------------
# The beats
# ---------------------------------------------------------------------------
beats() {
  narration_zero
  chapter "Your terminal, our cluster" "A Kubernetes sandbox, over plain SSH"
  say "Wardyn runs on Kubernetes. Same product, a different substrate underneath."

  # --- B1 · what the cluster is actually holding --------------------------
  type_cmd "kubectl get pods -n ${NAMESPACE}"
  say "The control plane is a Deployment. Every sandbox it starts is a Pod."
  say "One run, two pods: the sandbox, and the proxy that fences everything it sends."
  say "Neither of them holds a kubeconfig, or any credential of this cluster."

  # --- B2 · the connect string is the product's, not mine -----------------
  say "To reach into it I do not need kubectl, a port forward, or a cluster role."
  type_cmd "./wardyn ssh --print ${RUN_ID}"
  say "The command comes from the daemon's own health endpoint. The console card prints the same string."
  say "The run id is the username. A registered public key is the whole credential."

  # --- B3 · the Pod answers with its own name -----------------------------
  type_cmd "${SSH_CMD} hostname"
  say "That hostname is the Pod kubectl just listed. Same name, seen from the inside."

  # --- B4 · the exit code crosses the cluster intact ----------------------
  say "Kubernetes carries an exec's exit status out of band, in a side channel."
  type_cmd "${SSH_CMD} 'exit 37'"
  # Single-quoted: `$?` must reach the shell being filmed, not this one.
  # type_cmd restores $? before its eval, so this prints ssh's status.
  type_cmd 'echo $?'
  say "Thirty-seven came back intact. Your shell keeps its contract across the cluster."

  # --- B5 · the trail ------------------------------------------------------
  type_cmd "./wardyn audit ${RUN_ID} --action-prefix ssh."
  say "Every connection is a row. Which key, which run, allowed or refused."
  type_cmd "./wardyn audit ${RUN_ID} --action-prefix ssh.exec --json | jq -c '.[] | {argv: .data.argv, exit: .data.exit}'"
  say "And each exec keeps what it ran, and what it exited. Thirty-seven, on the record."

  say "Your terminal. Our cluster. The same governed trail as everything else."
  narration_end
}

# ---------------------------------------------------------------------------
# The take's own honesty gate.
#
# Everything above NARRATES success. Silent on a good take, loud on a bad one —
# "exit 0" has been the least reliable signal on this project three times over,
# and a take that speaks "thirty-seven, on the record" over an empty trail is
# invisible until somebody watches the whole thing. These are the same rows
# scripts/run-e2e-ssh-k8s.sh asserts and verify-demo-take.sh re-checks; asserting
# them here means a broken beat fails the take at the beat.
# ---------------------------------------------------------------------------
gate() {
  local rows n
  # Audit writes settle asynchronously; give them the same few seconds the e2e
  # lane does rather than racing the last beat.
  for _ in $(seq 1 10); do
    rows="$(api "${WARDYN_URL}/api/v1/audit?run_id=${RUN_ID}&action=ssh.exec" 2>/dev/null)"
    n="$(jq '[ (.items? // .)[] | select(.data.exit == 37) ] | length' <<<"${rows}" 2>/dev/null || echo 0)"
    [[ "${n}" -ge 1 ]] && break
    sleep 1
  done
  [[ "${n}" -ge 1 ]] || die "no ssh.exec row carrying exit 37 — beat 5 films that number and beat 4 spoke it"
  n="$(jq '[ (.items? // .)[] | select(.outcome == "success" and .data.argv == "hostname" and .data.exit == 0) ] | length' <<<"${rows}" 2>/dev/null || echo 0)"
  [[ "${n}" -ge 1 ]] || die "no successful ssh.exec row for argv 'hostname' — beat 3 did not reach the Pod"
}

# ---------------------------------------------------------------------------
if [[ "${1:-}" == "--preflight" ]]; then
  preflight
  printf 'v13 preflight OK\n  cluster    %s/%s\n  run        %s\n  sandbox    %s\n  gateway    %s:%s %s\n  connect    %s\n  handoff    %s\n' \
    "${CONTEXT}" "${NAMESPACE}" "${RUN_ID}" "${SANDBOX_REF}" \
    "${SSH_HOST}" "${SSH_PORT}" "${FP_CARD}" "${SSH_CMD}" "${HANDOFF}"
  exit 0
fi

preflight
beats
gate
exit 0
