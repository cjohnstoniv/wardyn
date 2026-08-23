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
#   scripts/record-demo.sh --video 03    # record ONE video of the 0.5 series
#                                        # (ui/e2e/demo/03-*.spec.ts)
#   scripts/record-demo.sh --video 11 --terminal-script scripts/demo-beats/11-ci-and-headless.sh
#                                        # film host-shell beats instead of (or
#                                        # before) the browser ones
#
# THE SERIES. --video <nn> records a single video instead of the whole
# walkthrough: it picks ui/e2e/demo/<nn>-*.spec.ts as the only spec to run and
# names the output after it (wardyn-<nn>-<slug>-<stamp>.mp4). Only video 02 —
# and the no-flag walkthrough, which starts from nothing by definition — wipes
# the stack first. Every other video opens on state an earlier video left
# behind (a workspace, a run, a permanent grant), so a reset there does not
# just cost minutes of dead air, it films the wrong thing. Pass --reset to
# override that, --no-reset to opt 02 out. See docs/DEMO-SCRIPT.md.
#
# BY DEFAULT the video is the console only, recorded by the browser ITSELF
# (Playwright recordVideo) — no screen grab, so nothing you do on this machine
# while it runs can end up in the take. Use the machine freely.
#
# --with-terminal additionally films `make setup` off the screen and joins it on
# the front. That segment IS a screen grab of a fixed rectangle and has twice
# captured whatever the operator was doing instead; clear the corner first.
#
# THE TERMINAL LANE. Three videos of the series have no page to film: V11 (CI &
# headless) is a policy file, a `scripts/ci-run.sh` invocation, its exit code and
# its artifacts; V12 (audit & attach) is three terminals holding an ssh session
# each; V13 (your terminal, our cluster) is kubectl and ssh against a kind
# cluster. --terminal-script <path> runs that script under the SAME gdigrab capture
# Act 0 uses, with scripts/demo-typist.sh giving it say/type_cmd/beat/chapter —
# the terminal's answer to ui/e2e/demo/overlay.ts, narration included. A video
# may be terminal-only (no <nn>-*.spec.ts exists), browser-only, or both; both
# are joined into ONE mp4, terminal first, with the two narration timelines
# merged so the whole thing speaks. Same capture-region discipline applies, for
# as long as the beats run rather than only during Act 0.
#
# Playwright's clicks are synthetic and never move the OS pointer, so the driver
# paints its own ring and captions — see ui/e2e/demo/overlay.ts. Those captions
# are also SPOKEN, by a local neural TTS (scripts/narrate-server.py, Kokoro-82M,
# offline); --silent turns that off.
#
# The stretches that are only the agent working are marked by the spec
# (overlay.ts's ffwdStart/ffwdEnd) and compressed 12x on the way out, with
# every later narration cue slid up to match — WARDYN_DEMO_FFWD_FACTOR
# overrides the 12. Browser-only takes; see scripts/demo-ffwd.py.
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
# Which video of the 0.5 series to record. Empty is the legacy end-to-end
# walkthrough — every spec the demo project matches, which today is the one
# six-act file. Env-overridable like every other knob in this script, and
# exported below because the verifier dispatches on it.
VIDEO="${WARDYN_DEMO_VIDEO:-}"
# A host-shell beat script to film (see THE TERMINAL LANE above). Empty means the
# browser lane alone, which is every video the series has shot so far.
TERMINAL_SCRIPT="${WARDYN_DEMO_TERMINAL_SCRIPT:-}"
# Whether a reset was actually ASKED for. DO_RESET's default is per-video (only
# 02 wipes the stack, see below) and a typed flag has to beat that default in
# both directions — otherwise `--video 05 --reset` would silently not reset.
RESET_EXPLICIT=0
# A while/shift loop, not `for arg in "$@"`: --video takes a value, and the for
# loop cannot consume the next word.
while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-reset)      DO_RESET=0; RESET_EXPLICIT=1 ;;
    --reset)         DO_RESET=1; RESET_EXPLICIT=1 ;;
    --no-record)     DO_RECORD=0 ;;
    --with-terminal) DO_TERMINAL=1 ;;
    --silent)        DO_VOICE=0 ;;
    # The emptiness check is here rather than with the rest of the validation in
    # preflight because an empty VIDEO is indistinguishable from no --video at
    # all — and that path means the whole walkthrough, i.e. reset-all, i.e. a
    # stack wipe nobody asked for.
    --video)         VIDEO="${2:-}"; shift; [[ -n "${VIDEO}" ]] || { echo "--video needs a number, e.g. --video 03" >&2; exit 2; } ;;
    --video=*)       VIDEO="${1#*=}";        [[ -n "${VIDEO}" ]] || { echo "--video needs a number, e.g. --video 03" >&2; exit 2; } ;;
    --terminal-script)   TERMINAL_SCRIPT="${2:-}"; shift; [[ -n "${TERMINAL_SCRIPT}" ]] || { echo "--terminal-script needs a path, e.g. --terminal-script scripts/demo-beats/11-ci-and-headless.sh" >&2; exit 2; } ;;
    --terminal-script=*) TERMINAL_SCRIPT="${1#*=}";        [[ -n "${TERMINAL_SCRIPT}" ]] || { echo "--terminal-script needs a path, e.g. --terminal-script scripts/demo-beats/11-ci-and-headless.sh" >&2; exit 2; } ;;
    # Pattern-bounded rather than a line count: this header grows, and a stale
    # `4,39p` silently truncates --help to something that no longer mentions the
    # flag the reader came for.
    -h|--help)   sed -n '4,/^#   claude setup-token/p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

