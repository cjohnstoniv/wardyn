#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The terminal lane's overlay: captions, typing, chapter cards and narration for
# the parts of the demo that are not a web page.
#
# WHY THIS EXISTS. The recorder has two capture lanes. The browser lane films the
# console from inside the page (Playwright recordVideo) and gets its presentation
# from ui/e2e/demo/overlay.ts — a caption bar, a spotlight ring, chapter cards,
# every line of it also SPOKEN by scripts/narrate-server.py. Two videos of the
# 0.5 series have no page to film: V09 (CI & headless) is `cat` a policy, run
# scripts/ci-run.sh, `echo $?`, read the artifacts; V10 (audit & attach) is three
# terminals holding `ssh <run-uuid>@127.0.0.1 -p 2222` with three different keys.
# Before this file the terminal lane could film exactly one hardcoded thing —
# `make setup` during Act 0 — with no captions and no voice. This gives it the
# same vocabulary the browser lane has, so the two halves of a video read as one
# series rather than as a polished web demo followed by somebody's screen share.
#
# HOW IT IS USED. Source it from a beat script and call the verbs in order:
#
#     . "$(dirname "${BASH_SOURCE[0]}")/../demo-typist.sh"
#     narration_zero
#     chapter "CI & headless" "The same governance, unattended"
#     say "This is the policy the pipeline runs under."
#     type_cmd "cat examples/policies/ci.json"
#     type_cmd 'echo $?'
#     narration_end
#
# and record it with:  scripts/record-demo.sh --video 09 --terminal-script <path>
#
# THE VOCABULARY MIRRORS overlay.ts ON PURPOSE. say() is caption(), beat() is
# beat(), chapter() is chapter(), type_cmd() is typeInTerminal(). Same names,
# same pacing constants, same "hold the frame until the line has finished being
# spoken" rule — so a beat written for one lane reads the same as a beat written
# for the other, and neither has a timing dialect of its own.
#
# NARRATION IS A TIMELINE, NOT PLAYBACK. Nothing is played out loud during a
# take: the machine being filmed has no speakers in the loop and a played clip
# would not be in the capture anyway. Each spoken line is rendered to a wav and
# appended to ui/test-results/demo-video/narration-terminal.json as
# {file, tMs, durMs, text} — the exact shape ui/e2e/demo/narrator.ts writes, so
# scripts/narrate-mux.py consumes it unchanged — and record-demo.sh lays the two
# lanes' timelines onto the finished mp4 at those offsets.
#
# WHY A SEPARATE TIMELINE FILE FROM THE BROWSER LANE'S. narrator.ts rewrites
# narration.json wholesale after every cue. A video that is terminal beats THEN
# browser beats would therefore lose every terminal cue the moment the driver
# spoke its first line. The two lanes write two files and record-demo.sh merges
# them, shifting the browser cues by the terminal segment's measured duration —
# which it has to compute anyway to join the picture.
#
# ZERO IS THE CAPTURE'S ZERO, NOT THIS SCRIPT'S. record-demo.sh exports
# WARDYN_DEMO_CAPTURE_ZERO when it starts the screen grab. Without that a beat
# script filmed after Act 0 would time its cues from the moment IT started and
# every line would land early by the length of everything filmed before it.
#
# FAILURE POSTURE, COPIED FROM THE BROWSER LANE. Narration is cosmetic and must
# never be able to fail a recording. No venv, no server, no jq, a dead renderer,
# a line that will not render — every one of them degrades to a printed caption
# held for a normal reading beat, exactly as narrator.ts's speak() returning 0
# leaves overlay.ts holding for PACE.read.
#
#     scripts/demo-typist.sh --selftest   # prove the wiring without recording

_TYPIST_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_TYPIST_REPO="$(cd "${_TYPIST_HERE}/.." && pwd)"

