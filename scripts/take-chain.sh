#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# One series take, end to end: wait for a WSL interop socket that can actually
# grab the screen, record, verify, and write a ledger line for every attempt.
#
#   scripts/take-chain.sh --video 03a
#   scripts/take-chain.sh --video 02 --reset
#   scripts/take-chain.sh --video 13 --terminal-script scripts/demo-beats/13-terminal-to-the-cluster.sh
#
# WHY THE PROBE. Every take is a gdigrab of the Windows desktop through the WSL
# interop socket, and those sockets FLAP per-exec: /run/WSL/*_interop fills up
# with dead ones, a .exe launched through a dead one fails with accept4-110, and
# nothing weaker than running a real Windows binary tells them apart (a `cmd.exe
# echo` passes on sockets ffmpeg then dies on). So each attempt probes with the
# ffmpeg the take will use, asking for the device it needs, and exports the
# socket that answered — the same chain that got episode 03 recorded on a night
# when four of five sockets were dead.
#
# WHY THE EXIT CODE IS NOT THE ANSWER. record-demo.sh has exited 0 over a run
# that never ran. The take is graded by scripts/verify-demo-take.sh against the
# audit trail, and the ledger records BOTH results per attempt.
#
# It does NOT set the stack's env (SSH gateway, agent images): that belongs in
# deploy/compose/.env — passing it here is inert on a stack that is already up.

set -uo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}" || exit 1
# shellcheck source=lib/common.sh
. "${REPO_ROOT}/scripts/lib/common.sh"   # wardyn_ffmpeg

VIDEO=""
TERMINAL_SCRIPT=""
REC_ARGS=()
ATTEMPTS="${WARDYN_TAKE_ATTEMPTS:-3}"
# 720 × 10s = two hours of flapping before giving up, which is what the original
# chain waited through on a bad night.
SOCKET_TRIES="${WARDYN_TAKE_SOCKET_TRIES:-720}"
LEDGER="${WARDYN_TAKES_LEDGER:-${REPO_ROOT}/local/TAKES-LEDGER.md}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --video)           VIDEO="${2:-}"; shift ;;
    --video=*)         VIDEO="${1#*=}" ;;
    --terminal-script) TERMINAL_SCRIPT="${2:-}"; shift ;;
    --terminal-script=*) TERMINAL_SCRIPT="${1#*=}" ;;
    --reset)           REC_ARGS+=(--reset) ;;
    -h|--help)         sed -n '5,/^set -uo/p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done
[[ -n "${VIDEO}" ]] || { echo "usage: scripts/take-chain.sh --video <id> [--terminal-script <path>] [--reset]" >&2; exit 2; }
[[ -n "${TERMINAL_SCRIPT}" ]] && REC_ARGS+=(--terminal-script "${TERMINAL_SCRIPT}")

# The take's OWN ffmpeg — the probe below has to ask the exact binary
# record-demo.sh will encode with, or a socket it clears is one that binary dies
# on. Same resolver, one place: wardyn_ffmpeg in scripts/lib/common.sh.
FF="$(wardyn_ffmpeg)"
[[ -n "${FF}" ]] || { echo "no Windows ffmpeg — winget.exe install Gyan.FFmpeg (or set WARDYN_DEMO_FFMPEG)" >&2; exit 1; }

# Where the finished mp4 lands — resolved INSIDE the loop, through the live
# socket (the parent shell's is the dead one on the nights this chain exists
# for), and handed down to record-demo.sh so the two cannot disagree.
OUT_DIR="${WARDYN_DEMO_OUT_DIR:-}"

