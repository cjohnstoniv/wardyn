#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# V11 · CI & headless — the TERMINAL half (beats 1-4).
#
# WHAT THIS FILMS. Four host-shell beats, in one continuous frame: the CI
# policy file (`cat examples/policies/ci.json`), one env-prefixed
# `scripts/ci-run.sh` invocation that builds its own control plane and runs one
# governed sandbox to completion, the exit code that invocation hands back
# (`echo $?`), and the three artifacts it leaves behind. Beats 5-6 are a browser
# and live in ui/e2e/demo/11-ci-and-headless.spec.ts; record-demo.sh joins the
# two segments, terminal first, and merges both narration timelines.
#
#     scripts/record-demo.sh --video 11 \
#       --terminal-script scripts/demo-beats/11-ci-and-headless.sh
#
# THE STACK THIS BRINGS UP IS NOT THE SERIES STACK, ON PURPOSE. Every other
# video in the 0.5 series films the :8080 console that `make setup` left behind.
# This one films a pipeline, and a pipeline does not inherit a running Wardyn —
# it starts from nothing. WARDYN_CI_PROJECT pins COMPOSE_PROJECT_NAME + WARDYN_NS
# to `wardyn-ci-demo` (so a retake's teardown is deterministic instead of
# scoped to a PID that has since exited), and WARDYN_UP_PORT moves the console
# off 8080 so the two stacks coexist for the length of the take.
#
# WARDYN_CI_KEEP=1 IS LOAD-BEARING, NOT A DEBUG FLAG. Without it ci-run.sh's
# EXIT trap tears the stack down the instant the run finishes — and beats 5-6
# would then point a browser at a closed port. KEEP is what makes the browser
# half of this video exist at all (DA14).
#
# ARTIFACTS ARE THE HANDOFF. Beat 4 films `ci-artifacts/`, and the browser spec
# READS `ci-artifacts/run.json` to learn which run id it is supposed to be
# looking at. That is deliberate: the two lanes agree on the run because they
# read the same file the pipeline actually wrote, not because a human typed the
# same uuid into two places.
#
# ═══ OPERATOR STAGING — do these BEFORE the take rolls ═══
#
#  1. LOCAL IMAGES MUST EXIST. WARDYN_CI_SKIP_BUILD=1 skips the wardynd/proxy/
#     agent builds — record-demo.sh's Act 0 (`make setup`) is what produces
#     them, so this is satisfied by any normal take. It also needs
#     `wardyn/agent-claude-code:local` present, because that image is where the
#     runner tools are extracted from even for a pure BYOI run.
#  2. REHEARSE ONCE, FULLY. The first `ubuntu:24.04` run pays for the image pull
#     AND the BYOI wrap build — minutes of dead air, and it changes beat 2's
#     line spacing. Shoot the second take, not the first.
#  3. RETAKE CLEANUP (DA14-rev). ci-run.sh reaps its own compose objects, but
#     the docker runner mints the sandbox trio directly and KEEP=1 skips the
#     branch that removes them. After a retake, for the PREVIOUS run's uuid:
#         docker rm -f wardyn-proxy-<id> wardyn-agent-<id>
#         docker network rm wardyn-int-<id>
#     (`wardyn-int-<id>` is a NETWORK, not a container — `docker rm -f` on it
#     silently does nothing.) The previous uuid is in that take's own
#     ci-artifacts/run.json, which outlives every teardown here.
#  4. TEARDOWN AFTER BEAT 6, not before. ci-run.sh prints the exact command with
#     all three load-bearing vars (DOCKER_HOST / WARDYN_NS / WARDYN_CI_TOOLS_DIR)
#     already interpolated — paste it from the scrollback, not from memory. With
#     the scrollback gone (a retake, a closed terminal) the tools dir's mktemp
#     path is unrecoverable, but the project name is FIXED and `down` mounts
#     nothing — the overlay's ${WARDYN_CI_TOOLS_DIR:?} only has to be non-empty:
#         DOCKER_HOST=unix:///run/wardyn-docker.sock WARDYN_NS=wardyn-ci-demo \
#         WARDYN_CI_TOOLS_DIR=/tmp docker compose -p wardyn-ci-demo \
#           -f deploy/compose/docker-compose.yaml \
#           -f deploy/compose/docker-compose.ci.yaml down --volumes
#     (That leaks the tools dir under /tmp. It is a few hundred KB of scripts.)
#     TWO-DAEMON BOX: ci-run.sh calls wardyn_pick_docker_host, which prefers
#     /run/wardyn-docker.sock then /var/run/wardyn-docker.sock over the default
#     context — so this stack lands on the NATIVE dockerd, not Docker Desktop,
#     and SKIP_BUILD's images have to exist THERE. A teardown, or a `docker
#     images` check, against the other daemon exits 0 having found nothing.
#  5. THE OPERATOR HAS TO BE IN THE ROOM. This segment is a gdigrab of a fixed
#     rectangle (WARDYN_DEMO_CAPTURE, default 1920x1080+0+0) of the REAL
#     DESKTOP — not of a window anything here owns. Nothing in this script can
#     see what is inside that rectangle, so nothing here can fail a take that
#     filmed a notification, a stray window, or somebody else's terminal. Before
#     rolling: clear the top-left 1920x1080, put the terminal there, silence
#     notifications, and stay and watch it. There is no fast-forward on this
#     lane — record-demo.sh's ffwd is browser-only and is explicitly skipped
#     whenever a terminal segment exists — so the whole thing ships in real
#     time, at the speed you are watching it happen.
#  6. The browser half authenticates with sessionStorage['wardyn_admin_token']
#     = WARDYN_ADMIN_TOKEN (default `demo-admin-token`). If you override it,
#     export it before record-demo.sh so BOTH lanes see the same value.