# The clean slate belongs to video 02 alone (Set up the host — the series'
# from-nothing take). Every later video opens on state an earlier one left
# behind — the workspace it onboarded, the run it launched, the host it
# permanently granted — so wiping the stack before, say, video 04 does not
# merely cost minutes of dead air: it films a story whose first half never
# happened. An explicit --reset/--no-reset always wins.
if [[ -n "${VIDEO}" && "${VIDEO}" != "02" && "${RESET_EXPLICIT}" == 0 ]]; then
  DO_RESET=0
fi

# Video 01 (the primer) is a slides-lane take: a local HTML deck over file://,
# recorded like any console take but touching NO product surface. It needs no
# stack, no workspace, no model token — and it must never gate on (or mutate)
# whatever happens to be answering :8080, which is not necessarily the series
# stack (a quickstart squatting the port with bearer auth killed a rehearsal
# at the subscription-connect step for a video that never uses the model).
STACKLESS=0
[[ "${VIDEO}" == "01" ]] && STACKLESS=1
[[ "${STACKLESS}" == 1 ]] && DO_RESET=0

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

# The beat script is validated HERE for the same reason --video is: a typo, or a
# script with a syntax error in it, must die before reset-all has wiped the stack
# — not two minutes into a take that then films a bash error message.
if [[ -n "${TERMINAL_SCRIPT}" ]]; then
  [[ "${TERMINAL_SCRIPT}" = /* ]] || TERMINAL_SCRIPT="${REPO_ROOT}/${TERMINAL_SCRIPT}"
  [[ -f "${TERMINAL_SCRIPT}" ]] || die "no such terminal beat script: ${TERMINAL_SCRIPT}"
  bash -n "${TERMINAL_SCRIPT}" || die "terminal beat script does not parse: ${TERMINAL_SCRIPT}"
fi

# Which spec the driver runs, and what the file is called. Resolved HERE, in
# preflight, for the reason everything else in this section is: a typo'd --video
# must not be discovered after reset-all has already wiped the stack.
SPEC=""
SLUG=""
# With no --video this is the legacy end-to-end walkthrough, and it is named
# EXPLICITLY rather than left empty. An empty filter means "every spec the demo
# project matches", which was the same thing back when walkthrough.spec.ts was
# the only file in ui/e2e/demo — it is not any more. The ten series specs live
# beside it now, so an empty filter would run all eleven back to back into one
# recording, after a reset-all wiped the state the later ones expect to inherit.
PW_FILTER=("walkthrough.spec.ts")
# Whether the browser lane runs at all. A terminal-only video (V13, and V11/V12
# before their browser halves existed) has no spec to hand Playwright.
RUN_DRIVER=1
if [[ -n "${VIDEO}" ]]; then
  [[ "${VIDEO}" =~ ^[0-9]{2}$ ]] || die "--video takes a two-digit number (01..13), got: ${VIDEO}"
  # No `shopt -s nullglob`: an unmatched glob stays literal and the -f test
  # below rejects it, which is one fewer shell option changed under the rest of
  # this script.
  MATCHES=("${REPO_ROOT}"/ui/e2e/demo/"${VIDEO}"-*.spec.ts)
  if [[ -f "${MATCHES[0]}" ]]; then
    [[ "${#MATCHES[@]}" -eq 1 ]] || die "--video ${VIDEO} matches ${#MATCHES[@]} specs: ${MATCHES[*]}"
    SPEC="${MATCHES[0]##*/}"
    SLUG="${SPEC#"${VIDEO}"-}"; SLUG="${SLUG%.spec.ts}"
    # Playwright takes a filename filter as a bare positional argument.
    PW_FILTER=("${SPEC}")
  elif [[ -n "${TERMINAL_SCRIPT}" ]]; then
    # Terminal-only: the beat script IS the video, and the slug comes off its
    # filename the same way it would have come off the spec's, so the take still
    # lands as wardyn-<nn>-<slug>-<stamp>.mp4 and sorts with its siblings.
    RUN_DRIVER=0
    SLUG="${TERMINAL_SCRIPT##*/}"; SLUG="${SLUG%.sh}"; SLUG="${SLUG#"${VIDEO}"-}"
  else
    die "no spec for --video ${VIDEO} (looked for ui/e2e/demo/${VIDEO}-*.spec.ts). Each series spec lands with its own video; until yours does, record the walkthrough with no --video — or pass --terminal-script if this one is a terminal video."
  fi
fi
# Exported, not just passed: scripts/verify-demo-take.sh dispatches its
# per-video checks on this, and the driver can read it too. Empty means the
# legacy walkthrough, which every consumer already treats as the default.
export WARDYN_DEMO_VIDEO="${VIDEO}"

# Per-video SCRATCH directory — console.webm, the two narration timelines, and
# Playwright's own test-results all land here. Distinct from WARDYN_DEMO_OUT_DIR
# below, which is where the finished mp4 goes.
#
# It is per-video because these paths used to be one shared directory, which was
# correct while only one take could ever be in flight. Recording two videos at
# once then has both contexts writing console.webm and narration.json over each
# other: the second take's picture replaces the first's, the mux staples one
# video's narration onto the other's frames, and BOTH invocations still exit 0.
# Silent, and only visible to whoever eventually watches six minutes of the
# wrong thing. Suffixed by video number so concurrent takes cannot collide, and
# left at the legacy path for the no---video walkthrough so nothing else moves.
DEMO_OUT_DIR="${REPO_ROOT}/ui/test-results/demo-video${VIDEO:+-${VIDEO}}"
mkdir -p "${DEMO_OUT_DIR}"
export WARDYN_DEMO_WORK_DIR="${DEMO_OUT_DIR}"

# DISPLAY gates only the SCREEN-GRAB lanes (--with-terminal / --terminal-script,
# which gdigrab a desktop rectangle). The browser lane records itself HEADLESS
# now — the old headed capture could never be pixel-clean here, because WSLg
# clamps any window taller than the screen and every take wore a gray
# letterbox for it.
if [[ ( "${DO_TERMINAL}" == 1 || -n "${TERMINAL_SCRIPT}" ) && -z "${DISPLAY:-}" ]]; then
  die "no DISPLAY — the terminal lane grabs the desktop. Under WSL that means WSLg."
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
# rendered on camera. A stackless take uses no model at all.
if [[ "${STACKLESS}" == 0 && -z "${WARDYN_SUBSCRIPTION_TOKEN:-}" && ! -s "${TOKEN_FILE}" ]]; then
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
# A series take is named for its video, so a folder of them sorts into viewing
# order instead of into the order someone happened to reshoot them. The whole
# walkthrough keeps the name it has always had.
if [[ -n "${VIDEO}" ]]; then
  OUT="${OUT_DIR}/wardyn-${VIDEO}-${SLUG}-${STAMP}.mp4"
else
  OUT="${OUT_DIR}/wardyn-demo-${STAMP}.mp4"
fi

[[ -n "${VIDEO}" ]] && log "series      video ${VIDEO} · ${SLUG} (${SPEC:-terminal-only})"
[[ -n "${TERMINAL_SCRIPT}" ]] && log "terminal    ${TERMINAL_SCRIPT}"
log "workspace   ${WORKSPACE_PATH}"
log "video       ${OUT}"
log "browser     ${DEMO_CDP:+CDP → ${DEMO_CDP}}${DEMO_CDP:-WSLg Chromium (DISPLAY=${DISPLAY:-})}"
log "ffmpeg      ${FFMPEG:-(not recording)}"
log "preflight OK"

# --- workspace --------------------------------------------------------------

if [[ "${STACKLESS}" == 0 ]]; then
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
fi # STACKLESS

# --- capture ----------------------------------------------------------------

FFPID=""
FIFO=""
# Set once a screen grab has actually rolled, so the assembly and the narration
# offset ask "is there a terminal segment?" rather than re-deriving it from the
# flags — with --terminal-script there are now two reasons for one to exist.
HAVE_TERMINAL=0
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

# One screen grab, two callers: --with-terminal starts it before Act 0, and
# --terminal-script starts it before the beat script if Act 0 did not already.
# A function rather than a second copy of the pipeline — there is exactly one
# ffmpeg, one FIFO and one stop_capture in this script and that stays true.
start_capture() {
  [[ "${DO_RECORD}" == 1 && -z "${FFPID}" ]] || return 0
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
  # The picture's t=0, handed to the beat script so its narration cues are timed
  # from the START OF THE VIDEO and not from the moment the script happened to
  # run. Without it, beats filmed after Act 0 would be spoken early by Act 0's
  # whole length. (gdigrab's own start-up latency is a few hundred ms; lines are
  # held for seconds, so that is under the noise floor.)
  export WARDYN_DEMO_CAPTURE_ZERO="$(date +%s%3N)"
  exec 9>"${FIFO}"
  sleep 2
  kill -0 "${FFPID}" 2>/dev/null || die "ffmpeg exited immediately — check that gdigrab can see the desktop"
  HAVE_TERMINAL=1
  log "capturing desktop → ${OUT}"
}

[[ "${DO_TERMINAL}" == 1 ]] && start_capture

# --- act 0: cold start ------------------------------------------------------

if [[ "${DO_RESET}" == 1 ]]; then
  step "Act 0 · Clean slate"
  log "wiping the stack (volumes, sandboxes, the wardyn-internal network, compose .env)"
  WARDYN_FORCE_RESET=1 WARDYN_FORCE_STOP_HOST=1 ./scripts/up.sh reset-all --purge-env \
    || die "reset-all failed"
  # Deliberately NOT --purge-images: rebuilding the agent images is minutes of
  # dead air, and they are not what the video is about.
fi

if [[ "${STACKLESS}" == 1 ]]; then
  step "Act 0 · skipped (slides-lane take — no stack involved)"
else
# Skip setup entirely when we did not reset and the stack is already answering.
#
# `make setup` rebuilds the image and re-ups the compose project. That is
# exactly right after a reset, and pure waste otherwise — but the reason this is
# a guard and not an optimisation is RECORDING TWO VIDEOS AT ONCE. Every take
# but 01 opens on a stack an earlier video left up; run three of them together
# and three `docker compose up`s hit ONE project name, so one take's
# container recreation lands in the middle of another take's beat. The browser
# is filming a console whose backend just restarted underneath it.
#
# So: reset asked for, or nothing listening -> set the stack up. Otherwise use
# what is already there, which is what a second take actually wants.
if [[ "${DO_RESET}" == 1 ]] || ! curl -fsS --max-time 5 "http://localhost:${WARDYN_UP_PORT:-8080}/healthz" >/dev/null 2>&1; then
  step "Act 0 · make setup"
  log "mode: containerized (WARDYN_SETUP_MODE=container)"
  log "workspaces root: ${WORKSPACES_ROOT}"
  # WARDYN_UP_NO_BROWSER: up.sh otherwise fires wslview and an uncontrolled window
  # lands in frame. The driver opens the browser itself, at the moment it wants it.
  if [[ "${VIDEO}" == "02" ]]; then
    # Episode 02 opens on THIS install, replayed: capture make setup as a real
    # shell recording (util-linux script + timing) and convert it to an
    # asciicast the take's browser lane plays back. The pty gets a fixed
    # geometry so the player's cols/rows match the converter's.
    step "Act 0 · make setup (recorded for the take)"
    rm -f "${DEMO_OUT_DIR}/setup.typescript" "${DEMO_OUT_DIR}/setup.timing" "${DEMO_OUT_DIR}/setup.cast"
    script -qe --timing="${DEMO_OUT_DIR}/setup.timing" -c \
      "stty cols 110 rows 30 2>/dev/null; WARDYN_SETUP_MODE=container WARDYN_WORKSPACES_ROOT='${WORKSPACES_ROOT}' WARDYN_UP_NO_BROWSER=1 make setup" \
      "${DEMO_OUT_DIR}/setup.typescript" || die "make setup failed (recorded)"
    python3 "${REPO_ROOT}/scripts/cast-convert.py" \
      "${DEMO_OUT_DIR}/setup.typescript" "${DEMO_OUT_DIR}/setup.timing" \
      "${DEMO_OUT_DIR}/setup.cast" || die "cast conversion failed — the install replay would film an empty player"
  else
  WARDYN_SETUP_MODE=container \
  WARDYN_WORKSPACES_ROOT="${WORKSPACES_ROOT}" \
  WARDYN_UP_NO_BROWSER=1 \
    make setup || die "make setup failed"
  fi
else
  step "Act 0 · stack already up"
  log "healthz answered on :${WARDYN_UP_PORT:-8080} and no reset was asked for — reusing it"
  log "(pass --reset to rebuild, which is what video 02 does)"
fi

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
fi # STACKLESS

# --- the terminal beats -----------------------------------------------------

# The host-shell half of a video: `cat` a policy, run a pipeline, hold an ssh
# session — beats no browser can drive. The script gets its captions, typing,
# pauses and voice from scripts/demo-typist.sh; everything this end has to do is
# roll the camera and run it.
TERM_RC=0
TERM_TIMELINE="${DEMO_OUT_DIR}/narration-terminal.json"
if [[ -n "${TERMINAL_SCRIPT}" ]]; then
  # Deleted BEFORE the beats for narrator.ts's reason, one lane over: the typist
  # only writes this file once a line has actually rendered, so a stale timeline
  # from an earlier take survives a fully silent run and gets muxed onto
  # tonight's picture at yesterday's offsets. Worse than silent.
  rm -f "${TERM_TIMELINE}"
  # With --with-terminal the grab is already rolling and Act 0 is on the front of
  # this same segment; without it, the camera starts here, so `make setup` stays
  # off camera and the video opens on the beats.
  start_capture
  step "Terminal beats · ${TERMINAL_SCRIPT##*/}"
  WARDYN_DEMO=1 \
  WARDYN_DEMO_VOICE="${DO_VOICE}" \
  WARDYN_DEMO_WORKSPACE="${WORKSPACE_PATH}" \
    bash "${TERMINAL_SCRIPT}"
  TERM_RC=$?
  [[ "${TERM_RC}" -eq 0 ]] || log "terminal beats exited ${TERM_RC} — this take is incomplete"
fi

# --- acts 1-6: the driver ---------------------------------------------------

# Stop the desktop grab HERE. The terminal — Act 0, or a --terminal-script's
# beats — has to be filmed off the screen, but the browser records itself from
# the inside (playwright.config's demo project), which is the only capture on
# this host that cannot be ruined by another window sitting on top of the frame.
stop_capture

# A stale span file from an earlier take would compress tonight's picture at
# yesterday's offsets — narration.json's problem below, exactly, so it gets
# narration.json's fix. Unconditional: --silent takes fast-forward too.
rm -f "${DEMO_OUT_DIR}/speedups.json"

# Warm the narration cache first: an unrendered line otherwise renders INLINE
# during the take, leaving the caption on screen in silence for ~1.5s — dead air
# in the finished video, once per line.
if [[ "${DO_VOICE}" == 1 ]]; then
  step "Pre-rendering narration"
  # DELETE ANY PRIOR TIMELINE FIRST. narrator.ts only writes this file after its
  # first SUCCESSFUL cue, and it degrades silently when the renderer is missing —
  # so a stale timeline from an earlier take would survive a fully silent run and
  # get muxed onto tonight's picture at yesterday's offsets. Worse than silent.
  rm -f "${DEMO_OUT_DIR}/narration.json"
  # Preflight the renderer for the same reason: narration is on by default, and a
  # broken venv otherwise yields a silent take nobody notices until the morning.
  if ! "${PY_NARRATE:-${HOME}/.cache/wardyn-narrate/venv/bin/python}" \
        "${REPO_ROOT}/scripts/narrate-server.py" --selftest >/dev/null 2>&1; then
    die "narration renderer is broken — fix it, or re-run with --silent to record without a voice"
  fi
  "${REPO_ROOT}/scripts/narrate-prewarm.sh" || log "prewarm failed — lines will render inline instead"
fi

DRIVER_RC=0
if [[ "${RUN_DRIVER}" == 1 ]]; then
if [[ -n "${VIDEO}" ]]; then
  step "Video ${VIDEO} · ${SLUG} · Driving the console"
else
  step "Acts 1-6 · Driving the console"
fi
(
  cd "${REPO_ROOT}/ui" || exit 1
  # The driver needs the capture rect too: it moves the browser window into the
  # frame via CDP and REFUSES to run if the window lands outside it. Without
  # that the recording happily films whatever else is in that screen corner.
  #
  # PW_FILTER is always exactly one spec filename: the --video one, or
  # walkthrough.spec.ts when no video was asked for. Never empty — see where it
  # is set for why letting the demo project's testMatch decide stopped being
  # safe once the ten series specs landed beside the walkthrough.
  WARDYN_DEMO=1 \
  WARDYN_DEMO_WORKSPACE="${WORKSPACE_PATH}" \
  WARDYN_DEMO_CAPTURE="${CAPTURE}" \
  WARDYN_DEMO_VOICE="${DO_VOICE}" \
    pnpm exec playwright test --project=demo --workers=1 --reporter=line \
      --output="${DEMO_OUT_DIR}/pw" "${PW_FILTER[@]}"
)
DRIVER_RC=$?
fi
# A failed beat script must fail the take too — it is half the video now.
[[ "${DRIVER_RC}" -eq 0 ]] && DRIVER_RC="${TERM_RC}"

# --- wrap -------------------------------------------------------------------

stop_capture
trap - EXIT INT TERM

# --- assemble the final video -----------------------------------------------
#
# The console segment is the browser's own recording and is always correct. The
# terminal segment is a screen grab and exists with --with-terminal or
# --terminal-script; when both are present they are concatenated, terminal
# first, re-encoded to a common 1920x1080/30 since they come from different
# sources. A terminal-only video skips all of that: gdigrab already wrote h264
# in an mp4 at the requested size, so the grab IS the finished picture.
BROWSER_VID=""
if [[ "${RUN_DRIVER}" == 1 ]]; then
  # Only probed when the driver actually ran. Otherwise the `ls -t` fallback
  # would happily adopt a webm from LAST NIGHT'S take and staple it onto a
  # terminal video that has no console segment at all.
  BROWSER_VID="${DEMO_OUT_DIR}/console.webm"
  if [[ ! -s "${BROWSER_VID}" ]]; then
    BROWSER_VID="$(ls -t "${DEMO_OUT_DIR}"/*.webm 2>/dev/null | head -1)"
  fi
fi
FINAL=""
# Whether the two segments really were concatenated — the narration offset below
# hangs off this, and inferring it from the flags again would be one more place
# for the two answers to disagree.
JOINED=0
V="[0:v]scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:-1:-1,setsar=1,fps=30"

if [[ "${DO_RECORD}" == 1 && -n "${BROWSER_VID}" && -s "${BROWSER_VID}" ]]; then
  if [[ "${HAVE_TERMINAL}" == 1 && -s "${OUT}" ]]; then
    step "Joining terminal + console segments"
    FINAL="${OUT%.mp4}-full.mp4"
    "${FFMPEG}" -hide_banner -loglevel error -y \
      -i "$(wslpath -w "${OUT}")" -i "$(wslpath -w "${BROWSER_VID}")" \
      -filter_complex "${V}[a];[1:v]scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:-1:-1,setsar=1,fps=30[b];[a][b]concat=n=2:v=1[v]" \
      -map "[v]" -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p \
      "$(wslpath -w "${FINAL}")" \
      && JOINED=1 \
      || { log "join failed — segments are still usable separately"; FINAL=""; }
  else
    step "Encoding the console recording"
    FINAL="${OUT}"
    "${FFMPEG}" -hide_banner -loglevel error -y -i "$(wslpath -w "${BROWSER_VID}")" \
      -vf "fps=30,setsar=1" -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p \
      "$(wslpath -w "${FINAL}")" || { log "encode failed — the raw webm is still there"; FINAL=""; }
  fi
elif [[ "${DO_RECORD}" == 1 && "${HAVE_TERMINAL}" == 1 && -s "${OUT}" ]]; then
  step "Terminal-only take"
  FINAL="${OUT}"
fi

# --- fast-forward the dead air ----------------------------------------------
#
# overlay.ts's ffwdStart/ffwdEnd mark the stretches that are just the agent
# working — minutes of a spinner nobody watches. Squeeze them here, between the
# assembly encode and the mux: the picture is still silent (so the cues can be
# re-timed rather than pitch-shifted along with it), and it is the same encode
# settings, so this costs a generation the take was going to spend anyway.
#
# BROWSER-ONLY takes. With a terminal segment on the front, the spans are in
# browser-recording time while the picture starts OFFSET earlier, and the merge
# below re-times the browser cues a second time. Two clocks, two shifts — out of
# scope, so those takes publish in real time.
FFWD_TIMELINE=""
if [[ -s "${DEMO_OUT_DIR}/speedups.json" && -n "${FINAL}" && -s "${FINAL}" && "${RUN_DRIVER}" == 1 ]]; then
  if [[ "${HAVE_TERMINAL}" == 1 || "${JOINED}" == 1 ]]; then
    log "fast-forward spans recorded, but this take has a terminal segment — publishing in real time"
  else
    step "Fast-forwarding the agent's working time"
    FFWD="${FINAL%.mp4}-ffwd.mp4"
    SHIFTED="${DEMO_OUT_DIR}/narration-ffwd.json"
    if python3 "${REPO_ROOT}/scripts/demo-ffwd.py" --ffmpeg "${FFMPEG}" \
        --video "${FINAL}" --spans "${DEMO_OUT_DIR}/speedups.json" \
        --timeline "${DEMO_OUT_DIR}/narration.json" --timeline-out "${SHIFTED}" \
        --out "${FFWD}" --factor "${WARDYN_DEMO_FFWD_FACTOR:-12}"; then
      FINAL="${FFWD}"
      # Only when it really landed: a --silent take has no timeline to shift.
      [[ -s "${SHIFTED}" ]] && FFWD_TIMELINE="${SHIFTED}"
    else
      log "ffwd failed — publishing real-time take"
    fi
  fi
fi

# --- narration ---------------------------------------------------------------
#
# Runs LAST and on the finished mp4, for two reasons: the raw console recording
# is VP8, which cannot be stream-copied into an mp4 container; and muxing here
# means the picture is encoded exactly once, so narration cannot soften the text.
# The cues are the fast-forwarded ones when the picture was fast-forwarded.
TIMELINE="${FFWD_TIMELINE:-${DEMO_OUT_DIR}/narration.json}"
[[ "${RUN_DRIVER}" == 1 && -s "${TIMELINE}" ]] || TIMELINE=""

if [[ "${DO_VOICE}" == 1 && -n "${FINAL}" && -s "${FINAL}" ]] \
   && [[ -n "${TIMELINE}" || -s "${TERM_TIMELINE}" ]]; then
  step "Adding narration"
  # When the console segment is concatenated AFTER a terminal one, every browser
  # cue is late by exactly the terminal segment's duration.
  OFFSET=0
  if [[ "${JOINED}" == 1 ]]; then
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
  # ONE timeline reaches the mux, because it takes one --offset-ms and the two
  # lanes need two: the terminal lane's cues are already timed from the start of
  # the picture (start_capture exports WARDYN_DEMO_CAPTURE_ZERO), while the
  # browser lane's are timed from the console segment, which begins OFFSET into
  # it. So the browser cues are shifted here and the merged file is muxed flat.
  MUX_TIMELINE="${TIMELINE}"
  if [[ -s "${TERM_TIMELINE}" ]]; then
    MUX_TIMELINE="${TERM_TIMELINE}"
    if [[ -n "${TIMELINE}" ]]; then
      MUX_TIMELINE="${DEMO_OUT_DIR}/narration-joined.json"
      jq -s --argjson off "${OFFSET}" \
        '{zero: .[0].zero, cues: (.[0].cues + [.[1].cues[] | .tMs += $off])}' \
        "${TERM_TIMELINE}" "${TIMELINE}" >"${MUX_TIMELINE}" \
        || { log "timeline merge failed — narrating the terminal half only"; MUX_TIMELINE="${TERM_TIMELINE}"; }
      log "merged $(jq '.cues | length' "${MUX_TIMELINE}" 2>/dev/null || echo '?') cues across both lanes"
    fi
    OFFSET=0
  fi
  NARRATED="${FINAL%.mp4}-narrated.mp4"
  if python3 "${REPO_ROOT}/scripts/narrate-mux.py" --ffmpeg "${FFMPEG}" \
      --video "${FINAL}" --timeline "${MUX_TIMELINE}" --out "${NARRATED}" --offset-ms "${OFFSET}"; then
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
if [[ "${HAVE_TERMINAL}" == 1 && -s "${OUT}" && "${FINAL}" != "${OUT}" ]]; then
  log "  terminal segment (screen grab): ${OUT}"
fi
if [[ "${DRIVER_RC}" -ne 0 ]]; then
  log "driver exited ${DRIVER_RC} — the recording is incomplete."
  [[ "${RUN_DRIVER}" == 1 ]] && log "  Playwright's report: ui/../test/reports/e2e/playwright-report/index.html"
fi
exit "${DRIVER_RC}"
