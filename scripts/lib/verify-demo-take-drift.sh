#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# The picture's clock: record-demo reconciles cue time onto it BEFORE ffwd and
# mux consume anything (scripts/demo-clockfix.py rewrites cues + spans through
# demo-drift's fitted rate/offset). So the gate here asserts the RESULT: the
# webm measured against the corrected timeline must sit within the legacy ±1%.
# On a finished browser-lane take the fit file must also say applied — proof
# the rewrite ran, not just that a fit existed. Mixed/terminal lanes exempt
# (narration-terminal.json present): gdigrab stamps realtime.
check_take_drift() {
  local WEBM="${TL%/*}/console.webm" FITF="${TL%/*}/drift-fit.json" DRIFT RC
  [[ -s "${WEBM}" ]] || return 0
  DRIFT="$(python3 "${REPO_ROOT}/scripts/demo-drift.py" --quiet --video "${WEBM}" --timeline "${TL}" 2>&1)"; RC=$?
  printf '%s\n' "${DRIFT}" | sed 's/^/  /'
  case "${RC}" in 0) ok "picture and narration on the same clock" ;;
    1) bad "clock drift survived — the fix never ran or its fit was bad" ;;
    *) printf '    (unmeasurable — skipped)\n' ;; esac
  [[ -s "${TL%/*}/narration-terminal.json" ]] && return 0
  if [[ -n "${VIDEO}" && -s "${VIDEO}" ]]; then
    if grep -q '"applied": true' "${FITF}" 2>/dev/null; then ok "cue clock reconciled before the mux (clockfix applied)"
    else bad "take published without the clock fix — reprocess with demo-clockfix.py"; fi
  fi
}
