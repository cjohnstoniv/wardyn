#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Records the Wardyn demo video end to end: virgin host -> `make setup` on
# camera -> the Getting Started funnel -> one real governed run -> the audit
# trail. Re-run it after a UI change and you have a fresh video; that is the
# whole point of it existing.
#
#   make record-demo                 # the works
#   scripts/record-demo.sh --no-reset    # keep the current stack (iterate on the driver)
#   scripts/record-demo.sh --no-record   # drive the UI with no capture (preflight)
#   scripts/record-demo.sh --with-terminal  # ALSO film `make setup` off the screen
#                                          # (clear the top-left corner first!)
#   scripts/record-demo.sh --silent      # no narration (captions still render)
#
# BY DEFAULT the video is the console only, recorded by the browser ITSELF
# (Playwright recordVideo) — no screen grab, so nothing you do on this machine
# while it runs can end up in the take. Use the machine freely.
#
# --with-terminal additionally films `make setup` off the screen and joins it on
# the front. That segment IS a screen grab of a fixed rectangle and has twice
# captured whatever the operator was doing instead; clear the corner first.
#
# Playwright's clicks are synthetic and never move the OS pointer, so the driver
# paints its own ring and captions — see ui/e2e/demo/overlay.ts. Those captions
# are also SPOKEN, by a local neural TTS (scripts/narrate-server.py, Kokoro-82M,
# offline); --silent turns that off.
#
# Prereqs, checked before anything destructive happens:
#   winget.exe install Gyan.FFmpeg          (from WSL it needs the .exe)
#   claude setup-token > ~/.wardyn-demo-token

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}" || exit 1

# One daemon everywhere — same reason screenshots.sh does this: a box running
# both Docker Desktop and a native dockerd will otherwise film the wrong one.
. "${REPO_ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

DO_RESET=1
DO_RECORD=1
# Filming the terminal means grabbing a REGION OF YOUR SCREEN, and on this host
# that cannot be made safe: WSLg windows can't be raised or z-ordered from
# Linux, so whatever you do on that monitor during the ~2 minutes of `make
# setup` lands in the video instead. It has eaten two takes. Off by default;
# the console acts record themselves and are never at risk.
DO_TERMINAL=0
# Narration: the captions already on screen, spoken by a local neural TTS
# (scripts/narrate-server.py). Renders offline, cached by content hash, and
# degrades to a silent take if the renderer is missing — so it is safe on.
DO_VOICE=1
for arg in "$@"; do
  case "$arg" in
    --no-reset)      DO_RESET=0 ;;
    --no-record)     DO_RECORD=0 ;;
    --with-terminal) DO_TERMINAL=1 ;;
    --silent)        DO_VOICE=0 ;;
    -h|--help)   sed -n '4,28p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[1;35m[record-demo]\033[0m %s\n' "$*"; }
step() { printf '\n\033[1;35m═══ %s\033[0m\n\n' "$*"; }
die()  { printf '\033[1;31m[record-demo] %s\033[0m\n' "$*" >&2; exit 1; }

# --- config ----------------------------------------------------------------

# The workspace the demo run attaches. Materialized as a COPY so the agent's
# edits never land in this repo, and git-init'd so the run's Files surface has a
# real diff to show.
WORKSPACES_ROOT="${WARDYN_DEMO_ROOT:-${HOME}/wardyn-demo}"
WORKSPACE_PATH="${WARDYN_DEMO_WORKSPACE:-${WORKSPACES_ROOT}/slugify}"
FIXTURE="${REPO_ROOT}/examples/workspaces/demo-node"

TOKEN_FILE="${WARDYN_DEMO_TOKEN_FILE:-${HOME}/.wardyn-demo-token}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

# Capture geometry, ImageMagick-style WxH+X+Y. Defaults to a 1080p window in the
# top-left corner, which is where the driver puts the browser
# (--window-position=0,0 --window-size=1920,1080 in playwright.config.ts).
#
# Grabbing the WHOLE desktop is the wrong default on this class of machine: an
# ultrawide or dual-monitor setup photographs at something like 5120x1440 (a
# 3.5:1 video nobody can share) and gdigrab cannot keep up with the pixel rate —
# measured 16fps of a requested 30. Cropping to 1080p fixes both at once.
# WARDYN_DEMO_CAPTURE=full grabs everything anyway.
CAPTURE="${WARDYN_DEMO_CAPTURE:-1920x1080+0+0}"
FRAMERATE="${WARDYN_DEMO_FRAMERATE:-30}"