# A socket that runs a real Windows binary AND reports the capture device.
probe() {
  local s out
  for s in $(ls -t /run/WSL/*_interop 2>/dev/null); do
    out="$(WSL_INTEROP="${s}" timeout 10 "${FF}" -hide_banner -devices 2>&1)"
    case "${out}" in *gdigrab*) printf '%s' "${s}"; return 0 ;; esac
  done
  return 1
}

wait_socket() {
  local i live
  for ((i = 0; i < SOCKET_TRIES; i++)); do
    live="$(probe)" && { printf '%s' "${live}"; return 0; }
    sleep 10
  done
  return 1
}

# Keep the take gradeable: record-demo.sh's per-id work dir (narration timeline,
# drift fit, speedups, run ids) is wiped by the NEXT take of any id, which is how
# the 13 staged 0.7 cuts ended up with ledger rows as their only evidence. Park
# a copy beside the mp4 so verify-demo-take.sh can re-grade it later.
archive_artifacts() {  # <mp4 path>
  local work="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-${VIDEO}}" dst
  dst="${1%.mp4}.artifacts"
  mkdir -p "${dst}" || return 0
  local f
  # v13-run-id.txt is 13's handoff (verify-demo-take-13.sh reads it out of this
  # same work dir) — it was being wiped with the rest of the dir by the next take.
  for f in narration.json narration-ffwd.json narration-joined.json narration-terminal.json speedups.json demo-runs.json drift-fit.json v13-run-id.txt; do
    [[ -s "${work}/${f}" ]] && cp -f "${work}/${f}" "${dst}/" 2>/dev/null
  done
  # 12's handoff does NOT live in the per-id work dir: both lanes resolve the one
  # fixed path below, which the next take of any id overwrites. check_video_10
  # reads WARDYN_DEMO_WORK_DIR first, so this copy is what a re-grade with
  # WARDYN_DEMO_WORK_DIR=<take>.artifacts finds. Without it: no run id at all.
  [[ "${VIDEO}" == "12" && -s "${REPO_ROOT}/ui/test-results/demo-video/v10-run-id.txt" ]] &&
    cp -f "${REPO_ROOT}/ui/test-results/demo-video/v10-run-id.txt" "${dst}/" 2>/dev/null
  # An empty archive used to be removed in silence, so "the work dir was already
  # wiped" and "this take had nothing to park" looked identical afterwards.
  if rmdir "${dst}" 2>/dev/null; then
    echo "NO_ARTIFACTS ${work}"
  else
    echo "ARTIFACTS ${dst}"
  fi
}

ledger() {  # <record rc> <verify> <artifact>
  [[ -s "${LEDGER}" ]] || printf '# Takes ledger\n\n## Attempt log (appended by scripts/take-chain.sh)\n\n| when (UTC) | id | attempt | record rc | verify | artifact |\n|---|---|---|---|---|---|\n' > "${LEDGER}"
  printf '| %s | %s | %s | %s | %s | %s |\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${VIDEO}" "${attempt}" "$1" "$2" "${3:-—}" >> "${LEDGER}"
}

attempt=0

# THE LABEL GATE. A spec that asserts a label ui/src no longer renders cannot
# pass verify, but it fails after the socket wait, the record and the encode —
# hours of camera time to learn something a grep knew before we started. Runs
# BEFORE wait_socket for exactly that reason.
#   rc 0 = every asserted label is still in the product (or the episode is
#          terminal-only and has none)
#   rc 1 = REFUSE: the take would film a lie
#   rc 2 = the tool could not judge (unknown id, two specs) — noted, not fatal,
#          because a new episode's lane must still be able to roll.
# ANY OTHER rc is the gate failing to RUN — no python3 (127), a crash — and that
# is "could not judge" too. Only rc 1 is a refusal; treating a broken gate as one
# would ground the camera over a tool, not over the take.
python3 "${REPO_ROOT}/scripts/demo-rerecord-impact.py" check "${VIDEO}"
case $? in
  0) ;;
  1) echo "LABEL_GATE_FAILED ${VIDEO} — the spec asserts labels ui/src no longer has; fix the spec or the app, do not roll"
     ledger "not run" "label-gate"; exit 1 ;;
  2) echo "LABEL_GATE_SKIPPED ${VIDEO} — the label gate could not judge this id; rolling anyway" ;;
  *) echo "LABEL_GATE_SKIPPED ${VIDEO} — gate rc=$? (it could not run); rolling anyway" ;;
esac

while :; do
  attempt=$((attempt + 1))
  echo "=== take ${VIDEO} attempt ${attempt}: waiting for a live interop socket $(date +%T) ==="
  live="$(wait_socket)" || { echo "NO_SOCKET after ${SOCKET_TRIES} tries — wsl --shutdown is the owner's fix"; ledger "no-socket" "not run"; exit 1; }
  echo "=== take ${VIDEO} attempt ${attempt} $(date +%T) socket=${live} ==="

  if [[ -z "${OUT_DIR}" ]]; then
    WIN_VIDEOS="$(WSL_INTEROP="${live}" powershell.exe -NoProfile -Command '[Environment]::GetFolderPath("MyVideos")' 2>/dev/null | tr -d '\r')"
    [[ -n "${WIN_VIDEOS}" ]] && OUT_DIR="$(wslpath -u "${WIN_VIDEOS}" 2>/dev/null)"
    OUT_DIR="${OUT_DIR:-${REPO_ROOT}/local/demo}"
  fi
  t0=$(date +%s)
  WSL_INTEROP="${live}" WARDYN_DEMO_OUT_DIR="${OUT_DIR}" "${REPO_ROOT}/scripts/record-demo.sh" --video "${VIDEO}" "${REC_ARGS[@]}"
  rc=$?
  echo "TAKE_RC=${rc} video=${VIDEO} attempt=${attempt}"

  # The newest take of THIS id — record-demo.sh names it wardyn-<id>-<slug>-<stamp>.mp4 —
  # and it must be NEWER than this attempt, or it is the previous take wearing this id.
  MP4="$(ls -t "${OUT_DIR}"/wardyn-"${VIDEO}"-*.mp4 2>/dev/null | head -1)"
  [[ -n "${MP4}" && "$(stat -c %Y "${MP4}")" -ge "${t0}" ]] || MP4=""
  if [[ "${rc}" -eq 0 && -z "${MP4}" ]]; then
    ledger "${rc}" "FAIL" "—"; echo "NO_MP4 ${VIDEO} — exit 0 but nothing new under ${OUT_DIR}"; break
  fi

  if [[ "${rc}" -eq 0 ]]; then
    archive_artifacts "${MP4}"
    WARDYN_DEMO_VIDEO="${VIDEO}" "${REPO_ROOT}/scripts/verify-demo-take.sh" "${MP4}"
    vrc=$?
    ledger "${rc}" "$([[ "${vrc}" -eq 0 ]] && echo PASS || echo FAIL)" "${MP4##*/}"
    [[ "${vrc}" -eq 0 ]] && { echo "TAKE_OK ${VIDEO} ${MP4}"; break; }
    echo "VERIFY_FAILED ${VIDEO} — the take recorded but does not prove what it claims"
    break   # a verify failure is a CONTENT problem; re-shooting it blind just films it again
  fi

  ledger "${rc}" "not run" "${MP4##*/}"
  # The weekly model limit is unretryable — the next attempt burns a take slot
  # for nothing (episodes 06 and 07 were lost that way).
  if grep -qiE "weekly limit|quota" "${REPO_ROOT}"/ui/test-results/demo-video-"${VIDEO}"/pw/*/error-context.md 2>/dev/null; then
    echo "QUOTA_PARKED=${VIDEO} — stop, this does not retry"; break
  fi
  [[ "${attempt}" -ge "${ATTEMPTS}" ]] && { echo "HALTED=${VIDEO} after ${attempt} attempts"; break; }
  sleep 20
done
echo "TAKE_CHAIN_DONE ${VIDEO} $(date +%T)"
