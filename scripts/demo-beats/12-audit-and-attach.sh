#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# V12 — Audit & attach, beats 1-3: the TERMINAL half of the series finale.
#
#   scripts/record-demo.sh --video 12 \
#     --terminal-script scripts/demo-beats/12-audit-and-attach.sh
#
# The other half is ui/e2e/demo/12-audit-and-attach.spec.ts (beats 4-6, the
# audit trail and the tape). record-demo.sh concatenates the two segments
# TERMINAL FIRST, so this file is the opening of the video and the spec picks up
# exactly where it leaves off.
#
# WHAT IS ON SCREEN. Three real terminals, all typing the same command at the
# same run, each offering a different key:
#
#   +--------------------------------------------------------------+
#   | driver / captions (this script)                              |
#   +---------------------------+--------------+-------------------+
#   | owner · key A   (WIDER)   | same person  | someone else      |
#   |   the holder              |   key B      |   key C           |
#   +---------------------------+--------------+-------------------+
#
# THE LANE PROBLEM, AND THE FIX. record-demo.sh films the desktop with one
# gdigrab capture and runs exactly ONE shell script inside it — so "three
# terminals" cannot mean three windows this script has no way to place or drive.
# It means three tmux panes: real terminals, three genuinely independent ssh
# clients, one driver typing into them with `send-keys` at overlay.ts's own 45ms
# keystroke pace. Everything the viewer sees is real; the only thing tmux buys
# is that one process can drive three sessions and PROVE what happened in each.
#
# Captions, typing pace, chapter cards and narration all come from
# scripts/demo-typist.sh, exactly like V09 — this file only adds the pane
# plumbing and the per-beat assertions.
#
# STAGING, AND WHY EACH PIECE IS CHECKED IN CODE BELOW. Every one of these has
# burned a take somewhere in this project, so preflight() fails loudly BEFORE
# the camera has anything to film rather than half way through:
#
#  1. The gateway is ON: WARDYN_SSH_LISTEN=:2222 WARDYN_SSH_ADVERTISE=127.0.0.1:2222
#     where wardynd starts (it is OFF by default — DA9/DA16). /healthz's `ssh`
#     block is the check, and it is also where the advertise address and the
#     host-key fingerprint printed on the run page's card come from.
#  2. The host key on the wire MATCHES the fingerprint the console discloses
#     (DA15). A mid-series `reset-all` regenerates it, and beat 1 is a human
#     saying "it matches the card" while the client prints something else.
#  3. The run was created by the SIGNED-IN HUMAN, never the admin token. sshAuth
#     authorizes owner-only by comparing run.created_by against the key's
#     principal, so a run created as `admin-token` refuses the owner's own key
#     and beat 1 films a denial (internal/api/sshgateway.go).
#  3b. The run STARTS ON A SHELL, not the agent. The New Run wizard defaults
#     "Start with" to the agent CLI, and interactive_start=agent makes the
#     sandbox's ~/.bashrc launch `claude` in the first shell of the shared tmux
#     session — the very session beat 1 ssh's into. Every beat below would then
#     be typed into Claude's prompt and STILL PASS, which makes this the only
#     staging miss here that yields a green take over garbage.
#  4. The run's Overview tab in the console is CLOSED. The web terminal
#     auto-attaches on mount and first attach wins the holder, so an open tab
#     means BOTH ssh sessions come up read-only and beat 2 has no contrast left.
#     Nothing here can see the operator's browser — but beat 1 asserts its own
#     session is NOT read-only, which is that same fact from the other side.
#  5. The foreign key is REGISTERED, under a DIFFERENT principal. An
#     unregistered key is refused earlier and logs reason "unregistered key"
#     with actor "unknown"; the money row this whole video builds to is the
#     registered-but-not-yours branch, reason "not the run owner" (SV13). The
#     REST API can only ever bind the CALLER's own principal, so the psql
#     INSERT is the sole method for a second identity (SV19).
#  6. Both ssh clients DETACH before the browser half runs. session.recording is
#     written at detach (attach.go's finishRecording, which the ssh bridge calls
#     too), so a client left connected means an empty Session picker at beat 6.
#  7. The KERNEL SENSOR IS OFF. Beat 5 (browser lane) says "unavailable, because
#     that sensor is opt-in" over the audit page's Ground-truth chip, which
#     renders /healthz's ebpf_groundtruth.state verbatim. A stack brought up with
#     the tetragon compose profile reports "healthy" and the line is false on
#     camera — checked here because preflight is the only thing that runs before
#     the camera does.
#  8. THE OPERATOR HAS TO BE IN THE ROOM (09's lesson, same lane). This segment
#     is a gdigrab of a fixed rectangle (WARDYN_DEMO_CAPTURE, default
#     1920x1080+0+0) of the REAL DESKTOP. Nothing here can see inside that
#     rectangle, so nothing here can fail a take that filmed a notification or a
#     stray window. Before rolling: clear the top-left 1920x1080, put ONE
#     terminal there filling it (the three "terminals" are this script's tmux
#     panes and they split the window's width 50/25/25 — left widest, as the
#     script calls for — so a small window makes all three unreadable),
#     silence notifications, and stay and watch. There is no
#     fast-forward on this lane — record-demo.sh's ffwd is browser-only and is
#     skipped outright whenever a terminal segment exists — so the whole take
#     ships in real time, and the spec files no ffwd spans at all.
#
# The three keys live in three throwaway HOMEs under /tmp, one key each. That is
# what lets all three panes type the BYTE-IDENTICAL command off the run page's
# card (`ssh <run-id>@127.0.0.1 -p 2222`) and still offer three different keys.
# It also isolates known_hosts, so the owner's pane gets the first-connect
# fingerprint prompt on camera while the other two do not stop to ask — and it
# is why DA15's `ssh-keygen -R` remedy has nothing to do here: no pane ever
# reads the operator's own ~/.ssh/known_hosts, so a mid-series host-key
# regeneration can never surface as HOST-IDENTIFICATION-CHANGED on camera. It
# surfaces as the fingerprint mismatch preflight dies on instead.
#
# SAFETY. This script only ever ATTACHES to the run — it never launches, kills
# or reconfigures one — and it leaves with the ssh escape (`~.`) rather than
# `exit`, which would end the shell inside the sandbox and take the run's
# session with it. (A read-only observer could not type `exit` anyway: its
# keystrokes are dropped server-side, which is exactly what beat 2 is about.)