# --- preflight (before anything destructive) --------------------------------

step "Preflight"

command -v docker >/dev/null || die "docker not found"
[[ -d "${FIXTURE}" ]] || die "missing workspace fixture: ${FIXTURE}"

if [[ -z "${DEMO_CDP:-}" && -z "${DISPLAY:-}" ]]; then
  die "no DISPLAY — the driver needs a headed browser. Under WSL that means WSLg; or set DEMO_CDP to attach to a Chrome with remote debugging."
fi

# gdigrab is a Windows capture device, so the encoder has to be the Windows
# ffmpeg. winget's Gyan.FFmpeg is a zip package: it appends to the WINDOWS PATH,
# and a WSL shell only inherits that at startup — so a freshly-installed ffmpeg
# stays invisible here until you open a new shell. Resolve it directly rather
# than making that the operator's problem.
resolve_ffmpeg() {
  if [[ -n "${WARDYN_DEMO_FFMPEG:-}" ]]; then printf '%s' "${WARDYN_DEMO_FFMPEG}"; return; fi
  if command -v ffmpeg.exe >/dev/null 2>&1; then command -v ffmpeg.exe; return; fi
  local p
  for p in /mnt/c/Users/*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe; do
    [[ -f "${p}" ]] && { printf '%s' "${p}"; return; }
  done
}

FFMPEG=""
if [[ "${DO_RECORD}" == 1 ]]; then  # needed for the join/transcode either way
  FFMPEG="$(resolve_ffmpeg)"
  [[ -n "${FFMPEG}" ]] \
    || die "no Windows ffmpeg found. Install it once with:  winget.exe install Gyan.FFmpeg   (or point WARDYN_DEMO_FFMPEG at ffmpeg.exe)"
  # gdigrab and libx264 are both build options — a stripped ffmpeg would fail
  # mid-take instead of here. (Playwright's own bundled ffmpeg is exactly that:
  # --disable-everything, no x11grab, no gdigrab.)
  #
  # Captured into variables rather than piped into `grep -q`: under `set -o
  # pipefail`, grep -q exits on its first match, SIGPIPEs ffmpeg mid-output, and
  # the pipeline reports failure even though the capability is present. The
  # short -devices list usually finished before grep bailed; the long -encoders
  # list did not, so this "no libx264" every time on a build that has it.
  _ff_devices="$("${FFMPEG}" -hide_banner -devices 2>&1)"
  _ff_encoders="$("${FFMPEG}" -hide_banner -encoders 2>&1)"
  case "${_ff_devices}" in
    *gdigrab*) ;;
    *) die "${FFMPEG} has no gdigrab — it cannot capture a Windows desktop" ;;
  esac
  case "${_ff_encoders}" in
    *" libx264"*) ;;
    *) die "${FFMPEG} has no libx264 encoder" ;;
  esac
fi

# Model access. The token is read from a file and piped, so it is never an
# argument, never in the environment of a child we do not control, and never
# rendered on camera.
if [[ -z "${WARDYN_SUBSCRIPTION_TOKEN:-}" && ! -s "${TOKEN_FILE}" ]]; then
  die "no Claude subscription token. Run:  claude setup-token > ${TOKEN_FILE}   (or export WARDYN_SUBSCRIPTION_TOKEN)"
fi

# Output path: prefer the real Windows Videos folder — encoding 1080p30 onto the
# \\wsl$ 9p share drops frames.
OUT_DIR="${WARDYN_DEMO_OUT_DIR:-}"
if [[ -z "${OUT_DIR}" ]]; then
  WIN_VIDEOS="$(powershell.exe -NoProfile -Command '[Environment]::GetFolderPath("MyVideos")' 2>/dev/null | tr -d '\r')"
  if [[ -n "${WIN_VIDEOS}" ]]; then
    OUT_DIR="$(wslpath -u "${WIN_VIDEOS}" 2>/dev/null)"
  fi
fi
OUT_DIR="${OUT_DIR:-${REPO_ROOT}/local/demo}"
mkdir -p "${OUT_DIR}" || die "cannot create output dir: ${OUT_DIR}"
OUT="${OUT_DIR}/wardyn-demo-${STAMP}.mp4"

log "workspace   ${WORKSPACE_PATH}"
log "video       ${OUT}"
log "browser     ${DEMO_CDP:+CDP → ${DEMO_CDP}}${DEMO_CDP:-WSLg Chromium (DISPLAY=${DISPLAY:-})}"
log "ffmpeg      ${FFMPEG:-(not recording)}"
log "preflight OK"

# --- workspace --------------------------------------------------------------

step "Materializing the demo workspace"

# Rebuilt from the fixture every time: a previous recording left the agent's
# slugify() in it, and the video is about adding that function.
rm -rf "${WORKSPACE_PATH}"
mkdir -p "$(dirname "${WORKSPACE_PATH}")"
cp -r "${FIXTURE}" "${WORKSPACE_PATH}" || die "could not copy fixture"
rm -f "${WORKSPACE_PATH}/TASK.md"   # director's notes, not part of the project on camera
(
  cd "${WORKSPACE_PATH}" || exit 1
  git init -q .
  git add -A
  # Local identity only — never touches the operator's global git config.
  git -c user.email=demo@wardyn.local -c user.name="Wardyn Demo" commit -qm "slugify demo workspace"
) || die "could not git init the workspace"
log "seeded $(find "${WORKSPACE_PATH}" -type f -not -path '*/.git/*' | wc -l) files at ${WORKSPACE_PATH}"

# --- capture ----------------------------------------------------------------

FFPID=""
FIFO=""
stop_capture() {
  [[ -n "${FFPID}" ]] || return 0
  # ffmpeg finalizes the container on 'q'. Signalling a Windows binary through
  # the interop layer is unreliable, so drive its stdin instead.
  printf 'q' >&9 2>/dev/null
  exec 9>&- 2>/dev/null
  wait "${FFPID}" 2>/dev/null
  FFPID=""
  [[ -n "${FIFO}" ]] && rm -f "${FIFO}"
}
trap stop_capture EXIT INT TERM

if [[ "${DO_RECORD}" == 1 && "${DO_TERMINAL}" == 1 ]]; then
  step "Rolling (screen grab — keep the capture region clear)"
  # WxH+X+Y -> gdigrab's -video_size / -offset_x / -offset_y.
  GEOM=()
  if [[ "${CAPTURE}" != "full" ]]; then
    if [[ "${CAPTURE}" =~ ^([0-9]+x[0-9]+)\+([0-9]+)\+([0-9]+)$ ]]; then
      GEOM=(-video_size "${BASH_REMATCH[1]}" -offset_x "${BASH_REMATCH[2]}" -offset_y "${BASH_REMATCH[3]}")
    else
      die "WARDYN_DEMO_CAPTURE must look like 1920x1080+0+0, or be 'full' (got: ${CAPTURE})"
    fi
  fi
  log "capture region ${CAPTURE} @ ${FRAMERATE}fps"

  FIFO="$(mktemp -u)"; mkfifo "${FIFO}"
  "${FFMPEG}" -hide_banner -loglevel error -y \
    -f gdigrab -framerate "${FRAMERATE}" -draw_mouse 1 "${GEOM[@]}" -i desktop \
    -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p \
    "$(wslpath -w "${OUT}")" < "${FIFO}" &
  FFPID=$!
  exec 9>"${FIFO}"
  sleep 2
  kill -0 "${FFPID}" 2>/dev/null || die "ffmpeg exited immediately — check that gdigrab can see the desktop"
  log "capturing desktop → ${OUT}"
fi

# --- act 0: cold start ------------------------------------------------------

if [[ "${DO_RESET}" == 1 ]]; then
  step "Act 0 · Clean slate"
  log "wiping the stack (volumes, sandboxes, the wardyn-internal network, compose .env)"
  WARDYN_FORCE_RESET=1 WARDYN_FORCE_STOP_HOST=1 ./scripts/up.sh reset-all --purge-env \
    || die "reset-all failed"
  # Deliberately NOT --purge-images: rebuilding the agent images is minutes of
  # dead air, and they are not what the video is about.
fi

step "Act 0 · make setup"
log "mode: containerized (WARDYN_SETUP_MODE=container)"
log "workspaces root: ${WORKSPACES_ROOT}"
# WARDYN_UP_NO_BROWSER: up.sh otherwise fires wslview and an uncontrolled window
# lands in frame. The driver opens the browser itself, at the moment it wants it.
WARDYN_SETUP_MODE=container \
WARDYN_WORKSPACES_ROOT="${WORKSPACES_ROOT}" \
WARDYN_UP_NO_BROWSER=1 \
  make setup || die "make setup failed"

step "Act 0 · Model access"
log "connecting the Claude subscription (token piped from a file — never printed, never an argument)"
if [[ -n "${WARDYN_SUBSCRIPTION_TOKEN:-}" ]]; then
  printf '%s' "${WARDYN_SUBSCRIPTION_TOKEN}" | ./wardyn subscription connect --token-stdin \
    || die "subscription connect failed"
else
  ./wardyn subscription connect --token-stdin < "${TOKEN_FILE}" \
    || die "subscription connect failed"
fi
./wardyn setup status || true

# --- acts 1-6: the driver ---------------------------------------------------

# Stop the desktop grab HERE. Act 0 is a terminal and has to be filmed off the
# screen, but the browser records itself from the inside (playwright.config's
# demo project), which is the only capture on this host that cannot be ruined
# by another window sitting on top of the frame.
stop_capture

# Warm the narration cache first: an unrendered line otherwise renders INLINE
# during the take, leaving the caption on screen in silence for ~1.5s — dead air
# in the finished video, once per line.
if [[ "${DO_VOICE}" == 1 ]]; then
  step "Pre-rendering narration"
  # DELETE ANY PRIOR TIMELINE FIRST. narrator.ts only writes this file after its
  # first SUCCESSFUL cue, and it degrades silently when the renderer is missing —
  # so a stale timeline from an earlier take would survive a fully silent run and
  # get muxed onto tonight's picture at yesterday's offsets. Worse than silent.
  rm -f "${REPO_ROOT}/ui/test-results/demo-video/narration.json"
  # Preflight the renderer for the same reason: narration is on by default, and a
  # broken venv otherwise yields a silent take nobody notices until the morning.
  if ! "${PY_NARRATE:-${HOME}/.cache/wardyn-narrate/venv/bin/python}" \
        "${REPO_ROOT}/scripts/narrate-server.py" --selftest >/dev/null 2>&1; then
    die "narration renderer is broken — fix it, or re-run with --silent to record without a voice"
  fi
  "${REPO_ROOT}/scripts/narrate-prewarm.sh" || log "prewarm failed — lines will render inline instead"
fi

step "Acts 1-6 · Driving the console"
(
  cd "${REPO_ROOT}/ui" || exit 1
  # The driver needs the capture rect too: it moves the browser window into the
  # frame via CDP and REFUSES to run if the window lands outside it. Without
  # that the recording happily films whatever else is in that screen corner.
  WARDYN_DEMO=1 \
  WARDYN_DEMO_WORKSPACE="${WORKSPACE_PATH}" \
  WARDYN_DEMO_CAPTURE="${CAPTURE}" \
  WARDYN_DEMO_VOICE="${DO_VOICE}" \
    pnpm exec playwright test --project=demo --workers=1 --reporter=line
)
DRIVER_RC=$?

# --- wrap -------------------------------------------------------------------

stop_capture
trap - EXIT INT TERM

# --- assemble the final video -----------------------------------------------
#
# The console segment is the browser's own recording and is always correct. The
# terminal segment is a screen grab and only exists with --with-terminal; when
# it does, the two are concatenated (re-encoded to a common 1920x1080/30, since
# they come from different sources).
BROWSER_VID="${REPO_ROOT}/ui/test-results/demo-video/console.webm"
if [[ ! -s "${BROWSER_VID}" ]]; then
  BROWSER_VID="$(ls -t "${REPO_ROOT}"/ui/test-results/demo-video/*.webm 2>/dev/null | head -1)"
fi
FINAL=""
V="[0:v]scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:-1:-1,setsar=1,fps=30"

if [[ "${DO_RECORD}" == 1 && -n "${BROWSER_VID}" && -s "${BROWSER_VID}" ]]; then
  if [[ "${DO_TERMINAL}" == 1 && -s "${OUT}" ]]; then
    step "Joining terminal + console segments"
    FINAL="${OUT%.mp4}-full.mp4"
    "${FFMPEG}" -hide_banner -loglevel error -y \
      -i "$(wslpath -w "${OUT}")" -i "$(wslpath -w "${BROWSER_VID}")" \
      -filter_complex "${V}[a];[1:v]scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:-1:-1,setsar=1,fps=30[b];[a][b]concat=n=2:v=1[v]" \
      -map "[v]" -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p \
      "$(wslpath -w "${FINAL}")" || { log "join failed — segments are still usable separately"; FINAL=""; }
  else
    step "Encoding the console recording"
    FINAL="${OUT}"
    "${FFMPEG}" -hide_banner -loglevel error -y -i "$(wslpath -w "${BROWSER_VID}")" \
      -vf "fps=30,setsar=1" -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p \
      "$(wslpath -w "${FINAL}")" || { log "encode failed — the raw webm is still there"; FINAL=""; }
  fi
fi

# --- narration ---------------------------------------------------------------
#
# Runs LAST and on the finished mp4, for two reasons: the raw console recording
# is VP8, which cannot be stream-copied into an mp4 container; and muxing here
# means the picture is encoded exactly once, so narration cannot soften the text.
TIMELINE="${REPO_ROOT}/ui/test-results/demo-video/narration.json"
if [[ "${DO_VOICE}" == 1 && -n "${FINAL}" && -s "${FINAL}" && -s "${TIMELINE}" ]]; then
  step "Adding narration"
  # With --with-terminal the console segment is concatenated AFTER Act 0, so
  # every cue is late by exactly Act 0's duration.
  OFFSET=0
  if [[ "${DO_TERMINAL}" == 1 && -s "${OUT}" ]]; then
    OFFSET="$(python3 -c "
import subprocess,sys
out=subprocess.run(['${FFMPEG}','-hide_banner','-i',r'$(wslpath -w "${OUT}")'],capture_output=True,text=True).stderr
for l in out.splitlines():
    if 'Duration:' in l:
        h,m,rest=l.split('Duration:')[1].split(',')[0].strip().split(':')
        print(int((int(h)*3600+int(m)*60+float(rest))*1000)); break
else: print(0)
" 2>/dev/null || echo 0)"
    log "console segment starts at ${OFFSET}ms — shifting cues"
  fi
  NARRATED="${FINAL%.mp4}-narrated.mp4"
  if python3 "${REPO_ROOT}/scripts/narrate-mux.py" --ffmpeg "${FFMPEG}" \
      --video "${FINAL}" --timeline "${TIMELINE}" --out "${NARRATED}" --offset-ms "${OFFSET}"; then
    FINAL="${NARRATED}"
  else
    log "narration mux failed — the silent video above is still good"
  fi
fi

# VERIFY THE TAKE — unattended, "exit 0" is not evidence. It has been wrong
# three times on this project (a run that never ran, a workspace never written,
# an approval granted to the wrong host), so the take is checked against the
# audit trail and the filesystem before anyone is told it succeeded.
if [[ -n "${FINAL:-}" && -s "${FINAL:-/nonexistent}" ]]; then
  step "Verifying the take"
  WARDYN_DEMO_SHELL_ACT5="${WARDYN_DEMO_SHELL_ACT5:-}" "${REPO_ROOT}/scripts/verify-demo-take.sh" "${FINAL}" || log "VERIFICATION FAILED — read the checks above before publishing this take"
fi

step "Done"
if [[ -n "${FINAL}" && -s "${FINAL}" ]]; then
  log "VIDEO: ${FINAL}  ($(du -h "${FINAL}" | cut -f1))"
elif [[ "${DO_RECORD}" == 1 ]]; then
  log "no final video assembled — check the segments above"
fi
if [[ -n "${BROWSER_VID}" && -s "${BROWSER_VID}" ]]; then
  log "  console segment (browser-recorded, always correct): ${BROWSER_VID}"
fi
if [[ "${DO_TERMINAL}" == 1 && -s "${OUT}" ]]; then
  log "  terminal segment (screen grab): ${OUT}"
fi
if [[ "${DRIVER_RC}" -ne 0 ]]; then
  log "driver exited ${DRIVER_RC} — the recording is incomplete. Playwright's report:"
  log "  ui/../test/reports/e2e/playwright-report/index.html"
fi
exit "${DRIVER_RC}"