set -uo pipefail

_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${_HERE}/../.." && pwd)"
cd "${REPO_ROOT}" || exit 1

# The typist: say / type_cmd / beat / chapter / narration_zero / narration_end.
# Same vocabulary as ui/e2e/demo/overlay.ts, same pacing constants, so a beat
# written for this lane reads the same as one written for the browser lane.
#
# POINT THE TIMELINE AT THIS TAKE'S OWN DIRECTORY, BEFORE SOURCING. The two
# halves of the recorder disagree on where a terminal timeline lives:
# demo-typist.sh defaults it to ui/test-results/demo-video/narration-terminal
# .json (the pre---video path), while record-demo.sh reads and merges
# ${WARDYN_DEMO_WORK_DIR}/narration-terminal.json — which for `--video 11` is
# ui/test-results/demo-video-11/. Left alone, the typist writes one file and the
# mux reads another: beats 1-4 ship SILENT under a fully narrated browser half,
# and every step of the take still exits 0. Setting it here is a per-caller
# patch; the root fix is one line in demo-typist.sh's own default (which
# scripts/demo-beats/12-audit-and-attach.sh needs too) and is out of scope for
# this file. Honors an explicit override, and collapses to the typist's default
# when this script is run standalone with no WARDYN_DEMO_WORK_DIR.
export WARDYN_DEMO_TERMINAL_TIMELINE="${WARDYN_DEMO_TERMINAL_TIMELINE:-${WARDYN_DEMO_WORK_DIR:+${WARDYN_DEMO_WORK_DIR}/narration-terminal.json}}"
# shellcheck source=../demo-typist.sh
. "${_HERE}/../demo-typist.sh"