# The renderer. Two spellings because the repo already has two — narrate-prewarm.sh
# reads WARDYN_NARRATE_PY and record-demo.sh reads PY_NARRATE — and inventing a
# third would be the sync problem this pipeline keeps refusing to create.
_TYPIST_PY="${WARDYN_NARRATE_PY:-${PY_NARRATE:-${HOME}/.cache/wardyn-narrate/venv/bin/python}}"
_TYPIST_SERVER="${_TYPIST_HERE}/narrate-server.py"
# WARDYN_DEMO_WORK_DIR is the per-video scratch dir record-demo.sh exports and
# READS THE TIMELINE BACK FROM (${DEMO_OUT_DIR}/narration-terminal.json). The
# old fixed demo-video/ default silently split the two on any --video take:
# the typist wrote one path, the merge read another, and the terminal half of
# the video shipped VOICELESS with every step exiting 0.
_TYPIST_TIMELINE="${WARDYN_DEMO_TERMINAL_TIMELINE:-${WARDYN_DEMO_WORK_DIR:-${_TYPIST_REPO}/ui/test-results/demo-video}/narration-terminal.json}"

# Pacing. The numbers are overlay.ts's PACE, verbatim: 45ms per keystroke (its
# page.keyboard.type delay), 2200ms to read a caption, 2600ms of chapter card,
# and the +300ms tail that keeps the next visual off the last syllable.
_TYPIST_KEY_DELAY="${WARDYN_TYPIST_KEY_DELAY:-0.045}"
_TYPIST_READ_MS="${WARDYN_TYPIST_READ_MS:-2200}"
_TYPIST_CHAPTER_MS="${WARDYN_TYPIST_CHAPTER_MS:-2600}"
_TYPIST_TAIL_MS="${WARDYN_TYPIST_TAIL_MS:-300}"
# The pause a human leaves between finishing a command and hitting Enter.
_TYPIST_ENTER_MS="${WARDYN_TYPIST_ENTER_MS:-450}"

_TYPIST_ZERO=0
_TYPIST_MUTE=0
_TYPIST_CUES=()
_TYPIST_FILE=""
_TYPIST_DUR=0
# The exit status of the last type_cmd. Public: a beat script may branch on it,
# and type_cmd itself restores it so a filmed `echo $?` tells the truth.
TYPIST_RC=0

# --- narration ---------------------------------------------------------------

_typist_now() { date +%s%3N; }

_typist_warn() { printf '\033[2m[typist] %s\033[0m\n' "$*" >&2; }

# Bring the TTS server up once, lazily. One process, one 310MB model load, N
# renders — the whole reason narrate-server.py is a server and not a CLI (a
# per-line invocation would add minutes to a take). Any reason it cannot start is
# a permanent, quiet mute for the rest of the run.
_typist_tts() {
  [[ "${_TYPIST_MUTE}" == 1 ]] && return 1
  [[ -n "${_TYPIST_TTS_PID:-}" ]] && return 0
  if [[ "${WARDYN_DEMO_VOICE:-1}" != "1" ]]; then
    _TYPIST_MUTE=1
    return 1
  fi
  if [[ ! -x "${_TYPIST_PY}" || ! -f "${_TYPIST_SERVER}" ]] || ! command -v jq >/dev/null 2>&1; then
    _typist_warn "narration off — no renderer at ${_TYPIST_PY} (or no jq)"
    _TYPIST_MUTE=1
    return 1
  fi
  # stderr swallowed: the server announces its engine choice there, and that
  # banner would be printed into the frame being filmed.
  coproc _TYPIST_TTS { "${_TYPIST_PY}" "${_TYPIST_SERVER}" 2>/dev/null; }
  return 0
}