set -uo pipefail

_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF="${_HERE}/$(basename "${BASH_SOURCE[0]}")"
REPO_ROOT="$(cd "${_HERE}/../.." && pwd)"

WARDYN_URL="${WARDYN_URL:-http://localhost:8080}"
SESSION="${WARDYN_V10_TMUX:-wardyn-v10}"
# Three throwaway HOMEs (one key each) + this take's scratch.
TAKE_DIR="${WARDYN_V10_DIR:-/tmp/wardyn-v10}"
# The second identity the refusal is attributed to — a real, distinct principal
# string. It is what the ssh.auth failure row records, and what beat 3's
# narration means by "a different person".
FOREIGN_PRINCIPAL="${WARDYN_V10_FOREIGN_PRINCIPAL:-local:dana}"
PG_CONTAINER="${WARDYN_V10_PG:-${WARDYN_NS:-wardyn}-postgres}"
LOG="${TAKE_DIR}/preflight.log"
STATUS="${TAKE_DIR}/status"
# Handed to the browser lane so beat 6 films the SAME run these beats attacked.
RUN_ID_HANDOFF="${REPO_ROOT}/ui/test-results/demo-video/v10-run-id.txt"

# Captions, pacing constants, chapter cards and the narration timeline — the
# terminal lane's answer to ui/e2e/demo/overlay.ts. Sourced at top level like
# V09's beat script does; nothing here starts until a verb is called.
# shellcheck source=../demo-typist.sh
. "${_HERE}/../demo-typist.sh"

die() { printf '\n\033[1;31mv10: %s\033[0m\n' "$*" >&2; exit 1; }
note() { printf '[v10] %s\n' "$*" >>"${LOG}" 2>/dev/null; }

# THE TWO-DAEMON GOTCHA, which this repo has been bitten by more than once:
# `make setup` runs the compose stack on its OWN docker daemon
# (/var/run/wardyn-docker.sock), and the DEFAULT socket has no wardyn-postgres
# on it at all. Find the daemon that can actually see the container rather than
# assuming, and honour an explicit DOCKER_HOST first.
V10_DOCKER_HOST=""
resolve_docker_host() {
  local cand
  for cand in "${DOCKER_HOST:-}" "unix:///run/wardyn-docker.sock" \
              "unix:///var/run/wardyn-docker.sock" "unix:///var/run/docker.sock"; do
    [[ -n "${cand}" ]] || continue
    if DOCKER_HOST="${cand}" docker inspect "${PG_CONTAINER}" >/dev/null 2>&1; then
      V10_DOCKER_HOST="${cand}"
      return 0
    fi
  done
  return 1
}

# psql inside the stack's postgres. The host has no psql of its own — this is
# the only way in, and it is also the ONLY way to register a key against a
# principal that is not the caller's (SV19).
pg_psql() { DOCKER_HOST="${V10_DOCKER_HOST}" docker exec -i "${PG_CONTAINER}" psql "$@"; }

api() {
  # Local mode has no auth at all (SV1) and V10 takes deliberately never seed
  # WARDYN_DEMO_TOKEN — a token-authenticated caller is attributed to the
  # non-human `admin-token` principal, which is precisely the principal that can
  # never ssh into its own run. The header is here for a rehearsal stack only.
  if [[ -n "${WARDYN_DEMO_TOKEN:-}" ]]; then
    curl -fsS -H "Authorization: Bearer ${WARDYN_DEMO_TOKEN}" "$@"
  else
    curl -fsS "$@"
  fi
}

