#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# The picture's clock: Playwright's screencast timebase and the monotonic cue
# clock disagree by a small per-take rate. The mux CORRECTS it (narrate-mux
# --drift-fit lays cues on the picture's clock), so the gate here is fit
# QUALITY — enough caption changes matched to correct reliably — plus, on a
# finished browser-lane take, proof the mux actually applied the fit.
# WEBM/fit sit next to TL, per-video dirs included. Mixed/terminal lanes
# (narration-terminal.json present) are exempt: gdigrab stamps realtime.
check_take_drift() {
  local WEBM="${TL%/*}/console.webm" FITF="${TL%/*}/drift-fit.json" DRIFT RC
  [[ -s "${WEBM}" ]] || return 0
  DRIFT="$(python3 "${REPO_ROOT}/scripts/demo-drift.py" --quiet --quality-gate --emit-fit "${FITF}" --video "${WEBM}" --timeline "${TL}" 2>&1)"; RC=$?
  printf '%s\n' "${DRIFT}" | sed 's/^/  /'
  case "${RC}" in 0) ok "picture/narration mapping measured — mux corrects to it" ;;
    1) bad "drift fit unreliable — a correction would guess; check the take" ;;
    *) printf '    (unmeasurable — skipped)\n' ;; esac
  [[ -s "${TL%/*}/narration-terminal.json" ]] && return 0
  if [[ -n "${VIDEO}" && -s "${VIDEO}" && "${RC}" -eq 0 ]]; then
    if grep -q '"applied": true' "${FITF}" 2>/dev/null; then ok "narration laid on the picture's own clock (drift-corrected mux)"
    else bad "take muxed without the drift correction — re-mux with --drift-fit"; fi
  fi
}