# ── the beat's own constants ────────────────────────────────────────────────
# Local, not in ui/e2e/demo/task.ts: no other video runs a pipeline, and the
# browser half reads every one of these back out of run.json rather than
# importing them. One writer, one reader, one file between them.
CI_PROJECT="${WARDYN_CI_PROJECT:-wardyn-ci-demo}"
CI_PORT="${WARDYN_UP_PORT:-8099}"
CI_TASK="${WARDYN_CI_TASK:-echo hello from a governed sandbox}"
CI_IMAGE="${WARDYN_CI_IMAGE:-ubuntu:24.04}"
ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
ART_DIR="${WARDYN_CI_OUT:-ci-artifacts}"
# The red-build beat (B3b): a SECOND, throwaway control plane — never B2's
# project/port/out-dir, or ci-run.sh's own unconditional `down --volumes` at
# start would tear down the stack beats 5-6 are about to film.
CI_RED_TASK="${WARDYN_CI_RED_TASK:-curl -sS --max-time 10 https://example.com/ || exit 7}"
CI_RED_PROJECT="${WARDYN_CI_RED_PROJECT:-${CI_PROJECT}-red}"
CI_RED_PORT="${WARDYN_CI_RED_PORT:-$((CI_PORT + 1))}"
CI_RED_OUT="${WARDYN_CI_RED_OUT:-${ART_DIR}-red}"

fail() { printf '\n\033[1;31m[09] %s\033[0m\n' "$*" >&2; exit 1; }

# ── quiet prep, before the chapter card clears the screen ───────────────────
# `ls ci-artifacts/` in beat 4 must show THIS take's three files and nothing a
# previous one left behind. Silent: the camera is already rolling.
rm -rf "${ART_DIR}" >/dev/null 2>&1

narration_zero
chapter "CI and headless" "The same governance, unattended"

# ── COLD OPEN ───────────────────────────────────────────────────────────────
say "So far, a human has been available to make the decision."
say "CI doesn't have that luxury."
say "A pipeline has no hands."
say "So the policy has to make the decision for it."
say "And instead of a human clicking a button, the pipeline gets a result."
# P9 (dialog review, owner-ratified 2026-08-23): "verdict" belongs to episode
# 09's replay chips now — dropped from both lanes of this episode.
say "That result becomes the build's pass or fail."

# ── B1 · The policy ──────────────────────────────────────────────────────────
# The dialog no longer counts the fields out loud (it lists the categories:
# allow, deny, unexpected, TTL, confinement) — but the fixture is still exactly
# 8 JSON keys (allowed_domains, denied_domains, allow_all_egress,
# first_use_approval, allowed_methods, min_confinement_class, eligible_grants,
# auto_stop_after_sec) and the check below stays as a drift guard: if a ninth
# key ever lands, the beat's "same basic things we've already seen" claim is
# the thing that goes stale, silently, on camera.
type_cmd "cat examples/policies/ci.json"
beat 600
say "This is the CI policy."
say "It defines the same basic things we've already seen:"
say "what is allowed,"
say "what gets denied,"
say "what happens to anything unexpected,"
say "how long the run can live,"
say "and what level of confinement is required."
say "The floor is Fence."
say "That's common for CI runners because they don't always have the virtualization support needed for stronger isolation."
say "And for unattended work, an unexpected request can't sit around waiting for somebody who isn't there."
say "It fails."

# The claim above is checked, not assumed: a fixture edit that adds a ninth key
# means the fixture no longer matches "the same basic things we've already
# seen" and nobody would notice from the picture alone.
if command -v jq >/dev/null 2>&1; then
  _fields="$(jq 'keys | length' examples/policies/ci.json 2>/dev/null || echo 0)"
  [[ "${_fields}" == "8" ]] \
    || fail "examples/policies/ci.json has ${_fields} fields, not the 8 this beat's policy fixture was written against. Re-cut the beat or the fixture."
fi

# ── B2 · One command, no human ──────────────────────────────────────────────
# The whole invocation on one line, because that IS the beat: a pipeline step is
# one command with its environment in front of it, not a session someone drove.
#
# WARDYN_UP_PORT, not WARDYN_CI_UP_PORT: ci-run.sh defaults it to 0 (an
# OS-assigned ephemeral port, right for a real CI host, useless for a browser
# beat) and compose binds 127.0.0.1:${WARDYN_UP_PORT}:8080.
say "A pipeline also needs to be able to start from scratch."
say "So one job can stand up a temporary Wardyn environment just for the build."
say "When the job is done, it goes away."
say "If you already have Wardyn running somewhere, the CLI can talk to that instance instead."
say "This is a normal shell command."
say "No agent."
say "No key."
say "Just a governed task."
say "And an agent job follows the same model."
# Round-2 dialog review: "The pipeline supplies the secret." was false to the
# lane — the pipeline passes a NAME/grant and Wardyn's proxy holds the value.
# The plan's replacement pair lands as a REPLACEMENT of the next line too: its
# second half ("Wardyn injects it at the boundary") already existed here almost
# verbatim, so adding it would have said the same thing twice and spent an
# extra say beat. Three says in, three says out — the typist's budget for this
# stretch is unchanged, and the trio still reads pipeline → Wardyn → workload.
say "The pipeline names the secret the run is allowed to use."
say "Wardyn injects it at the boundary — the pipeline never handles the value."
say "The workload doesn't have to carry the credential itself."

