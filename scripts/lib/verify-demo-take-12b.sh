# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-12b.sh — grader for V12b, four eyes on egress.
check_video_12b() {
head_ "Video 12b · four eyes, receipts for both"
V12B_TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-12b}/narration.json"
if [[ -s "${V12B_TL}" ]]; then
  V12B_CUES="$(jq 'if type=="array" then length else ((.cues // []) | length) end' "${V12B_TL}" 2>/dev/null || echo 0)"
  [[ "${V12B_CUES}" -ge 15 ]] && ok "the episode spoke ${V12B_CUES} lines (floor 15)" \
    || bad "only ${V12B_CUES} narration cues — the take died early (floor 15)"
  grep -q "someone else approves or denies" "${V12B_TL}" && ok "narration reaches the four-eyes refusal" \
    || bad "narration never reaches the refusal — act 3's 403 did not film"
  grep -q "did not create the run" "${V12B_TL}" && ok "narration reaches the second human's decide" \
    || bad "narration never reaches the second human's decide — act 4 did not film"
  grep -q "authz.denied" "${V12B_TL}" && ok "narration reaches the audit trail (authz.denied)" \
    || bad "narration never reaches the trail — act 5 did not film"
else
  bad "no narration timeline at ${V12B_TL} — the take recorded silent or not at all"
fi
}