# Same call, but reporting the HTTP STATUS instead of failing on it. `curl -f`
# exits 22 on any 4xx and prints nothing, so registering an already-registered
# key (a legitimate 409 on every retake — DA11) is indistinguishable from a dead
# server unless the status is read directly.
api_code() {
  local auth=()
  [[ -n "${WARDYN_DEMO_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer ${WARDYN_DEMO_TOKEN}")
  curl -sS -o /dev/null -w '%{http_code}' "${auth[@]}" "$@"
}

# ---------------------------------------------------------------------------
# Preflight — everything in the STAGING list above, checked before a frame of it
# is filmed. Runs in the OUTER invocation with its noise in ${LOG}: the camera
# is already rolling when record-demo.sh starts this script, so a chatty
# preflight would be the first thing in the finished video.
# ---------------------------------------------------------------------------
preflight() {
  command -v jq >/dev/null || die "jq is required"
  command -v tmux >/dev/null || die "tmux is required — this video's three terminals are three panes"
  command -v ssh >/dev/null || die "an ssh client is required"
  command -v docker >/dev/null || die "docker is required (the foreign key is written straight into postgres)"
  mkdir -p "${TAKE_DIR}" || die "cannot create ${TAKE_DIR}"
  : >"${LOG}"
  rm -f "${STATUS}"

  local health
  health="$(curl -fsS "${WARDYN_URL}/healthz" 2>>"${LOG}")" || die "no control plane at ${WARDYN_URL} — bring the stack up first"
  [[ "$(jq -r '.ssh.enabled // false' <<<"${health}")" == "true" ]] \
    || die "the SSH gateway is OFF. Restart wardynd with WARDYN_SSH_LISTEN=:2222 WARDYN_SSH_ADVERTISE=127.0.0.1:2222 (DA9/DA16)"
  ADVERTISE="$(jq -r '.ssh.advertise_addr // ""' <<<"${health}")"
  FP_CARD="$(jq -r '.ssh.host_key_fingerprint // ""' <<<"${health}")"
  [[ -n "${ADVERTISE}" && -n "${FP_CARD}" ]] \
    || die "/healthz reports the gateway on but discloses no advertise address / host key"
  # host:port. IPv6 is not a demo concern — the series advertises 127.0.0.1:2222.
  SSH_HOST="${ADVERTISE%:*}"
  SSH_PORT="${ADVERTISE##*:}"

  # Beat 5 narrates the ground-truth chip as "unavailable" and the browser lane
  # ASSERTS that word (audit.tsx's GroundTruthChip renders this field verbatim).
  # A stack up with the tetragon profile answers "healthy" — a true screen under
  # a false line, four minutes into a take nothing else would fail.
  local gt
  gt="$(jq -r '.ebpf_groundtruth.state // "unavailable"' <<<"${health}")"
  # "unavailable" (never opted in) and "degraded" (ran once, gone quiet — the
  # heartbeat is persisted, so a host that ever ran the sensor stays degraded)
  # both mean no sensor feeds this stack; the narration says "dark", true for
  # either. A LIVE sensor (healthy/partial/idle) makes the line false on camera.
  [[ "${gt}" == "unavailable" || "${gt}" == "degraded" ]] \
    || die "the kernel sensor is LIVE (ebpf_groundtruth.state=${gt}) — beat 5 says the sensor is dark and the browser lane asserts it. Stop the groundtruth compose profile (tetragon + wardyn-tetragon-ingest) before this take"

  PRINCIPAL="$(api "${WARDYN_URL}/api/v1/me" | jq -r '.principal // ""')"
  [[ -n "${PRINCIPAL}" ]] || die "GET /api/v1/me returned no principal"
  [[ "${PRINCIPAL}" != "admin-token" ]] \
    || die "this session is the ADMIN TOKEN, not a human — every key it registers is dead on arrival (internal/api/sshkeys.go). Shoot V10 on the local-mode stack with no WARDYN_DEMO_TOKEN (SV1)"
  [[ "${PRINCIPAL}" != "${FOREIGN_PRINCIPAL}" ]] \
    || die "the 'foreign' principal (${FOREIGN_PRINCIPAL}) IS the operator — beat 3 would film an owner attempt"

  # WHICH run: named outright, or the newest RUNNING one.
  RUN_ID="${WARDYN_DEMO_RUN_ID:-}"
  if [[ -z "${RUN_ID}" ]]; then
    RUN_ID="$(api "${WARDYN_URL}/api/v1/runs" \
      | jq -r '[ (.items? // .)[] | select(.state == "RUNNING") ] | sort_by(.created_at) | last | .id // ""')"
  fi
  [[ -n "${RUN_ID}" ]] \
    || die "no RUNNING run to attach to — launch one from the signed-in console first (never with the admin token)"

  local run owner
  run="$(api "${WARDYN_URL}/api/v1/runs/${RUN_ID}")" || die "run ${RUN_ID} not found"
  [[ "$(jq -r .state <<<"${run}")" == "RUNNING" ]] || die "run ${RUN_ID} is not RUNNING — there is nothing to attach to"
  owner="$(jq -r '.created_by // ""' <<<"${run}")"
  [[ "${owner}" == "${PRINCIPAL}" ]] \
    || die "run ${RUN_ID} was created by '${owner}', not by you ('${PRINCIPAL}'). sshAuth is owner-only, so beat 1 would film a denial — launch the run from the signed-in console"
  # INTERACTIVE, the other half of check 3b. An AUTONOMOUS run is RUNNING too,
  # and its shared session is the AGENT's — so beat 1's `hostname && whoami`
  # lands in Claude's prompt exactly as interactive_start=agent would, the
  # observer still echoes it, and every assertion below still passes. It also
  # ends by itself the moment the agent finishes, mid-take. An interactive run
  # with an EMPTY task comes up idle and stays up, which is what this video
  # wants; leave the boot seed blank (dispatch's seed is interactive + agent +
  # non-empty-task only, so an empty one is structurally inert anyway).
  [[ "$(jq -r '.interactive // false' <<<"${run}")" == "true" ]] \
    || die "run ${RUN_ID} is not INTERACTIVE — its session belongs to the agent, so beats 1-2 would type into the agent's prompt and still pass, and the run would end by itself mid-take. Launch an interactive run (New run → Interactive, task left blank)"

  # THE ATTACH SESSION MUST OPEN ON A SHELL. The New Run wizard's "Start with"
  # DEFAULTS to the agent (ui/.../new-run/wizard-types.ts), and with
  # interactive_start=agent the sandbox's ~/.bashrc runs `claude` in the FIRST
  # shell of the shared tmux session — which is the ssh session beat 1 opens.
  # Beat 1's `hostname && whoami` and beat 2's `echo same session` would then be
  # typed into Claude Code's prompt box, and NOTHING downstream would notice:
  # the observer still sees the holder's screen, so `pane_wait 'same session'`
  # still matches the text Claude echoed back. That is the one failure mode here
  # that produces a GREEN take over garbage, which is why it is checked before a
  # frame is filmed. interactive_start is request-scoped and never stored on the
  # run row; run.create's audit data is the only record of it.
  local istart
  istart="$(api "${WARDYN_URL}/api/v1/audit?run_id=${RUN_ID}&action=run.create" \
    | jq -r '[ (.items? // .)[] | .data.interactive_start // empty ] | last // ""')"
  [[ "${istart}" != "agent" ]] \
    || die "run ${RUN_ID} was created with interactive_start=agent — its attach session opens INSIDE the agent CLI, so beats 1-2 would type into Claude's prompt and still pass. Relaunch it with New run → Start with → 'Terminal — a shell in the workspace dir'"

  # --- the three keys ------------------------------------------------------
  # Idempotent: a retake reuses them, and re-registering an identical key is a
  # 409 the gateway is right to refuse (DA11's 409-tolerance).
  local role
  for role in owner observer stranger; do
    mkdir -p "${TAKE_DIR}/${role}/.ssh"
    chmod 700 "${TAKE_DIR}/${role}/.ssh"
    [[ -f "${TAKE_DIR}/${role}/.ssh/id_ed25519" ]] \
      || ssh-keygen -q -t ed25519 -N '' -C "wardyn-v10-${role}" -f "${TAKE_DIR}/${role}/.ssh/id_ed25519" \
      || die "ssh-keygen failed for ${role}"
  done

  # The owner's two keys go in the ordinary way: the same POST /me/ssh-keys the
  # console's SSH keys page makes, resolved to the caller's own principal
  # server-side (DA11 — never the admin-token header).
  local code
  for role in owner observer; do
    code="$(api_code -X POST "${WARDYN_URL}/api/v1/me/ssh-keys" \
      -H 'Content-Type: application/json' \
      --data "$(jq -nc --arg n "wardyn v10 ${role}" --arg k "$(<"${TAKE_DIR}/${role}/.ssh/id_ed25519.pub")" \
        '{name:$n, public_key:$k}')" 2>>"${LOG}")" || code=000
    case "${code}" in
      201|409) note "key ${role}: ${code}" ;;
      *) die "registering the ${role} key returned ${code} — see ${LOG}" ;;
    esac
  done

  # The foreign key CANNOT go through the API: POST /me/ssh-keys always binds
  # the caller's own principal, so writing the row is the only way to have a
  # genuinely second identity on a single-operator stack (SV19).
  local pub
  FOREIGN_FP="$(ssh-keygen -lf "${TAKE_DIR}/stranger/.ssh/id_ed25519.pub" | awk '{print $2}')"
  [[ "${FOREIGN_FP}" == SHA256:* ]] || die "could not read the foreign key's SHA256 fingerprint"
  # Fields 1-2 only: the canonical form the API stores (ssh.MarshalAuthorizedKey
  # drops the comment), so sshAuth's byte-equality re-check against the stored
  # material passes exactly as it does for a key added through the console.
  pub="$(awk '{print $1" "$2}' "${TAKE_DIR}/stranger/.ssh/id_ed25519.pub")"
  resolve_docker_host \
    || die "no docker daemon can see ${PG_CONTAINER} (tried DOCKER_HOST, /var/run/wardyn-docker.sock, /var/run/docker.sock) — is the compose stack up?"
  note "postgres on ${V10_DOCKER_HOST}"
  pg_psql -q -v ON_ERROR_STOP=1 -U wardyn -d wardyn -c \
    "INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key)
     VALUES ('${FOREIGN_FP}', '${FOREIGN_PRINCIPAL}', 'v10 foreign key', '${pub}')
     ON CONFLICT (fingerprint) DO UPDATE SET principal = EXCLUDED.principal" >>"${LOG}" 2>&1 \
    || die "could not insert the foreign key into ${PG_CONTAINER} — see ${LOG}"
  # PROVE it landed under the OTHER principal. Without this the take films
  # reason "unregistered key" and the finale's money row is the wrong row (SV13).
  local got
  got="$(pg_psql -tAq -U wardyn -d wardyn -c \
    "SELECT principal FROM ssh_public_keys WHERE fingerprint = '${FOREIGN_FP}'" 2>>"${LOG}")"
  [[ "${got}" == "${FOREIGN_PRINCIPAL}" ]] \
    || die "the foreign key is registered to '${got}', not '${FOREIGN_PRINCIPAL}' — the refusal would log the wrong reason"

  # --- the host key --------------------------------------------------------
  # What the wire offers must be what the console's card discloses (DA15). The
  # owner's known_hosts is deliberately left EMPTY so the first-connect prompt
  # plays on camera; the other two are seeded so only beat 1 stops to ask.
  local scanned fp_wire
  scanned="$(ssh-keyscan -T 10 -p "${SSH_PORT}" "${SSH_HOST}" 2>>"${LOG}")"
  [[ -n "${scanned}" ]] || die "nothing answered ssh on ${SSH_HOST}:${SSH_PORT} — is the container port published?"
  fp_wire="$(ssh-keygen -lf - <<<"${scanned}" | awk '$NF == "(ED25519)" {print $2; exit}')"
  [[ "${fp_wire}" == "${FP_CARD}" ]] \
    || die "the gateway's host key (${fp_wire}) is NOT the one the console discloses (${FP_CARD}) — something regenerated it mid-series (DA15). Restart the stack, or re-shoot from V01"
  : >"${TAKE_DIR}/owner/.ssh/known_hosts"
  printf '%s\n' "${scanned}" >"${TAKE_DIR}/observer/.ssh/known_hosts"
  printf '%s\n' "${scanned}" >"${TAKE_DIR}/stranger/.ssh/known_hosts"
  chmod 600 "${TAKE_DIR}"/*/.ssh/known_hosts

  mkdir -p "$(dirname "${RUN_ID_HANDOFF}")" && printf '%s\n' "${RUN_ID}" >"${RUN_ID_HANDOFF}"
  note "run ${RUN_ID} owned by ${PRINCIPAL}; gateway ${SSH_HOST}:${SSH_PORT} ${FP_CARD}"
}

# ---------------------------------------------------------------------------
# Pane plumbing — the three terminals and the verbs used to drive them.
# ---------------------------------------------------------------------------

# A shell in a throwaway HOME, so a bare `ssh` in this pane can only ever offer
# that HOME's single key. `env -i` also drops any inherited SSH_AUTH_SOCK, which
# would otherwise let the operator's own agent answer for the wrong identity.
pane_shell() {
  printf 'env -i HOME=%q PATH=%q TERM=%q PS1=%q bash --norc --noprofile' \
    "${TAKE_DIR}/$1" "${PATH}" "${TERM:-xterm-256color}" '$ '
}

pane_text() { tmux capture-pane -p -t "$1" 2>/dev/null; }

# Type at overlay.ts's keystroke pace, then Enter after the pause a human leaves
# between finishing a command and running it. Same two constants demo-typist.sh's
# own type_cmd uses, so a beat here is paced identically to one in V09.
pane_type() {
  local pane="$1" cmd="$2" i
  for ((i = 0; i < ${#cmd}; i++)); do
    tmux send-keys -t "${pane}" -l -- "${cmd:i:1}"
    sleep "${_TYPIST_KEY_DELAY}"
  done
  beat "${_TYPIST_ENTER_MS}"
  tmux send-keys -t "${pane}" Enter
}

# Wait for the PRODUCT: poll a pane until it shows something. Seconds, because a
# real handshake into a real container is on the other end. These (and
# pane_settle) are the only waits allowed to be about the app — beat() is
# pacing, never a wait, exactly as in overlay.ts.
pane_wait() {
  local pane="$1" want="$2" secs="${3:-60}"
  # `end` gets its own statement: under `set -u`, bash evaluates the arithmetic
  # in a multi-assignment `local` BEFORE the earlier names in that same command
  # are visible, and `local a=1 b=$((a))` dies with "a: unbound variable".
  local end=$((SECONDS + secs))
  while ((SECONDS < end)); do
    pane_text "${pane}" | grep -qE "${want}" && return 0
    sleep 0.4
  done
  return 1
}

# Wait for a pane to stop changing. Typing into a terminal that is still
# painting eats the first characters and the shell reports "command not found"
# on camera — walkthrough.spec.ts learned this the same way, one lane over.
pane_settle() {
  local pane="$1" quiet="${2:-2}" secs="${3:-60}" last="" now="" stable=0
  local end=$((SECONDS + secs)) # see pane_wait: `local` + `set -u` + arithmetic
  while ((SECONDS < end)); do
    now="$(pane_text "${pane}")"
    if [[ "${now}" == "${last}" ]]; then
      stable=$((stable + 1))
      ((stable >= quiet * 2)) && return 0
    else
      stable=0
      last="${now}"
    fi
    sleep 0.5
  done
  return 1
}

# The ssh escape: kill the CLIENT, leave the sandbox's tmux session alone. It is
# only recognized right after a newline, hence the leading Enter.
pane_detach() {
  tmux send-keys -t "$1" Enter
  sleep 0.4
  tmux send-keys -t "$1" -l -- '~'
  sleep 0.2
  tmux send-keys -t "$1" -l -- '.'
  sleep 1.5
}

# How many of this run's audit rows match a jq predicate. The take verifier
# checks these too; asserting them HERE means a broken beat kills the take at
# the beat, not four minutes later in the browser half.
audit_rows() {
  api "${WARDYN_URL}/api/v1/audit?run_id=${RUN_ID}&action=$1" | jq "[ (.items? // .)[] | ${2} ] | length"
}

# ---------------------------------------------------------------------------
# The beats
# ---------------------------------------------------------------------------
drive() {
  # A driver that dies must take the session with it, or `tmux attach` in the
  # outer invocation waits forever on three idle panes and the recorder hangs.
  # The pause on failure is so the operator can read the reason before the
  # window disappears.
  trap '_rc=$?; if ((_rc != 0)); then printf "FAILED %s\n" "${_rc}" >"${STATUS}"; sleep 8; else printf "ok\n" >"${STATUS}"; fi; tmux kill-session -t "${SESSION}" 2>/dev/null' EXIT

  narration_zero

  # Nothing is worth speaking until the capture can see us: the session was
  # created detached and the outer invocation is still on its way to attaching.
  local end=$((SECONDS + 30))
  while ((SECONDS < end)) && [[ "$(tmux list-clients -t "${SESSION}" 2>/dev/null | wc -l)" -eq 0 ]]; do sleep 0.2; done

  # --- cold open (full screen; the panes are split in AFTER it) ------------
  chapter "Audit and attach" "The last question"
  say "We've spent the series putting boundaries around what an agent can do."
  say "But there's one question left."
  say "After the work is finished..."
  say "can you prove what happened?"

  # Three terminals, left one wider — the layout the script calls for. Split
  # here rather than in the bootstrap so they arrive on cue instead of sitting
  # empty under the cold open. Pane ids are read back from tmux (-P -F) rather
  # than assumed: they are per-SERVER, so a tmux already running other sessions
  # does not hand out %0/%1/%2.
  P_OWNER="$(tmux split-window -d -v -l 78% -t "${TMUX_PANE}" -P -F '#{pane_id}' "$(pane_shell owner)")"
  P_OBS="$(tmux split-window -d -h -l 50% -t "${P_OWNER}" -P -F '#{pane_id}' "$(pane_shell observer)")"
  P_STRANGER="$(tmux split-window -d -h -l 50% -t "${P_OBS}" -P -F '#{pane_id}' "$(pane_shell stranger)")"
  tmux select-pane -t "${P_OWNER}" -T " owner · key A "
  tmux select-pane -t "${P_OBS}" -T " same person · key B "
  tmux select-pane -t "${P_STRANGER}" -T " someone else · key C "
  tmux select-pane -t "${P_OWNER}"
  beat 900

  # The command is the run page's own, byte for byte: the "Attach from your
  # terminal" card renders `ssh <run.id>@<host> -p <port>` off the SAME
  # /healthz advertise_addr preflight split above (run-detail-ssh.tsx:82). The
  # script's old note that SSH.md/ENV.md miscall the card "Connect via SSH" is
  # STALE — both docs use the real title today; only a comment in
  # ui/src/app/lib/api/health.ts still carries the old name. All three panes
  # type THIS string; only the key behind it differs.
  local cmd="ssh ${RUN_ID}@${SSH_HOST} -p ${SSH_PORT}"

  # --- B1 · the owner attaches --------------------------------------------
  say "Let's start with access to the run itself."
  say "These are the public keys registered with Wardyn."
  say "No passwords."
  say "Just registered keys."
  say "My key was registered during setup."
  say "And that's its fingerprint."
  pane_type "${P_OWNER}" "${cmd}"
  # First connect against an empty known_hosts: the client stops and prints the
  # host key. preflight already proved that key is the one /healthz discloses —
  # the same string the run page's card shows — so "it matches the card" is a
  # fact by the time it is spoken, not a hope.
  pane_wait "${P_OWNER}" 'continue connecting|fingerprint' 45 \
    || die "no host-key prompt in the owner's terminal — known_hosts was not empty, or nothing answered on ${SSH_HOST}:${SSH_PORT}"
  pane_type "${P_OWNER}" "yes"
  pane_settle "${P_OWNER}" 2 120 || die "the owner's session never settled after accepting the host key"
  # THE STAGING CHECK THAT CANNOT BE MADE ANY OTHER WAY. If the operator left
  # the run's Overview tab open, the web terminal already holds this session and
  # the owner lands read-only — beat 2 would then have nothing to contrast.
  pane_text "${P_OWNER}" | grep -q 'read-only' \
    && die "the owner attached READ-ONLY — something already holds this terminal. Close the run's Overview tab in the console and re-shoot"
  pane_text "${P_OWNER}" | grep -q 'Permission denied' \
    && die "the owner's own key was refused — this run is not owned by ${PRINCIPAL}"
  # DIALOG-NEW-BEAT (dialog review, P10c): the episode is titled "Audit &
  # attach" and the browser half never says the word — the attach is HERE, and
  # this is the frame where it lands (the owner's key accepted, the run's own
  # shell on screen). One line names it. Drafted; see
  # local/light-episodes-dialog-flags.md.
  say "This is attach — stepping into the live session without loosening a single rule."
  # KEEP-VERIFY: both clauses below check against the SSH lane's actual
  # implementation (internal/api/sshgateway.go: "ssh <run-id>@<advertise-host>
  # lands in the same tmux session and masked live recorder the web terminal
  # (attach.go) uses" — no sshd runs inside the sandbox, and whatever the
  # shell reaches from either door crosses the same proxy/egress policy) —
  # re-check before the take.
  say "The connection is brokered by Wardyn."
  say "There isn't an SSH daemon sitting inside the sandbox waiting for connections."
  say "The browser terminal and this shell are two ways of driving the same run."
  pane_type "${P_OWNER}" "hostname && whoami"
  pane_settle "${P_OWNER}" 2 60 || true
  # The attach is real and attributed, from the control plane's own side.
  [[ "$(audit_rows session.attach 'select(.outcome == "success" and .data.transport == "ssh")')" -ge 1 ]] \
    || die "no successful session.attach with transport=ssh — the owner is not actually attached"

  # --- B2 · two windows, one driver ---------------------------------------
  say "Now I'll attach from another window."
  say "Same person."
  say "A different registered key."
  # Anchor the holder's frame before the observer arrives. If an arriving
  # observer clamped the shared tmux window (the bug the third line says is
  # fixed), the sandbox redraws and the holder's scrollback is disturbed. The
  # WIDTH itself is pinned by internal/api/attach_holder_test.go, which drives
  # the real bridge; from out here the honest, non-flaky check is that the
  # holder's own frame survived the arrival intact.
  local anchor
  anchor="$(pane_text "${P_OWNER}" | grep -v '^[[:space:]]*$' | tail -1)"
  pane_type "${P_OBS}" "${cmd}"
  pane_wait "${P_OBS}" 'read-only' 60 \
    || die "the second session was NOT admitted read-only — the holder was lost, or both clients are writing"
  # The notice names WHO holds it, which is what makes "same person" true on
  # screen: "wardyn: read-only — <principal> (ssh) holds this terminal; take it
  # over from the run page" (internal/api/sshgateway_channels.go).
  pane_text "${P_OBS}" | grep -qF "${PRINCIPAL}" \
    || die "the read-only notice does not name the holder's principal (${PRINCIPAL})"
  say "Wardyn allows the second session to observe the run."
  # One session: what is typed on the left lands on the right.
  pane_type "${P_OWNER}" "echo same session"
  pane_wait "${P_OBS}" 'same session' 30 \
    || die "the observer never saw the holder's terminal — these are not one session"
  say "And the first session doesn't have to do anything differently."
  say "The watcher gets another authenticated view of the same run."
  if [[ -n "${anchor}" ]]; then
    pane_text "${P_OWNER}" | grep -qF -- "${anchor}" \
      || die "the holder's frame was rewritten when the observer arrived — the shared terminal was disturbed (see internal/api/attach_holder_test.go)"
  fi
  beat 900

  # --- B3 · the refusal ----------------------------------------------------
  say "Now let's change one thing."
  say "Different person."
  say "Same run."
  say "Same command."
  pane_type "${P_STRANGER}" "${cmd}"
  pane_wait "${P_STRANGER}" 'Permission denied \(publickey\)' 60 \
    || die "the foreign key was not refused — check it is registered under ${FOREIGN_PRINCIPAL} and the run is owned by ${PRINCIPAL}"
  say "Refused."
  say "The run belongs to the person who created it."
  say "There's no administrator backdoor through SSH."
  say "That's intentional."
  say "An administrator can still stop the run and inspect its records."
  say "But attaching to somebody else's interactive session isn't an override."
  # THE MONEY ROW. "not the run owner" is the registered-but-foreign branch; a
  # key that was never registered logs "unregistered key" instead and the whole
  # finale is then about the wrong refusal (SV13).
  [[ "$(audit_rows ssh.auth 'select(.outcome == "failure" and .data.reason == "not the run owner")')" -ge 1 ]] \
    || die "no ssh.auth failure with reason 'not the run owner' — the foreign key was refused for some other reason"
  [[ "$(audit_rows ssh.auth 'select(.outcome == "success")')" -ge 2 ]] \
    || die "fewer than two successful ssh.auth rows — beat 4 would have nothing to show but the refusal"
  beat 1200

  # --- B3b · the decoy, tried for real -------------------------------------
  # V04's stolen sentinel, from OUTSIDE the boundary this time: an in-sandbox
  # curl would be upgraded by the proxy (it terminates TLS and injects the
  # live token), so this proof can only run from the driver's own host shell —
  # never from inside one of the three panes above. VERIFIED LIVE: this
  # endpoint answers a bad bearer with 401.
  # Owner-ratified ordinal (restructure): the decoy/SENTINEL beat is episode 07.
  say "And now let's finish the loop from episode seven."
  say "We stole the decoy credential from inside the sandbox."
  say "Let's try using it from outside."
  type_cmd "curl -s -o /dev/null -w '%{http_code}\n' https://api.anthropic.com/v1/models -H 'authorization: Bearer sk-ant-oat01-wardyn-inert-sentinel-proxy-injects-the-live-token'"
  say "Four-oh-one."
  say "The service doesn't recognize it."
  say "The sandbox had something that looked like a credential."
  say "But it never had the real one."

  # --- hand over to the browser half --------------------------------------
  # Observer first, holder last, neither with `exit`. These detaches are what
  # WRITE the session.recording rows beat 6's Session picker lists and beat 6's
  # player replays; the holder's is written last, which is what makes it the
  # newest row in the run's own trail.
  pane_detach "${P_OBS}"
  pane_detach "${P_OWNER}"
  local rec='select(.outcome == "success" and ((.target // "") | test("~ssh-")))'
  local end2=$((SECONDS + 60))
  while ((SECONDS < end2)); do
    [[ "$(audit_rows session.recording "${rec}")" -ge 1 ]] && break
    sleep 1
  done
  [[ "$(audit_rows session.recording "${rec}")" -ge 1 ]] \
    || die "no ssh session.recording was written — beat 6's Session picker would be empty. Are both ssh clients really gone?"

  narration_end
}

# ---------------------------------------------------------------------------
# Bootstrap: preflight, build the layout, hand the screen to tmux.
#
# The driver runs INSIDE the session (as its first pane) rather than outside it,
# because demo-typist.sh's captions are printed TEXT — they have to land in the
# frame being filmed, and after the split above that first pane is the caption
# strip across the top. The outer invocation's only remaining job is to attach,
# which is also what keeps record-demo.sh blocked until the beats are done.
# ---------------------------------------------------------------------------
if [[ -n "${WARDYN_V10_DRIVE:-}" ]]; then
  drive
  exit 0
fi

# `--preflight` stages the take and stops: keys registered, foreign identity in
# place, host key pinned, run ownership proved — and NOTHING filmed or attached.
# Run it the morning of the shoot; every failure it can print is a failure that
# would otherwise happen with the camera rolling.
if [[ "${1:-}" == "--preflight" ]]; then
  preflight
  printf 'v10 preflight OK\n  run        %s (owner %s)\n  gateway    %s:%s %s\n  foreign    %s -> %s\n  handoff    %s\n' \
    "${RUN_ID}" "${PRINCIPAL}" "${SSH_HOST}" "${SSH_PORT}" "${FP_CARD}" \
    "${FOREIGN_FP}" "${FOREIGN_PRINCIPAL}" "${RUN_ID_HANDOFF}"
  exit 0
fi

preflight
tmux kill-session -t "${SESSION}" 2>/dev/null

# Every value the driver needs, passed EXPLICITLY and under the same names this
# script reads at the top: a tmux server that is already running has its own
# environment, and inheritance is not a thing to bet a take on. RUN_ID /
# PRINCIPAL / SSH_HOST / SSH_PORT have no env-var form — preflight is the only
# thing that can resolve them, and the driver must not re-resolve them and
# possibly land on a different run.
#
# WARDYN_DEMO_WORK_DIR is in that list for the SAME reason and is the one that
# bites silently: demo-typist.sh writes its narration timeline to
# ${WARDYN_DEMO_WORK_DIR}/narration-terminal.json and record-demo.sh merges it
# back from ui/test-results/demo-video-12/. The typist is sourced INSIDE this
# tmux session, so without the pass-through the driver falls back to the
# pre---video path, the mux reads an empty per-video file, and beats 1-3 ship
# VOICELESS under a fully narrated browser half with every step exiting 0.
# Empty when run standalone, which `:-` collapses back to the typist's default.
tmux new-session -d -s "${SESSION}" \
  -e "WARDYN_V10_DRIVE=1" \
  -e "WARDYN_URL=${WARDYN_URL}" \
  -e "WARDYN_V10_TMUX=${SESSION}" \
  -e "WARDYN_V10_DIR=${TAKE_DIR}" \
  -e "WARDYN_V10_FOREIGN_PRINCIPAL=${FOREIGN_PRINCIPAL}" \
  -e "WARDYN_DEMO_CAPTURE_ZERO=${WARDYN_DEMO_CAPTURE_ZERO:-}" \
  -e "WARDYN_DEMO_VOICE=${WARDYN_DEMO_VOICE:-1}" \
  -e "WARDYN_DEMO_WORK_DIR=${WARDYN_DEMO_WORK_DIR:-}" \
  -e "RUN_ID=${RUN_ID}" \
  -e "PRINCIPAL=${PRINCIPAL}" \
  -e "SSH_HOST=${SSH_HOST}" \
  -e "SSH_PORT=${SSH_PORT}" \
  "bash '${SELF}'" || die "tmux could not start the take's session"

# Frame hygiene: no status bar, no mouse — but DO show pane titles, because they
# are how the viewer knows which terminal belongs to whom.
tmux set-option -t "${SESSION}" status off
tmux set-option -t "${SESSION}" mouse off
tmux set-option -t "${SESSION}" pane-border-status top
tmux set-option -t "${SESSION}" pane-border-format '#{pane_title}'

tmux attach -t "${SESSION}"

# The driver's verdict, not tmux's: `tmux attach` exits 0 whenever the session
# goes away, including when it went away because a beat failed its assertion.
# record-demo.sh treats a non-zero exit here as "this take is incomplete".
[[ "$(cat "${STATUS}" 2>/dev/null)" == "ok" ]] \
  || die "the beats did not complete — $(cat "${STATUS}" 2>/dev/null || echo 'no status written'); preflight log: ${LOG}"
