#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-claims-match-code.sh — daemon-free, network-free assertions that what
# Wardyn SAYS matches what it DOES. This script used to carry a long list of
# checks; most of them were prose `grep -q` pins with no fact behind them, or
# facts a Go guard under cmd/wardynd/*_test.go already covers directly. Part
# of #209 (lane 2a) cut them down to the two that compare two files neither
# a human diff nor a Go test already watches:
#
#   C6    RELEASING.md's required-status-checks `contexts` list covers the two
#         gates it BOLDS as gates — `notices` and every cell of ci.yml's `trivy`
#         matrix (F121). The trivy cells are derived from ci.yml, so adding an
#         image to the scan matrix and forgetting branch protection goes red
#         here instead of merging a red trivy scan.
#   R-02  every client-go `.List(` call in internal/runner/k8s needs a "list"
#         verb on the chart's namespaced Role for that resource, derived from
#         the driver source itself — every k8s test runs against a fake
#         clientset, which enforces no RBAC at all, so this is the only thing
#         that catches the drift before a real cluster 403s.
#
# See #209's PR for lane 2a for the mapping from each deleted check to the Go
# guard that already covers its fact, or the note that nothing did.
#
# Run (no docker, no network, no writes outside mktemp):
#   bash scripts/test-claims-match-code.sh
#
# Wired into `make test-scripts`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() { echo "test-claims-match-code: FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

WORK="$(mktemp -d)"; trap 'rm -rf "${WORK}"' EXIT

# ── C6: the documented branch protection covers the gates it calls gates ────
#
# main's required contexts were `build`, `ui`, `dco` and the five `gates (…)`
# cells — and nothing else. `notices` (the copyleft / unreviewed-dependency
# gate, which RELEASING.md bolds) and `trivy` (the only CVE scan of the five
# images a release publishes) both report on every PR and neither blocked a
# merge. Branch protection is a GitHub-side setting no test can read offline;
# what IS checkable is that the command RELEASING.md tells the maintainer to run
# would set them, and that it stays in step with ci.yml's matrix.
RELEASING="${ROOT}/RELEASING.md"
CI_YML="${ROOT}/.github/workflows/ci.yml"
contexts="$(awk '/"contexts": \[/{f=1} f{print} f&&/\]/{exit}' "${RELEASING}")"
[ -n "${contexts}" ] || fail "RELEASING.md no longer carries a required_status_checks \"contexts\" array — the one place the merge gate is written down (F121)"
trivy_cells="$(awk '/^  trivy:/{f=1} f&&/^  [a-z]/&&!/^  trivy:/{f=0} f' "${CI_YML}" \
               | grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' | awk '{print $3}' | sort -u || true)"
[ -n "${trivy_cells}" ] || fail "could not derive ci.yml's trivy matrix — this guard would compare against an empty list"
for want in notices; do
  printf '%s' "${contexts}" | grep -qF "\"${want}\"" \
    || fail "RELEASING.md's required contexts omit '${want}' — the copyleft / unreviewed-dependency gate merges red (F121)"
done
for cell in ${trivy_cells}; do
  printf '%s' "${contexts}" | grep -qF "\"trivy (${cell})\"" \
    || fail "RELEASING.md's required contexts omit 'trivy (${cell})' — ci.yml scans that image and branch protection would let it merge red. A matrix job reports one context per cell (F121)"
done
pass "C6 RELEASING.md's required contexts cover notices + every ci.yml trivy matrix cell"

# ── R-02: the chart's Role grants every "list" verb the k8s driver issues ───
#
# R-02 (v0.7.4) — the chart's Role and the substrate's API calls drifted apart
# silently: the orphan sweep started LISTING Secrets and NetworkPolicies while
# rbac.yaml still granted neither and its own comment block said "NO get/list".
# Nothing caught it, because every k8s test runs against client-go's fake
# clientset, which enforces no RBAC at all. This reads the verbs the substrate
# actually issues out of the code and requires the rendered rule to carry them.
#
# Scope: the `list` verb on the namespaced Role, keyed on the RESOURCE each
# `.List(` call is made on. Deliberately not a full verb audit — it pins the one
# axis that has now drifted once, and the one a fake clientset can never see.
# Ceiling: the extractor below requires the accessor and `.List(` on the SAME
# line; a call split across lines is invisible to it.
k8s_src="${ROOT}/internal/runner/k8s"
rbac="${ROOT}/deploy/helm/wardyn/templates/rbac.yaml"
[ -d "${k8s_src}" ] && [ -f "${rbac}" ] || fail "internal/runner/k8s or the chart's rbac.yaml moved — this guard would check nothing (R-02)"
# Map a client-go typed accessor to the RBAC resource name it needs a verb on.
# Non-test sources only: a _test.go file lists PVCs through the FAKE clientset
# to assert the driver never created one, which is a test's own bookkeeping and
# not a verb wardynd ever issues (and granting pvc:list would be wrong).
k8s_files="$(find "${k8s_src}" -maxdepth 1 -name '*.go' ! -name '*_test.go' | sort)"
[ -n "${k8s_files}" ] || fail "no non-test .go files under internal/runner/k8s — this guard would check nothing (R-02)"
# shellcheck disable=SC2086
grep -hoE '(CoreV1|NetworkingV1|RbacV1)\(\)\.[A-Za-z]+\([^)]*\)\.List\(' ${k8s_files} \
  | sed -E 's/.*\(\)\.([A-Za-z]+)\(.*/\1/' | sort -u > "${WORK}/k8s-list-kinds"
[ -s "${WORK}/k8s-list-kinds" ] || fail "no .List( call found in internal/runner/k8s — this guard would check nothing (R-02)"
# The Role's rules only (not the cluster-scoped ClusterRole): granting a verb
# there instead would satisfy a whole-file grep while widening every namespace.
awk '/^---/{r=0} /^kind: Role$/{r=1} r' "${rbac}" > "${WORK}/rbac-role"
[ -s "${WORK}/rbac-role" ] || fail "rbac.yaml no longer contains a 'kind: Role' block — this guard would check nothing (R-02)"
while read -r kind; do
  [ -n "${kind}" ] || continue
  # Pods -> pods, Secrets -> secrets, NetworkPolicies -> networkpolicies.
  res="$(printf '%s' "${kind}" | tr 'A-Z' 'a-z')"
  # CAPTURE, THEN MATCH — never `grep | grep -q` under `pipefail`: `grep -q`
  # exits on its first match while the upstream grep is still writing, the
  # upstream takes SIGPIPE, and pipefail reports the pipeline failed even
  # when the match was found.
  _rule_block="$(grep -A1 "resources: \[\"${res}\"\]" "${WORK}/rbac-role" || true)"
  grep -q '"list"' <<<"${_rule_block}" \
    || fail "internal/runner/k8s calls .${kind}(...).List(...) but deploy/helm/wardyn/templates/rbac.yaml's Role grants no \"list\" verb on \"${res}\" — on a real cluster that call 403s (every k8s test uses a fake clientset, which enforces no RBAC) (R-02)"
done < "${WORK}/k8s-list-kinds"
pass "R-02 every resource internal/runner/k8s lists has a 'list' verb on the chart's Role"

echo "test-claims-match-code: self-test PASS"
