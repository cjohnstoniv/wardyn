#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# V02c — One command to a cluster. The TERMINAL half (beats 1-4): from an empty
# host to a governed multi-user control plane. The browser half —
# ui/e2e/demo/02c-one-command-to-a-cluster.spec.ts — signs in through Dex and
# walks the forced Getting Started; record-demo.sh joins the two segments,
# terminal first, and merges both narration timelines.
#
#     WARDYN_DEMO_SKIP_MODEL=1 scripts/record-demo.sh --video 02c \
#       --terminal-script scripts/demo-beats/02c-one-command-to-a-cluster.sh
#
# WHAT THIS FILMS.
#   1. `make kind-quickstart` (port-parameterized)   an entire cluster + chart
#      install from nothing — the title's one command, in real time
#   2. the SSO overlay (deploy/kind/sso/)            Dex + the chart re-rendered
#      on OIDC: the flip from "a token" to "your people sign in"
#   3. the split-horizon port-forward                the one line that makes the
#      browser and the cluster agree on who the issuer is
#   4. `kubectl get pods -n wardyn`                  control plane + identity
#      provider, Running, before a browser ever opens
#
# PACING NOTE (persona round adjudicates): beat 1 ships in real time — the
# terminal lane has no fast-forward (record-demo.sh skips ffwd whenever a
# terminal segment exists). The build is the honest cost of the title's claim;
# if the round bills it, the trim is a take-side cut, not a staged skip.
#
# ═══ OPERATOR STAGING — checked by --preflight, which films nothing ═══
#  1. NO wardyn-quickstart CLUSTER EXISTS. This script's whole subject is the
#     install; a leftover cluster means a retake — run `make kind-down` first
#     (preflight refuses rather than deleting anything itself).
#  2. Ports 8280 (console), 2322 (ssh), 5557 (dex) are free.
#  3. docker + kind + helm + kubectl are on PATH (quickstart checks too; the
#     camera should not be rolling for a missing-binary error).
#  3b. DOCKER_HOST in the take's shell points at the daemon that owns your
#     kind clusters (on the recording box: the wardyn daemon,
#     unix:///var/run/wardyn-docker.sock). The typed command stays generic —
#     the daemon choice is staging, not film.
#  4. The operator is in the room (gdigrab lane — V11/V12/V13's rule 9): one
#     clear terminal in the capture rectangle, notifications off.

set -uo pipefail

_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${_HERE}/../.." && pwd)"
cd "${REPO_ROOT}" || exit 1

# shellcheck source=../demo-typist.sh
. "${_HERE}/../demo-typist.sh"

CONTEXT="kind-wardyn-quickstart"
NAMESPACE="wardyn"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
SSH_PORT="${WARDYN_QUICKSTART_SSH_PORT:-2322}"
DEX_PORT=5557
WORK_DIR="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-02c}"
PF_PID_FILE="${WORK_DIR}/v02c-dex-port-forward.pid"

die() { printf '\n\033[1;31mv02c: %s\033[0m\n' "$*" >&2; exit 1; }

preflight() {
  command -v docker >/dev/null || die "docker not on PATH"
  command -v kind   >/dev/null || die "kind not on PATH"
  command -v helm   >/dev/null || die "helm not on PATH"
  command -v kubectl >/dev/null || die "kubectl not on PATH"
  if kind get clusters 2>/dev/null | grep -qx "wardyn-quickstart"; then
    die "a wardyn-quickstart cluster already exists — this video films the install; run 'make kind-down' first"
  fi
  local p
  for p in "${HTTP_PORT}" "${SSH_PORT}" "${DEX_PORT}"; do
    if (exec 3<>"/dev/tcp/127.0.0.1/${p}") 2>/dev/null; then
      exec 3>&- 3<&-
      die "port ${p} is in use — the take needs ${HTTP_PORT}/${SSH_PORT}/${DEX_PORT} free"
    fi
  done
  mkdir -p "${WORK_DIR}"
  echo "v02c preflight: clean — no cluster, ports free, binaries present"
}

if [[ "${1:-}" == "--preflight" ]]; then preflight; exit 0; fi
preflight

# ---------------------------------------------------------------------------
# Beat 1 — the one command
# ---------------------------------------------------------------------------
chapter "One command to a cluster" "From an empty host to a governed control plane"
say "No Wardyn is running anywhere on this machine."
say "One command. A real Kubernetes cluster, the chart, and the control plane."
type_cmd "WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} WARDYN_QUICKSTART_SSH_PORT=${SSH_PORT} make kind-quickstart"
say "That's the whole install. What you just watched build is disposable — and identical in shape to a production chart install."

# ---------------------------------------------------------------------------
# Beat 2 — from a token to your people: the SSO overlay
# ---------------------------------------------------------------------------
say "Out of the box it trusts one admin token. A team install trusts an identity provider instead."
type_cmd "kubectl --context ${CONTEXT} apply -f deploy/kind/sso/dex.yaml"
type_cmd "helm --kube-context ${CONTEXT} upgrade wardyn deploy/helm/wardyn -n ${NAMESPACE} --reuse-values -f deploy/kind/sso/values.yaml"
type_cmd "kubectl --context ${CONTEXT} -n ${NAMESPACE} rollout status deploy/wardyn --timeout=180s"
say "Same chart, re-rendered on OIDC. Who is an admin and who is a member is one role-map line in those values."

# ---------------------------------------------------------------------------
# Beat 3 — split horizon, one line
# ---------------------------------------------------------------------------
say "The cluster reaches the identity provider by its service name. Your browser needs its own road."
type_cmd "kubectl --context ${CONTEXT} -n ${NAMESPACE} port-forward svc/wardyn-dex ${DEX_PORT}:5556 >/dev/null 2>&1 & echo \$! > '${PF_PID_FILE}'; sleep 2; echo forwarding :${DEX_PORT}"

# ---------------------------------------------------------------------------
# Beat 4 — proof before a browser opens
# ---------------------------------------------------------------------------
type_cmd "kubectl --context ${CONTEXT} -n ${NAMESPACE} get pods"
say "Control plane, database, and the identity provider. All of it pods; none of it touched a browser yet."
say "Now the console — localhost ${HTTP_PORT}. The first thing it does to an unfinished install is refuse to let you wander."