# SPOKEN IN FRONT OF THE COMMAND, NOT BEHIND IT. type_cmd blocks for the whole
# invocation, so nothing can be narrated while it runs, and this lane has no
# fast-forward to compress the gap with (record-demo.sh skips ffwd on any take
# carrying a terminal segment — the picture publishes in real time). Two of the
# stretches on camera are genuinely silent: ci-run.sh's health poll prints
# NOTHING while it waits for wardynd (up to 90s), and waitForRun prints once
# when the wait opens and once when it closes (cmd/wardyn/commands.go:410,434)
# with nothing in between. The lines that warn a blocking wait is coming
# therefore belong in FRONT of it, setting expectation — behind it, they explain
# in the past tense a silence the viewer already sat through. The task stays the
# fastest thing that can still prove the point (one `echo`) for the same reason:
# everything after "Launching governed run" is dead air that ships as-is.
say "The CI command waits for the run to reach a final state."
say "Then that state becomes the pipeline's result."

type_cmd "WARDYN_CI_TASK='${CI_TASK}' WARDYN_CI_TASK_MODE=exec WARDYN_CI_IMAGE=${CI_IMAGE} WARDYN_CI_SKIP_BUILD=1 WARDYN_CI_KEEP=1 WARDYN_CI_PROJECT=${CI_PROJECT} WARDYN_UP_PORT=${CI_PORT} scripts/ci-run.sh"
# Captured BEFORE anything else touches TYPIST_RC, because beat 3 films this
# exact number and the script's own verdict at the bottom depends on it.
CI_RC="${TYPIST_RC}"

# ── B3 · The exit code ──────────────────────────────────────────────────────
# type_cmd restores $? before its eval, so this prints the pipeline's status and
# not the status of the typing loop. Single-quoted: `$?` must reach the shell
# being filmed, not be expanded by this one.
type_cmd 'echo $?'
say "This one completed successfully."
say "Zero."

# ── B3b · The red build ──────────────────────────────────────────────────────
# The proof-of-thesis beat: nothing bad has been stopped on camera yet. A
# second, THROWAWAY control plane — ci-run.sh always tears down and rebuilds
# its OWN stack at start (`"${COMPOSE[@]}" down --volumes` runs unconditionally,
# before any KEEP check), so reusing B2's project by name would destroy the
# very stack beats 5-6 are about to film. Distinct project/port/out-dir: this
# run's artifacts must never land in ${ART_DIR}, which beat 4 and the browser
# half both read as B2's run.
say "Now let's give it a destination the policy doesn't allow."
type_cmd "WARDYN_CI_TASK='${CI_RED_TASK}' WARDYN_CI_TASK_MODE=exec WARDYN_CI_IMAGE=${CI_IMAGE} WARDYN_CI_SKIP_BUILD=1 WARDYN_CI_PROJECT=${CI_RED_PROJECT} WARDYN_UP_PORT=${CI_RED_PORT} WARDYN_CI_OUT=${CI_RED_OUT} scripts/ci-run.sh"
type_cmd 'echo $?'
CI_RED_RC="${TYPIST_RC}"
say "No reviewer."
say "No approval screen."
say "No waiting."
say "The policy makes the decision immediately."
say "The build goes red."

