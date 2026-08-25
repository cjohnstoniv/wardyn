#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# Cue floors for the episodes that have no content arm yet (pre-record review
# H-6): a take that dies mid-episode still leaves a narration.json, just a
# short one — before these, a died-early 02/05/07 take could verify green on
# the shared gates alone. Floors ~85% of the spec's static caption+act count
# (02: 116, 05: 45, 07: 75 on 2026-08-24); recalibrate against the first real
# rehearsal the way the 03 arms were.
_vfloor_cues() {
  local tl n
  tl="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-${WARDYN_DEMO_VIDEO}}/narration.json"
  if [[ -s "${tl}" ]]; then
    n=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("cues",[])))' "${tl}" 2>/dev/null || echo 0)
    [[ "${n}" -ge "$1" ]] && ok "the episode spoke ${n} lines (floor $1)" \
      || bad "only ${n} narration cues — the take died before the end of the episode (floor $1)"
  else
    bad "no narration timeline at ${tl}"
  fi
}
check_video_floor_02() { head_ "Video 02"; _vfloor_cues 98; }
check_video_floor_05() { head_ "Video 05"; _vfloor_cues 38; }
check_video_floor_07() { head_ "Video 07"; _vfloor_cues 63; }