# Render one line. Sets _TYPIST_FILE/_TYPIST_DUR and returns non-zero when the
# line could not be rendered. Deliberately not a command substitution: bash
# closes coproc file descriptors inside subshells, so a `$(...)` wrapper here
# would read from a descriptor that does not exist.
_typist_render() {
  _TYPIST_FILE=""; _TYPIST_DUR=0
  _typist_tts || return 1
  local req reply parsed
  req="$(jq -nc --arg t "$1" '{text:$t}')" || return 1
  printf '%s\n' "${req}" >&"${_TYPIST_TTS[1]}" 2>/dev/null || {
    _typist_warn "renderer died — the rest of this take is silent"
    _TYPIST_MUTE=1
    return 1
  }
  # 30s, same as narrator.ts: a hung renderer must not hang the take.
  if ! IFS= read -r -t 30 -u "${_TYPIST_TTS[0]}" reply; then
    _typist_warn "render timeout — the rest of this take is silent"
    _TYPIST_MUTE=1
    return 1
  fi
  parsed="$(jq -r '[(.file // ""), (.durMs // 0)] | @tsv' <<<"${reply}" 2>/dev/null)" || return 1
  IFS=$'\t' read -r _TYPIST_FILE _TYPIST_DUR <<<"${parsed}"
  [[ -n "${_TYPIST_FILE}" && "${_TYPIST_DUR}" -gt 0 ]] || {
    _typist_warn "not rendered :: ${1:0:60}"
    return 1
  }
  return 0
}

# Persist the timeline after EVERY cue, for narrator.ts's reason: a take that
# dies on the last beat should still leave a muxable timeline for the ones that
# landed, and there is no exit hook here worth trusting.
_typist_flush() {
  [[ "${#_TYPIST_CUES[@]}" -gt 0 ]] || return 0
  mkdir -p "$(dirname "${_TYPIST_TIMELINE}")" 2>/dev/null
  printf '%s\n' "${_TYPIST_CUES[@]}" \
    | jq -s --argjson z "${_TYPIST_ZERO}" '{zero: $z, cues: .}' >"${_TYPIST_TIMELINE}.tmp" 2>/dev/null \
    && mv -f "${_TYPIST_TIMELINE}.tmp" "${_TYPIST_TIMELINE}"
  return 0
}

# Start narration's clock. Prefers the capture's zero (see the header) and
# deletes any timeline an earlier take left behind — a stale one survives a
# fully silent run and gets muxed onto tonight's picture at yesterday's offsets,
# which is worse than silence.
narration_zero() {
  _TYPIST_ZERO="${WARDYN_DEMO_CAPTURE_ZERO:-$(_typist_now)}"
  _TYPIST_CUES=()
  rm -f "${_TYPIST_TIMELINE}"
  return 0
}

# Close the renderer and write the timeline one last time.
narration_end() {
  _typist_flush
  if [[ -n "${_TYPIST_TTS_PID:-}" ]]; then
    eval "exec ${_TYPIST_TTS[1]}>&-" 2>/dev/null
    wait "${_TYPIST_TTS_PID}" 2>/dev/null
    unset _TYPIST_TTS_PID
  fi
  return 0
}

# --- the vocabulary ----------------------------------------------------------

# A readable pause, in milliseconds. Pacing only — never a wait on the app.
beat() {
  local ms="${1:-${_TYPIST_READ_MS}}"
  [[ "${ms}" -gt 0 ]] && sleep "$(printf '%d.%03d' "$((ms / 1000))" "$((ms % 1000))")"
  return 0
}

# Print a caption and speak it, then hold the terminal until the line has
# finished being spoken. The hold is what keeps speech from running over the
# next command — overlay.ts's awaitSpeech(), with the same +300ms tail.
#
# The clip is rendered BEFORE the line is printed so that the render pause (~1.5s
# on a cold cache) lands on the previous frame rather than inside this caption's
# own hold, where it would leave text on screen in silence. Clips are cached by
# content hash, so this costs nothing from the second take onward.
say() {
  local text="$1" dur=0
  if _typist_render "${text}"; then dur="${_TYPIST_DUR}"; fi
  printf '\n\033[1;36m▌\033[0m \033[1m%s\033[0m\n' "${text}"
  if [[ "${dur}" -gt 0 ]]; then
    _TYPIST_CUES+=("$(jq -nc --arg f "${_TYPIST_FILE}" --argjson t "$(($(_typist_now) - _TYPIST_ZERO))" \
      --argjson d "${dur}" --arg x "${text}" '{file:$f, tMs:$t, durMs:$d, text:$x}')")
    _typist_flush
    beat "$((dur + _TYPIST_TAIL_MS))"
  else
    # Silent lane: hold for a normal reading beat, which is exactly what
    # overlay.ts does when speak() returns 0. A caption that flashes past is
    # broken footage, not a degraded one.
    beat "${_TYPIST_READ_MS}"
  fi
  return 0
}