# ── B4 · Receipts ───────────────────────────────────────────────────────────
type_cmd "ls ${ART_DIR}/"
say "And the pipeline gets artifacts back."
say "The run."
say "The log."
say "And the audit trail."
type_cmd "jq .state ${ART_DIR}/run.json"
say "The run says completed."
type_cmd "jq '.[-3:]' ${ART_DIR}/audit.json"
say "And the final audit entries show the same lifecycle we've seen in the browser:"
say "execution,"
say "network decision,"
say "completion."
say "The same governance."
say "Just without a person sitting in front of it."

narration_end

# ── the take's own honesty gate ─────────────────────────────────────────────
#
# Everything above NARRATES success. Silent on a good take, loud on a bad one —
# because "exit 0" has been the least reliable signal on this project three
# times running, and a green take that speaks "State, completed" over a FAILED
# run is invisible until a human watches the whole thing.
#
# The port check is the one that matters most: beats 5-6 are a browser pointed
# at this stack, so a KEEP that did not hold, an image that never came up, or a
# token the console will not accept is named HERE, in one line, instead of as a
# blank console two minutes later.
#
# NOTE: a non-zero exit here does NOT stop the browser lane. record-demo.sh
# captures TERM_RC, logs "this take is incomplete", and runs the driver anyway
# (it only folds TERM_RC into the final exit code). The spec's own
# pipelineRun() re-reads run.json and refuses a non-COMPLETED state for exactly
# that reason — these two gates are belt and braces, not one gate twice.
[[ "${CI_RC}" -eq 0 ]] \
  || fail "ci-run.sh exited ${CI_RC}; beat 3 films that number and beat 4 calls it completed."
[[ -s "${ART_DIR}/run.json" && -s "${ART_DIR}/run.log" && -s "${ART_DIR}/audit.json" ]] \
  || fail "beat 4 says 'three files' — ${ART_DIR}/ does not have all three."

if command -v jq >/dev/null 2>&1; then
  _state="$(jq -r '.state // ""' "${ART_DIR}/run.json")"
  [[ "${_state}" == "COMPLETED" ]] || fail "run.json state is '${_state}', not COMPLETED."
  _id="$(jq -r '.id // ""' "${ART_DIR}/run.json")"
  [[ -n "${_id}" ]] || fail "run.json carries no run id — the browser half reads it from there."
  jq -e 'any(.[]; .action == "run.complete")' "${ART_DIR}/audit.json" >/dev/null 2>&1 \
    || fail "audit.json has no run.complete event; beat 6 narrates 'create through complete'."

  # The stack the browser half will film, proved with the credential it will
  # use. Same header the console sends (lib/api/core.ts's wfetch).
  #
  # Captured into a variable rather than piped into `grep -q`: under pipefail,
  # grep -q exits on its first match, SIGPIPEs curl, and the pipeline reports
  # failure on the happy path. (record-demo.sh's own ffmpeg probe learned this
  # the same way.)
  _listed="$(curl -fsS --max-time 15 -H "Authorization: Bearer ${ADMIN_TOKEN}" \
    "http://127.0.0.1:${CI_PORT}/api/v1/runs" 2>/dev/null)"
  [[ "${_listed}" == *"${_id}"* ]] \
    || fail "run ${_id} is not listed at http://127.0.0.1:${CI_PORT} with this admin token — WARDYN_CI_KEEP did not hold the stack, or the token differs from the one the spec seeds."
fi

# The red build (B3b) must actually have gone red: the opposite of the checks
# above, kept separate so a broken take names WHICH run it is unhappy about.
[[ "${CI_RED_RC}" -ne 0 ]] \
  || fail "beat 3b says 'the build goes red' but the second ci-run.sh (WARDYN_CI_OUT=${CI_RED_OUT}) exited 0."
if command -v jq >/dev/null 2>&1 && [[ -s "${CI_RED_OUT}/run.json" ]]; then
  _red_state="$(jq -r '.state // ""' "${CI_RED_OUT}/run.json")"
  [[ "${_red_state}" == "FAILED" ]] \
    || fail "${CI_RED_OUT}/run.json state is '${_red_state}', not FAILED — beat 3b narrates a refused run."
fi

exit 0
