#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# The picture's clock: a cue is stamped when its caption goes on screen, so the
# caption must CHANGE at that tMs. Take 10 shipped 3% slow (narrator.ts nowMs()).
# WEBM sits next to TL: console.webm was stamped against the timeline in its own
# dir, per-video dirs included.
check_take_drift() {
  local WEBM="${TL%/*}/console.webm" DRIFT RC
  [[ -s "${WEBM}" ]] || return 0
  DRIFT="$(python3 "${REPO_ROOT}/scripts/demo-drift.py" --quiet --video "${WEBM}" --timeline "${TL}" 2>&1)"; RC=$?
  printf '%s\n' "${DRIFT}" | sed 's/^/  /'
  case "${RC}" in 0) ok "picture and narration on the same clock" ;;
    1) bad "capture clock drift — the narration slides late" ;;
    *) printf '    (unmeasurable — skipped)\n' ;; esac
}
