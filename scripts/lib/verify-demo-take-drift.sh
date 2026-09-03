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
#
# NO BLOCK MAY EMIT ZERO VERDICTS WITHOUT FAILING. This gate used to open with
# `[[ -s "${WEBM}" ]] || return 0` — a SILENT return on exactly the input that
# makes it unrunnable, so a browser-lane take whose console.webm never landed
# graded green on a clock nothing had measured. A missing picture is now a
# verdict; the terminal lane's exemption says its own name instead of arriving
# as an absent file; and the tail counts, so a run that graded NOTHING fails
# rather than reading like a pass.
check_take_drift() {
  local WEBM="${TL%/*}/console.webm" FITF="${TL%/*}/drift-fit.json" DRIFT RC
  # The running verdict total at entry — the tail below compares against it.
  local VERDICTS=$((PASS + FAIL))
  if [[ ! -s "${WEBM}" ]]; then
    # THE ONE EXEMPTION, and it is named rather than inferred: a terminal lane's
    # picture is gdigrab's, which stamps realtime, so there is no drift to fit
    # and no console recording to fit it on.
    if [[ -s "${TL%/*}/narration-terminal.json" ]]; then
      printf '    (terminal lane: no console recording — gdigrab stamps realtime, so there is no drift to measure)\n'
      return 0
    fi
    bad "no ${WEBM} — the browser lane recorded no picture, so this gate measured nothing (which is not the same as being in sync)"
    return 1
  fi
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
  # The rule, enforced rather than asserted by eye: an unmeasurable fit on a take
  # with no published video left this whole block grading nothing at all.
  ((PASS + FAIL > VERDICTS)) || bad "the drift gate produced no verdict at all — this take's clock is UNKNOWN, and a green take must not say that is fine"
}
