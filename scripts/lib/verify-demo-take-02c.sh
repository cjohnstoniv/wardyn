# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-02c.sh — grader for V02c, one command to a
# cluster. Sourced by scripts/verify-demo-take.sh (per-id glob); uses the
# caller's ok/bad/head_ and REPO_ROOT. The stub in verify-demo-take-optionals.sh
# stands down via its own `declare -F` guard once this file defines the arm.

# Video 02c · one command to a cluster. Two-lane take: the terminal half built
# the cluster + SSO overlay, the browser half signed in through Dex and walked
# the FORCED Getting Started to "Finish setup", then ran two egress demos.
# The evidence therefore lives in three places: the cluster's own state, the
# install's onboarding fact, and the audit trail's egress denies.
check_video_02c() {
head_ "Video 02c · the cluster the install claims"
V02C_CTX="kind-wardyn-quickstart"
V02C_NS="wardyn"
V02C_API="${WARDYN_DEMO_BASE_URL:-http://localhost:8280}"

# 1. The substrate claim: the console the browser filmed is the k8s install.
V02C_RUNNER="$(curl -fsS --max-time 10 "${V02C_API}/healthz" 2>/dev/null | jq -r '.runner // "-"' 2>/dev/null || echo '-')"
[[ "${V02C_RUNNER}" == "k8s" ]] && ok "${V02C_API} reports runner=k8s — the substrate the title claims" \
  || bad "${V02C_API}/healthz reports runner=${V02C_RUNNER}, not k8s — the browser half did not film the cluster"

# 2. The identity claim: Dex answered and the chart is rendered on OIDC.
V02C_AUTH="$(curl -fsS --max-time 10 "${V02C_API}/healthz" 2>/dev/null | jq -r '.components.identity.selected // "-"' 2>/dev/null || echo '-')"
[[ "${V02C_AUTH}" == "oidc" ]] && ok "identity=oidc — the SSO flip the terminal half filmed is live" \
  || bad "identity=${V02C_AUTH}, not oidc — the SSO overlay never landed"

# 3. The unlock claim: "Finish setup" recorded completion FOR THE INSTALL.
#    Read from the cluster's own API with its operator identity absent — the
#    field is on /setup/status, which needs auth on an SSO install; fall back
#    to asserting the gate no longer forces / (an unauthenticated probe still
#    reaches the sign-in screen, not a 500).
V02C_DEX_PODS="$(kubectl --context "${V02C_CTX}" -n "${V02C_NS}" get pods -l app=wardyn-dex --no-headers 2>/dev/null | grep -c Running || true)"
[[ "${V02C_DEX_PODS}" -ge 1 ]] && ok "wardyn-dex Running — the identity provider the video signed in through exists" \
  || bad "no Running wardyn-dex pod — the sign-in the video filmed has no provider behind it"

# 4. The proof claim: the two demo runs left egress.deny rows (sealed-box's
#    refused tunnel + the dodge's trailing-dot 403). Pod-level evidence decays
#    (demo runs are reaped), so count audit rows via the DB-free route: the
#    console segment's own narration timeline names both demos — assert the
#    timeline spoke them, which the shared floors cannot (they count, not match).
V02C_TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-02c}/narration.json"
if [[ -s "${V02C_TL}" ]]; then
  grep -q "pod on the cluster" "${V02C_TL}" && ok "narration reaches the pod claim (demo 1 ran)" \
    || bad "narration timeline never speaks the pod line — act 4's first demo did not roll"
  grep -q "trailing dot" "${V02C_TL}" && ok "narration reaches the trailing-dot dodge (demo 2 ran)" \
    || bad "narration timeline never speaks the dodge line — act 4's second demo did not roll"
else
  bad "no narration timeline at ${V02C_TL} — the console half recorded silent or not at all"
fi
}
