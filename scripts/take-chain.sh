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

FF="${WARDYN_DEMO_FFMPEG:-$(command -v ffmpeg.exe 2>/dev/null || ls /mnt/c/Users/*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe 2>/dev/null | head -1)}"
[[ -n "${FF}" ]] || { echo "no Windows ffmpeg — winget.exe install Gyan.FFmpeg (or set WARDYN_DEMO_FFMPEG)" >&2; exit 1; }

# Where record-demo.sh puts the finished mp4 — same rule, so the chain can find
# what it just shot and hand it to the verifier.
OUT_DIR="${WARDYN_DEMO_OUT_DIR:-}"
if [[ -z "${OUT_DIR}" ]]; then
  WIN_VIDEOS="$(powershell.exe -NoProfile -Command '[Environment]::GetFolderPath("MyVideos")' 2>/dev/null | tr -d '\r')"
  [[ -n "${WIN_VIDEOS}" ]] && OUT_DIR="$(wslpath -u "${WIN_VIDEOS}" 2>/dev/null)"
fi
OUT_DIR="${OUT_DIR:-${REPO_ROOT}/local/demo}"

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

ledger() {  # <record rc> <verify> <artifact>
  [[ -s "${LEDGER}" ]] || printf '# Takes ledger\n\n## Attempt log (appended by scripts/take-chain.sh)\n\n| when (UTC) | id | attempt | record rc | verify | artifact |\n|---|---|---|---|---|---|\n' > "${LEDGER}"
  printf '| %s | %s | %s | %s | %s | %s |\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${VIDEO}" "${attempt}" "$1" "$2" "${3:-—}" >> "${LEDGER}"
}

attempt=0
while :; do
  attempt=$((attempt + 1))
  echo "=== take ${VIDEO} attempt ${attempt}: waiting for a live interop socket $(date +%T) ==="
  live="$(wait_socket)" || { echo "NO_SOCKET after ${SOCKET_TRIES} tries — wsl --shutdown is the owner's fix"; ledger "no-socket" "not run"; exit 1; }
  echo "=== take ${VIDEO} attempt ${attempt} $(date +%T) socket=${live} ==="

  WSL_INTEROP="${live}" "${REPO_ROOT}/scripts/record-demo.sh" --video "${VIDEO}" "${REC_ARGS[@]}"
  rc=$?
  echo "TAKE_RC=${rc} video=${VIDEO} attempt=${attempt}"

  # The newest take of THIS id — record-demo.sh names it wardyn-<id>-<slug>-<stamp>.mp4.
  MP4="$(ls -t "${OUT_DIR}"/wardyn-"${VIDEO}"-*.mp4 2>/dev/null | head -1)"

  if [[ "${rc}" -eq 0 ]]; then
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
