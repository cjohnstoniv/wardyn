# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-13.sh — the cluster episode's grader, sourced by
# scripts/verify-demo-take.sh. Its own file because that script is a shared seam:
# 0.6 added this video while main kept editing the same script, and the two sets
# of additions summed past the 1000-line file-size gate. Uses the caller's
# ok/bad/head_ and REPO_ROOT (resolved at call time, after the caller defines them).

# Video 13 · your terminal, our cluster. A TERMINAL-ONLY video (there is no
# ui/e2e/demo/13-*.spec.ts), shot against the cluster `make kind-quickstart`
# leaves behind — so unlike every other take the evidence is on a k8s install
# that wants a bearer token, read from its own Secret the way
# scripts/run-e2e-ssh-k8s.sh and deploy/kind/quickstart.sh read it.
#
# THREE audit checks, deliberately: the two exec rows the video films by their
# argv and exit code, and who the connections were attributed to. The run's
# STATE is not asserted for check_video_10's reason — an idle interactive run
# may be reaped between the take and the check, which does not make the footage
# dishonest.
check_video_13() {
head_ "Video 13 · the run the terminal reached"
V13_CTX="${WARDYN_V13_CONTEXT:-kind-wardyn-quickstart}"
V13_NS="${WARDYN_V13_NAMESPACE:-wardyn}"
V13_API="${WARDYN_URL:-http://127.0.0.1:8080}"
V13_TOK="${WARDYN_ADMIN_TOKEN:-$(kubectl --context "${V13_CTX}" -n "${V13_NS}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)}"
V13_HANDOFF="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-13}/v13-run-id.txt"
V13_RUN="${WARDYN_DEMO_RUN_ID:-}"
[[ -z "${V13_RUN}" && -s "${V13_HANDOFF}" ]] && V13_RUN="$(tr -d '[:space:]' <"${V13_HANDOFF}")"

# The title's own claim, and the one a 200 on :8080 does NOT prove: quickstart
# and the operator's compose stack publish the same port.
V13_RUNNER="$(curl -fsS --max-time 10 "${V13_API}/healthz" 2>/dev/null | jq -r '.runner // "-"' 2>/dev/null || echo '-')"
[[ "${V13_RUNNER}" == "k8s" ]] && ok "${V13_API} is the k8s install (runner=k8s) — the substrate the video claims" \
  || bad "${V13_API}/healthz reports runner=${V13_RUNNER}, not k8s — this take did not film a cluster"

if [[ -z "${V13_RUN}" ]]; then
  bad "no run id at ${V13_HANDOFF} — the beats never got past preflight, so there is nothing this video could have filmed"
elif [[ -z "${V13_TOK}" ]]; then
  bad "no admin token (WARDYN_ADMIN_TOKEN, or Secret wardyn-auth in ${V13_CTX}/${V13_NS}) — the trail on a k8s install cannot be read without one"
else
  ok "run ${V13_RUN} (from the handoff the beats wrote)"
  V13_OWNER="$(curl -fsS --max-time 10 -H "Authorization: Bearer ${V13_TOK}" "${V13_API}/api/v1/runs/${V13_RUN}" 2>/dev/null | jq -r '.created_by // "-"')"
  V13_AUD="$(curl -fsS --max-time 20 -H "Authorization: Bearer ${V13_TOK}" "${V13_API}/api/v1/audit?run_id=${V13_RUN}&limit=1000" 2>/dev/null || echo '[]')"
  v13q() { jq "[ (.items? // .)[] | $1 ] | length" <<<"${V13_AUD}" 2>/dev/null || echo 0; }

  # 1 · beat 3: the Pod answered, over the k8s exec lane.
  N="$(v13q 'select(.action == "ssh.exec" and .outcome == "success" and .data.argv == "hostname" and .data.exit == 0)')"
  [[ "${N}" -ge 1 ]] && ok "${N} ssh.exec row(s) for argv 'hostname', exit 0 — beat 3 really reached the Pod" \
    || bad "no successful ssh.exec row for argv 'hostname' — beat 3's shell never ran inside the sandbox"

  # 2 · beat 4/5: the ONE number this video films twice, in the shell and on
  # the trail. k8s carries an exec's status out of band, so this row is the
  # substrate claim as much as the audit one.
  N="$(v13q 'select(.action == "ssh.exec" and .data.exit == 37)')"
  [[ "${N}" -ge 1 ]] && ok "${N} ssh.exec row(s) recording exit 37 — the code crossed the cluster intact" \
    || bad "no ssh.exec row carrying exit 37 — beat 4 echoed a number the trail does not have"

  # 3 · beat 5: every connection attributed to the run's owner. sshAuth is
  # owner-only, so a success under any other actor would mean the gateway let
  # somebody else in — the opposite of what the closing line says.
  N="$(v13q 'select(.action == "ssh.auth" and .outcome == "success")')"
  M="$(v13q "select(.action == \"ssh.auth\" and .outcome == \"success\" and .actor == \"${V13_OWNER}\")")"
  [[ "${N}" -ge 1 && "${N}" == "${M}" ]] && ok "all ${N} ssh.auth successes attributed to the run's owner (${V13_OWNER})" \
    || bad "${N} ssh.auth success(es), ${M} of them the run's owner (${V13_OWNER}) — a connection this video does not account for"
fi
}
