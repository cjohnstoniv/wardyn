#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The lettered optional sub-episodes' arms: 02b (desktop), 02c (cloud), 04b
# (members), 04c (admin) and 12b (admin operations).
#
# OUT OF LINE ON PURPOSE, like scripts/lib/verify-demo-take-03.sh:
# scripts/verify-demo-take.sh sits a handful of lines under
# scripts/check-file-size.sh's 1000-line threshold, so an inline arm breaks
# `make lint`. Functions, never subshells — ok()/bad() increment counters in the
# caller that must survive to the summary.
#
# THESE STUBS FAIL BY DESIGN. Each episode's own lane (campaign §2 lanes 2.1-2.4)
# replaces the body with rows read off the audit trail. Until then the arm exists
# only so a take of one of these ids stops hitting the dispatch's `*)` — and it
# still has to fail, because an id whose checks nobody has written must never
# verify green off a cue count. A stub that passes is how a dishonest take ships.

# Placeholder cue floor: a died-early tripwire (the 01 arm's shape), not a
# content check. The lane that writes the episode sets its own.
_VOPT_FLOOR="${WARDYN_DEMO_CUE_FLOOR:-20}"

_vopt_stub() {
  local id="$1" title="$2" work tl n
  head_ "Video ${id} · ${title}"
  work="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-${id}}"
  tl="${work}/narration.json"
  # A terminal-lane optional (02c's console half aside, 12b's beats) has no
  # browser timeline at all — same fallback the shared narration block makes.
  [[ -s "${tl}" ]] || tl="${work}/narration-terminal.json"
  if [[ -s "${tl}" ]]; then
    n=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("cues",[])))' "${tl}" 2>/dev/null || echo 0)
    [[ "${n}" -ge "${_VOPT_FLOOR}" ]] && ok "the take spoke ${n} lines (placeholder floor ${_VOPT_FLOOR})" \
      || bad "only ${n} narration cues — the take died early (placeholder floor ${_VOPT_FLOOR})"
  else
    bad "no narration timeline at ${tl}"
  fi
  bad "no checks yet — the episode's spec ships them"
}

check_video_02b() { _vopt_stub 02b "the desktop deck (desktop)"; }
check_video_02c() { _vopt_stub 02c "one command to a cluster (cloud)"; }
check_video_04b() { _vopt_stub 04b "a member's own workspace (members)"; }
check_video_04c() { _vopt_stub 04c "permissions — who may do what (admin)"; }
check_video_12b() { _vopt_stub 12b "admin operations (admin)"; }
