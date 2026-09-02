# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-04c.sh — grader for V04c, who may do what.
# Sourced by scripts/verify-demo-take.sh; uses the caller's ok/bad/head_.
check_video_04c() {
head_ "Video 04c · one install, two roles"
V04C_TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-04c}/narration.json"
if [[ -s "${V04C_TL}" ]]; then
  V04C_CUES="$(jq 'if type=="array" then length else ((.cues // []) | length) end' "${V04C_TL}" 2>/dev/null || echo 0)"
  [[ "${V04C_CUES}" -ge 15 ]] && ok "the episode spoke ${V04C_CUES} lines (floor 15)" \
    || bad "only ${V04C_CUES} narration cues — the take died early (floor 15)"
  grep -q "who this changes" "${V04C_TL}" && ok "narration reaches the enforcement throw (who this changes)" \
    || bad "narration never reaches the enforcement dialog — act 2's switch throw did not film"
  grep -q "Not a host you're granted\|not a host you're granted" "${V04C_TL}" && ok "narration reaches the member's lived refusal" \
    || bad "narration never reaches the member's refusal chip — act 3's climax did not film"
else
  bad "no narration timeline at ${V04C_TL} — the take recorded silent or not at all"
fi
V04C_SSO="$(curl -fsS --max-time 10 "${WARDYN_DEMO_BASE_URL:-http://localhost:8280}/healthz" 2>/dev/null | jq -r '.sso // false' 2>/dev/null || echo false)"
[[ "${V04C_SSO}" == "true" ]] && ok "filmed against the SSO install (healthz sso=true)" \
  || bad "healthz sso=${V04C_SSO} — this take did not film the multi-user install"
}