# A full-screen act divider, the terminal's answer to overlay.ts's chapter card:
# clear the screen, hold the title alone, clear it again. Spoken as one line
# ("Title. Subtitle.") so the card does not clear between its own two halves.
chapter() {
  local title="$1" sub="${2:-}" dur=0 line="$1"
  [[ -n "${sub}" ]] && line="${title}. ${sub}."
  if _typist_render "${line}"; then dur="${_TYPIST_DUR}"; fi
  printf '\033[2J\033[3J\033[H'
  printf '\n\n\n\n\n    \033[1;97m%s\033[0m\n' "${title}"
  [[ -n "${sub}" ]] && printf '    \033[2;37m%s\033[0m\n' "${sub}"
  if [[ "${dur}" -gt 0 ]]; then
    _TYPIST_CUES+=("$(jq -nc --arg f "${_TYPIST_FILE}" --argjson t "$(($(_typist_now) - _TYPIST_ZERO))" \
      --argjson d "${dur}" --arg x "${line}" '{file:$f, tMs:$t, durMs:$d, text:$x}')")
    _typist_flush
  fi
  beat "$((dur + _TYPIST_TAIL_MS > _TYPIST_CHAPTER_MS ? dur + _TYPIST_TAIL_MS : _TYPIST_CHAPTER_MS))"
  printf '\033[2J\033[3J\033[H'
  beat 400
  return 0
}

# Type a command out at human speed, run it, and let its real output land on
# camera. The status is the command's own — returned, and left in TYPIST_RC.
#
# eval, not "$@": the beats are shell, not argv. V09's headline command is an
# env-prefixed invocation with a pipeline into jq, and quoting that through an
# array would make the beat script unreadable for no gain — these strings are
# ours, written next to the video they film.
#
# $? IS RESTORED BEFORE THE EVAL. A beat of `echo $?` is a real beat (V09 shows
# the pipeline's exit code) and without this it would report the status of the
# keystroke loop above, which is always 0 — a filmed lie.
type_cmd() {
  local cmd="$1" i
  printf '\n\033[1;32m$\033[0m '
  for ((i = 0; i < ${#cmd}; i++)); do
    printf '%s' "${cmd:i:1}"
    sleep "${_TYPIST_KEY_DELAY}"
  done
  printf '\n'
  beat "${_TYPIST_ENTER_MS}"
  (exit "${TYPIST_RC}")
  eval "${cmd}"
  TYPIST_RC=$?
  return "${TYPIST_RC}"
}

# --- selftest ----------------------------------------------------------------
#
# Only when EXECUTED, never when sourced. Proves the whole chain — clock, TTS
# coprocess, cue shape, timeline write, exit-status restoration — without
# recording anything.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  [[ "${1:-}" == "--selftest" ]] || { sed -n '5,60p' "${BASH_SOURCE[0]}"; exit 0; }
  WARDYN_TYPIST_KEY_DELAY=0.005
  _TYPIST_KEY_DELAY=0.005
  _TYPIST_READ_MS=200
  _TYPIST_TIMELINE="$(mktemp -d)/narration-terminal.json"
  narration_zero
  say "Every run gets its own identity, its own barrier, and no resident credentials."
  type_cmd "echo hello"
  type_cmd 'echo $?'
  say "Egress was wide open, and these two never even got a connection."
  narration_end
  echo
  if [[ -s "${_TYPIST_TIMELINE}" ]]; then
    jq . "${_TYPIST_TIMELINE}"
    jq -e '[.cues[].file] | all(. as $f | ($f | length) > 0)' "${_TYPIST_TIMELINE}" >/dev/null \
      && echo "selftest: timeline OK ($(jq '.cues | length' "${_TYPIST_TIMELINE}") cues) -> ${_TYPIST_TIMELINE}"
  else
    echo "selftest: no timeline (renderer absent — this is the silent-degrade path, not a failure)"
  fi
fi
