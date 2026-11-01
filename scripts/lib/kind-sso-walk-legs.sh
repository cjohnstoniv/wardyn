#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# scripts/kind-sso-walk.sh's leg selection (WARDYN_KIND_SSO_LEG) and the role
# walk's render helpers (set_render, roles_leg), split out to keep the walk under
# the 1000-line file-size gate. Sourced, not exec'd: it reads and sets the walk's
# specs and run_roles, and uses die, roles_rc and the walk's cluster variables.

# WARDYN_KIND_SSO_LEG splits this walk across two clusters (nightly.yml runs one
# leg per job). It only SELECTS from the list above, in the same order:
#   member       sso-member + sso-member-recovery, the pair that must share a
#                cluster (recovery inherits what sso-member leaves); no role walk.
#   concurrency  sso-concurrency (+ sso-reauth-hold when not skipped) and the
#                role walk. sso-concurrency re-pins the provider itself and the
#                role walk changes nothing the AWS specs depend on, so neither
#                needs sso-member's leftovers.
# Unset runs everything, as before. The two legs together are exactly that set.
run_roles=1
case "${WARDYN_KIND_SSO_LEG:-}" in
  "") ;;
  member | concurrency)
    leg_specs=()
    for spec in "${specs[@]}"; do
      case "${WARDYN_KIND_SSO_LEG}:${spec}" in
        member:sso-member | member:sso-member-recovery | concurrency:sso-concurrency | concurrency:sso-reauth-hold)
          leg_specs+=("${spec}") ;;
      esac
    done
    specs=("${leg_specs[@]}")
    [[ "${WARDYN_KIND_SSO_LEG}" == "member" ]] && run_roles=0
    ;;
  *) die "WARDYN_KIND_SSO_LEG must be unset, 'member' or 'concurrency' (got '${WARDYN_KIND_SSO_LEG}')" ;;
esac

# ui/e2e/walk/sso-roles.spec.ts signs every Dex identity in and checks the role
# it derives, the console it gets and three API tiers, plus the cross-member
# existence oracle. It runs AFTER the AWS walk so it can change nothing that walk
# depends on, then twice: on the render above (SSO + the admin token), and on
# auth.ssoOnly=true with the token removed — the recipe the SSO-only posture
# ships with. The render is restored afterwards (and by the EXIT trap if this is
# interrupted), and each leg's /healthz is saved beside its log, so the evidence
# names the render every assertion ran against.
set_render() { # sso | sso-only
  local only=false token="${ADMIN_TOKEN}"
  [[ "$1" == "sso-only" ]] && { only=true; token=""; }
  helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn -n "${NAMESPACE}" --reuse-values \
    --set "auth.ssoOnly=${only}" --set "auth.adminToken.value=${token}" \
    >"${EVIDENCE_DIR}/helm-render-$1.log" 2>&1 \
    || { tail -20 "${EVIDENCE_DIR}/helm-render-$1.log" >&2; return 1; }
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=300s >/dev/null || return 1
  # The old pod stays in the endpoints for a moment after the new one is Ready.
  for _ in $(seq 1 60); do
    curl -s "${BASE_URL}/healthz" | jq -e --argjson o "${only}" '.sso_only == $o' >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}
roles_leg() { # <render>
  curl -s "${BASE_URL}/healthz" >"${EVIDENCE_DIR}/roles-$1-healthz.json"
  WARDYN_WALK_ROLES_RENDER="$1" ./scripts/run-ui-e2e.sh sso-roles 2>&1 | tee "${EVIDENCE_DIR}/roles-$1.log"
  [[ "${PIPESTATUS[0]}" -eq 0 ]] || roles_rc=1
}
