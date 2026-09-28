#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# T-41 (#701): hybrid federation beyond Postgres — the laptop-compose-to-
# org-kind walk. What IngestDeviceAudit itself does (the batch-splice refusal,
# duplicate-name MintEnrolmentToken/CreateDevice 23505 arms) is ALREADY
# PROVEN, hermetically, by test/apie2e/federation_test.go against a real
# server on both ends (#1117) — this walk does not repeat it. bootHybrid's
# ResetFederation-after-Put ordering (including every crash-recovery
# permutation) is ALREADY PROVEN by cmd/wardynd/boot_hybrid_test.go's own
# table (TestBootHybrid_ResumesResetAfterCrashBeforeReset and neighbours) —
# not repeated here either. The migration-0078 (formerly numbered 0070 before
# a later renumbering) CONCURRENTLY question is answered in that file's own
# comment, not here. The macOS launchd smoke is OWNER (a real laptop, not a
# CI runner).
#
# What is left, and what THIS walk proves: two REAL wardynd processes, never
# faked — the "org" is `make kind-quickstart` on a real (Calico-enforced) kind
# cluster, exactly the cluster scripts/kind-sso-walk.sh already reuses and
# never creates or deletes itself; the "laptop" is a dedicated docker-compose
# project this script owns end to end.
#
#   1. enrolment across the two: an org admin mints a token, the laptop boots
#      with it and WARDYN_USER_DESKTOP=true, and /healthz.org_federation on
#      the laptop reads enrolled=true;
#   2. rows accrue then drain across a partition: the laptop's route to the
#      org is severed (a relay sidecar this script owns is stopped, not the
#      cluster or the daemon), local audit rows keep landing while
#      wardyn_org_federation_lag grows, then the relay comes back and lag
#      drains to 0 — matching docs/OPERATIONS.md's own description of what an
#      unreachable (vs. a refusing) organisation looks like: growing lag, NO
#      device.audit.ingest failure rows on the org side, because the org
#      never saw the batch;
#   3. a laptop restart keeps its acked_seq: docker-kill/restart the laptop's
#      wardynd mid-forward (a persistent age key, same reason
#      scripts/survival-walk.sh mints one) and confirm org_federation's
#      last_forwarded_seq — read directly from the laptop's OWN Postgres, not
#      inferred from a log line — never resets to 0, so the resumed forwarder
#      never re-sends what the org already acknowledged;
#   4. revocation: the org admin DELETEs the device, the laptop's next tick
#      hits the refusal, /healthz.org_federation.enrolled flips to false, and
#      POST /api/v1/runs on the laptop starts answering 503 with
#      internal/api/org_revocation.go's own message — surviving a further
#      laptop restart, because the revoked mark is durable
#      (MarkFederationRevoked), never re-derived from the org being reachable.
#
# THE REVOCATION STATUS CODE, CORRECTED AGAINST LIVE CODE: docs/ENV.md's own
# WARDYN_ORG_URL row (and this plan's own text) says "401 with the device
# realm, or 410". The code path this walk actually drives
# (internal/api/devices_auth.go's deviceAuth, store.PG.GetDeviceByRaw
# filtering `WHERE revoked_at IS NULL`) answers a revoked device exactly the
# same 401 it gives an UNKNOWN one, by design ("revoked and unknown are one
# 401" — no oracle). internal/federation/client.go's StatusError.Revoked()
# treats that 401 (carrying deviceAuth's own `realm="wardyn-device"`) and a
# bare 410 as the SAME definitive signal; only the 401 arm is reachable from
# any route this repo ships today, so that is what this walk asserts on. A
# future route that answers 410 instead needs no change here — the assertion
# is on the FORWARDER's own revoked state and the laptop's own 503, not on
# the wire status code the org happened to send.
#
# ORG REACHABILITY IS LOOPBACK, DELIBERATELY, NOT A REAL TLS HOP.
# validateHybridPosture (cmd/wardynd/boot_posture.go) requires WARDYN_ORG_URL
# to be https:// UNLESS its host is loopback (listenIsLoopback,
# cmd/wardynd/listen_addr.go — a literal 127.0.0.1 is recognised without a
# lookup). WARDYN_ORG_URL here is http://127.0.0.1:${RELAY_PORT}: genuinely
# loopback FROM WARDYND'S OWN NETWORK NAMESPACE, because the relay it names
# shares that namespace (see "THE PARTITION MECHANISM" below) rather than
# sitting on the host — a host-gateway address is NOT loopback and
# validateHybridPosture correctly refuses it (an earlier version of this
# walk asserted that shape and a review caught both that it would be refused
# and that the relay behind it could not start at all — see below). A real
# cross-network HTTPS proof against a certificate an operator's own CA
# issued is the live-estate half of this ask and stays OWNER, like every
# other real-TLS/real-DNS proof in this handoff.
#
# THE LAPTOP'S OWN OIDC POSTURE. WARDYN_USER_DESKTOP=true needs TWO things
# validated at boot, both in cmd/wardynd/boot_posture.go, and bringing up Dex
# alone satisfies neither — an earlier version of this walk assumed it did,
# and wardynd crash-looped on the first of them in the hosted nightly:
#
#  1. validateMemberModePosture requires WARDYN_OIDC_ISSUER to be actually
#     SET (compose's own default is empty, docker-compose.yaml — bringing up
#     the `dex` container does not set it for you). compose() sets it to
#     "http://localhost:5556", the literal `issuer:` in deploy/compose/
#     dex.yaml — the PUBLIC, browser-facing issuer string, which is what this
#     var means. wardynd itself never dials "localhost" for discovery: the
#     compose file's own WARDYN_OIDC_INTERNAL_ISSUER default (http://dex:5556)
#     is what internal/auth/oidc's split-horizon rewrite actually calls (the
#     SAME self-contained fixture scripts/compose-sso-roles.sh already proves
#     nightly), so this is a real discovery against a real issuer, not a
#     fake one.
#  2. validateOperatorPosture then refuses an OIDC deployment with an empty
#     operator allowlist ("every signed-in human would be admin-equivalent").
#     compose() sets WARDYN_OIDC_OPERATOR_EMAILS="admin@wardyn.local" to clear
#     it — an address that never actually signs in (see below), so its exact
#     value only has to be non-empty and does not need to correspond to a
#     Dex user.
#
# Nothing in this walk drives an actual human sign-in — the device-credential
# and revocation paths never touch OIDC at all (devices_auth.go's own header:
# device routes never publish a human identity) — so Dex only has to answer
# discovery, never issue a token here.
#
# THE PARTITION MECHANISM: a `socat` relay running as a SIDECAR CONTAINER,
# listening on 127.0.0.1 inside a shared network namespace — reachable from
# nothing else on the host or the laptop's compose network, and never bound
# wide (0.0.0.0 would republish the org's NodePort on every interface, which
# an earlier version of this walk did and a review caught). This is now
# wardynd's OWN loopback because wardynd shares that namespace too (see "THE
# ANCHOR" below), not because the relay joined wardynd's container directly —
# `docker run --network container:X --add-host ...` is refused outright by
# the daemon ("conflicting options: custom host-to-IP mapping and the network
# mode", exit 125), which an earlier version of this walk did not catch
# because it never actually ran the command it claimed would work.
#
# THE ROUTE TO THE ORG: not the host at all. host.docker.internal does not
# resolve on native Linux without an explicit --add-host (which the relay
# cannot combine with --network container:), and even where it resolves it
# names the bridge gateway, which cannot reach a NodePort published on the
# HOST's 127.0.0.1 only. Instead the shared namespace joins the `kind` docker
# network (`docker network connect kind ${ANCHOR_CONTAINER}` — the same
# network every kind cluster's own nodes are already on) and the relay
# forwards to the org's node CONTAINER directly by name and CONTAINER port
# (`${CONTROL_PLANE_CONTAINER}:${ORG_NODE_CONTAINER_PORT}`, deploy/kind/
# quickstart.sh's own fixed NodePort) — a plain container-to-container hop
# on a shared bridge network, nothing to do with the host's loopback
# publish. start_relay confirms this end to end (a real request through the
# relay to the org, from a container in the shared namespace) rather than
# only checking the relay container is Running, so a dead route fails loudly
# at start instead of surfacing later as a stuck enrolment.
#
# THE ANCHOR: see netns-anchor in test/hybrid-walk/compose-override.yaml —
# wardynd cannot own this namespace itself, because its own `restart:
# unless-stopped` would hand the relay a dead namespace on every restart,
# and its FIRST boot enrols synchronously (cmd/wardynd/boot_hybrid.go)
# before this script could ever detect and restart the relay fast enough.
# The anchor is a `sleep infinity` container with nothing to crash; the
# relay and the `kind` network connection are both established against it
# BEFORE wardynd's own container is created, so wardynd's first enrolment
# attempt never races the relay's own startup, and none of wardynd's later
# restarts (docker-kill/restart in sections 3 and 4 below) touch the relay
# at all — it stays attached to the anchor throughout the whole walk.
#
# Partition = stop the relay container (ECONNREFUSED from inside the shared
# namespace, the "organisation unreachable" shape); drain = start a fresh
# one on the same port. Neither ever touches the kind cluster, the NodePort
# or the daemon socket. The relay itself runs capability-dropped, read-only
# and as an unprivileged uid (start_relay) — a fixed socat command inside a
# namespace that can otherwise reach everything wardynd can, so this is
# defence in depth, not a functional requirement.
#
# GUARD: needs BOTH a kind cluster (WARDYN_TEST_K8S=1) and Docker
# (WARDYN_TEST_DOCKER=1) — this walk is the one script in this repo that
# spans both substrates at once, so it self-skips unless both are
# acknowledged. Never creates or deletes the kind cluster itself — it mints
# enrolment tokens/devices on whatever wardyn-quickstart cluster is already
# up, so a hand run must also set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm
# that cluster is disposable (see the guard below); nightly.yml sets it for
# the hosted run, which always creates its own.
#
# NOT RUN OR PROVEN IN THIS AUTHORING PASS: no kind cluster and no Docker
# execution were available to write it (see this repo's own lane rules for
# why) — bash -n and the repo's shellcheck-equivalent lint are what this pass
# can offer; the hosted nightly is what proves the mechanism end to end. See
# this PR's own UNVERIFIED list.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" || "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  skip_lane "hybrid-walk: set WARDYN_TEST_K8S=1 AND WARDYN_TEST_DOCKER=1 to run the laptop<->org hybrid federation walk (skipping)."
fi

# This walk mutates whatever wardyn-quickstart cluster is already up (mints
# enrolment tokens and devices on it) — never creates or deletes it itself,
# unlike scripts/kind-upgrade-walk.sh's own throwaway cluster. Fine on a
# hosted nightly runner, which never has a pre-existing cluster of its own;
# a hand run on a long-lived dev machine must say explicitly that the cluster
# it will find is disposable, so an unrelated cluster of the same name is
# never mutated by accident.
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "hybrid-walk: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the wardyn-quickstart cluster this walk finds may be mutated (it mints and revokes enrolment tokens/devices on it) — refusing to run against a cluster it did not create otherwise."
fi

# One daemon everywhere, same reasoning as kind-sso-walk.sh: the kind node and
# the laptop's own containers must be reachable from the same docker context.
wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
pass() { printf '  \033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m[FAIL]\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0
CURL_MAX_TIME="${WARDYN_HYBRID_CURL_MAX_TIME:-10}"

for bin in docker curl jq kubectl; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

# ── the org: must already be up — this script never creates or deletes it ──
CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
ORG_NAMESPACE="wardyn"
ORG_RELEASE="wardyn"
ORG_NODE_HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
# kind's own node-container naming (every kind cluster's control-plane node is
# a docker container named "<cluster>-control-plane" on the docker network
# named "kind") and quickstart.sh's own FIXED NodePort (deploy/kind/
# quickstart.sh:75 — never overridden by WARDYN_QUICKSTART_HTTP_PORT, which
# only controls the HOST-side publish deploy/kind/quickstart-kind-config.yaml
# rewrites onto that Service). This is the relay's route to the org — a
# container-to-container hop on the `kind` network, not the host loopback
# publish above (which the script itself, running ON the host, still uses).
CONTROL_PLANE_CONTAINER="${CLUSTER}-control-plane"
ORG_NODE_CONTAINER_PORT=30080
KIND_NETWORK="kind"

step "checking the org cluster is up (this walk never creates or deletes it)"
kubectl --context "${CONTEXT}" -n "${ORG_NAMESPACE}" get deployment "${ORG_RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_HTTP_PORT=${ORG_NODE_HTTP_PORT} make kind-quickstart"

ORG_ADMIN_TOKEN="$(kubectl --context "${CONTEXT}" -n "${ORG_NAMESPACE}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${ORG_ADMIN_TOKEN}" ]] || die "could not read the org admin token from Secret wardyn-auth"
ORG_BASE="http://127.0.0.1:${ORG_NODE_HTTP_PORT}"
curl -sf --max-time "${CURL_MAX_TIME}" "${ORG_BASE}/healthz" >/dev/null 2>&1 \
  || die "${ORG_BASE}/healthz did not answer — is the NodePort published on 127.0.0.1:${ORG_NODE_HTTP_PORT}?"
pass "org cluster reachable at ${ORG_BASE}"

# org_api METHOD PATH [JSON-BODY] -> writes the body to $TMPDIR/org-resp.json,
# prints the HTTP status code on stdout.
org_api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/org-resp.json" -w '%{http_code}' -X "${method}" "${ORG_BASE}${path}" \
      -H "Authorization: Bearer ${ORG_ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/org-resp.json" -w '%{http_code}' -X "${method}" "${ORG_BASE}${path}" \
      -H "Authorization: Bearer ${ORG_ADMIN_TOKEN}"
  fi
}

# ── the laptop: a dedicated compose project this script owns ───────────────
PROJECT="${WARDYN_HYBRID_PROJECT:-cl-701-hybrid}"
COMPOSE_FILE="${ROOT}/deploy/compose/docker-compose.yaml"
OVERRIDE_FILE="${ROOT}/test/hybrid-walk/compose-override.yaml"
API_PORT="${WARDYN_HYBRID_API_PORT:-18190}"
PG_PORT="${WARDYN_HYBRID_PG_PORT:-15533}"
REGISTRY_PORT="${WARDYN_HYBRID_REGISTRY_PORT:-15111}"
DEX_PORT="${WARDYN_HYBRID_DEX_PORT:-15577}"
SSH_PORT="${WARDYN_HYBRID_SSH_PORT:-12323}"
UI_SANDBOX_PORT="${WARDYN_HYBRID_UI_SANDBOX_PORT:-18191}"
RELAY_PORT="${WARDYN_HYBRID_RELAY_PORT:-18192}"
BASE="http://127.0.0.1:${API_PORT}"
ADMIN_TOKEN="demo-admin-token"
WARDYND_IMAGE="wardyn/wardynd:${PROJECT}"
PROXY_IMAGE="wardyn/wardyn-proxy:${PROJECT}"
RELAY_IMAGE="wardyn/hybrid-relay:${PROJECT}"
WARDYND_CONTAINER="${PROJECT}-api"
ANCHOR_CONTAINER="${PROJECT}-netns-anchor"
RELAY_CONTAINER="${PROJECT}-relay"

EVIDENCE_DIR="${WARDYN_HYBRID_EVIDENCE:-${ROOT}/local/evidence/hybrid-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-hybrid-walk.XXXXXX)"

compose() {
  COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT="${API_PORT}" WARDYN_PG_PORT="${PG_PORT}" WARDYN_REGISTRY_PORT="${REGISTRY_PORT}" \
    WARDYN_DEX_PORT="${DEX_PORT}" WARDYN_SSH_PORT="${SSH_PORT}" WARDYN_UI_SANDBOX_PORT="${UI_SANDBOX_PORT}" \
    WARDYN_WARDYND_IMAGE="${WARDYND_IMAGE}" WARDYN_PROXY_IMAGE="${PROXY_IMAGE}" \
    WARDYN_USER_DESKTOP="true" WARDYN_LOCAL_MODE="false" \
    WARDYN_OIDC_ISSUER="http://localhost:5556" WARDYN_OIDC_OPERATOR_EMAILS="admin@wardyn.local" \
    WARDYN_ORG_URL="http://127.0.0.1:${RELAY_PORT}" \
    WARDYN_ORG_ENROLMENT_TOKEN="${ENROL_TOKEN:-}" \
    docker compose -p "${PROJECT}" -f "${COMPOSE_FILE}" -f "${OVERRIDE_FILE}" --profile sso "$@"
}

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}

psql1() { docker exec "${PROJECT}-postgres" psql -U wardyn -d wardyn -tAc "$1" 2>&1; }

DEVICE_ID=""

# start_relay -> a socat sidecar sharing the netns-anchor container's network
# namespace (`--network container:${ANCHOR_CONTAINER}` — never wardynd's own
# container: `--network container:X` and `--add-host` are mutually exclusive
# at the daemon level, "conflicting options: custom host-to-IP mapping and
# the network mode", and wardynd needs an --add-host-free route to the org
# anyway — see this file's "THE ROUTE TO THE ORG"). 127.0.0.1 inside that
# namespace is wardynd's own loopback too, once wardynd joins the same
# namespace via network_mode: service:netns-anchor (test/hybrid-walk/
# compose-override.yaml) — reachable from nothing else, never 0.0.0.0.
# Hardened (R2-3): no capabilities, no privilege escalation, a read-only
# rootfs and an unprivileged uid — a fixed socat command needs none of them,
# and the namespace it shares can otherwise reach everything wardynd can.
# Verifies the FULL route end to end (a real request through the relay to
# the org, from inside the anchor's own namespace) rather than only that the
# container is Running, so a route that cannot reach the org fails loudly
# here instead of surfacing later as a stuck enrolment.
start_relay() {
  docker run --rm -d --name "${RELAY_CONTAINER}" \
    --network "container:${ANCHOR_CONTAINER}" \
    --cap-drop ALL --security-opt no-new-privileges --read-only --user 65532:65532 \
    "${RELAY_IMAGE}" \
    "TCP-LISTEN:${RELAY_PORT},bind=127.0.0.1,fork,reuseaddr" "TCP:${CONTROL_PLANE_CONTAINER}:${ORG_NODE_CONTAINER_PORT}" \
    >"${EVIDENCE_DIR}/relay-start.log" 2>&1 \
    || { cat "${EVIDENCE_DIR}/relay-start.log" >&2; die "starting the relay sidecar (${RELAY_CONTAINER}) failed"; }
  sleep 1
  [[ "$(docker inspect -f '{{.State.Running}}' "${RELAY_CONTAINER}" 2>/dev/null)" == "true" ]] \
    || { docker logs "${RELAY_CONTAINER}" >"${EVIDENCE_DIR}/relay.log" 2>&1 || true; cat "${EVIDENCE_DIR}/relay.log" >&2; die "the relay sidecar (${RELAY_CONTAINER}) on :${RELAY_PORT} did not stay up"; }
  docker exec "${ANCHOR_CONTAINER}" wget -qO- -T "${CURL_MAX_TIME}" "http://127.0.0.1:${RELAY_PORT}/healthz" \
    >"${EVIDENCE_DIR}/relay-healthz.log" 2>&1 \
    || { cat "${EVIDENCE_DIR}/relay-healthz.log" >&2; die "the relay is running but a request through it to ${CONTROL_PLANE_CONTAINER}:${ORG_NODE_CONTAINER_PORT}/healthz failed — is the anchor connected to the ${KIND_NETWORK} network?"; }
}

stop_relay() {
  docker stop "${RELAY_CONTAINER}" >/dev/null 2>&1 || true
}

teardown() {
  compose logs wardynd >"${EVIDENCE_DIR}/wardynd.log" 2>&1 || true
  docker logs "${RELAY_CONTAINER}" >"${EVIDENCE_DIR}/relay.log" 2>&1 || true
  stop_relay
  echo "tearing down ${PROJECT} (compose down --volumes; this project only)"
  compose down --volumes >/dev/null 2>&1 || true
  docker rm -f "${RELAY_CONTAINER}" >/dev/null 2>&1 || true
  # Best-effort hygiene on the shared org cluster: revoke (never delete — this
  # script only ever has security-admin-shaped calls through the admin
  # token) the device this run enrolled, so a re-run of this walk against the
  # same long-lived cluster does not accumulate live rows.
  if [[ -n "${DEVICE_ID}" ]]; then
    org_api DELETE "/api/v1/admin/devices/${DEVICE_ID}" >/dev/null 2>&1 || true
  fi
  rm -rf "${TMPDIR}"
}
trap teardown EXIT

# Clean stragglers from a prior aborted run of THIS script only.
compose down --volumes >/dev/null 2>&1 || true
docker rm -f "${RELAY_CONTAINER}" >/dev/null 2>&1 || true

# ── 1. mint an enrolment token, boot the laptop, confirm enrolment ─────────
step "minting an enrolment token for a new device (org admin)"
DEVICE_NAME="hybrid-walk-$(date -u +%Y%m%dT%H%M%SZ)"
code=$(org_api POST /api/v1/admin/devices/enrolment-tokens "$(jq -nc --arg n "${DEVICE_NAME}" '{name:$n}')")
[[ "${code}" == "201" ]] || { cat "${TMPDIR}/org-resp.json" >&2; die "POST /admin/devices/enrolment-tokens answered ${code}"; }
ENROL_TOKEN="$(jq -r '.token' "${TMPDIR}/org-resp.json")"
[[ -n "${ENROL_TOKEN}" && "${ENROL_TOKEN}" != "null" ]] || die "mint response carried no token"
pass "token minted for device \"${DEVICE_NAME}\""

step "minting a persistent age key for the laptop (a restart must still read what it captured)"
compose build wardynd >"${EVIDENCE_DIR}/build-wardynd.log" 2>&1 || { tail -40 "${EVIDENCE_DIR}/build-wardynd.log" >&2; die "build ${WARDYND_IMAGE} failed"; }
compose --profile build-only build proxy-image >"${EVIDENCE_DIR}/build-proxy.log" 2>&1 || { tail -40 "${EVIDENCE_DIR}/build-proxy.log" >&2; die "build ${PROXY_IMAGE} failed"; }
docker build -q -f "${ROOT}/test/hybrid-walk/Dockerfile.relay" -t "${RELAY_IMAGE}" "${ROOT}/test/hybrid-walk" \
  >"${EVIDENCE_DIR}/build-relay.log" 2>&1 || { tail -40 "${EVIDENCE_DIR}/build-relay.log" >&2; die "build ${RELAY_IMAGE} failed"; }
export WARDYN_AGE_KEY="$(docker run --rm "${WARDYND_IMAGE}" -gen-age-key 2>"${EVIDENCE_DIR}/gen-age-key.log")"
[[ "${WARDYN_AGE_KEY}" == AGE-SECRET-KEY-* ]] || { cat "${EVIDENCE_DIR}/gen-age-key.log" >&2; die "-gen-age-key failed"; }
pass "age key minted"

# The anchor, the kind-network route and the relay are all brought up and
# PROVEN reachable BEFORE wardynd exists at all (this file's "THE ANCHOR"),
# so wardynd's very first enrolment attempt never races the relay's own
# startup.
step "starting the network-namespace anchor and connecting it to the ${KIND_NETWORK} docker network"
compose up -d netns-anchor || die "compose up (netns-anchor) failed for ${PROJECT}"
docker network connect "${KIND_NETWORK}" "${ANCHOR_CONTAINER}" 2>"${EVIDENCE_DIR}/network-connect.log" \
  || grep -qi 'already exists in network' "${EVIDENCE_DIR}/network-connect.log" \
  || { cat "${EVIDENCE_DIR}/network-connect.log" >&2; die "connecting ${ANCHOR_CONTAINER} to the ${KIND_NETWORK} network failed"; }
pass "anchor up (${ANCHOR_CONTAINER}) and on the ${KIND_NETWORK} network"

step "starting the relay sidecar and proving the route to the org (${CONTROL_PLANE_CONTAINER}:${ORG_NODE_CONTAINER_PORT})"
start_relay
pass "relay up (${RELAY_CONTAINER}) — a real request through it to the org's /healthz succeeded"

step "bringing up the laptop (postgres, dex, wardynd) with WARDYN_ORG_URL + WARDYN_ORG_ENROLMENT_TOKEN set"
compose up -d postgres dex || die "compose up (postgres, dex) failed for ${PROJECT}"
compose up -d wardynd || die "compose up (wardynd) failed for ${PROJECT}"
pass "laptop wardynd container up (${WARDYND_CONTAINER}, sharing ${ANCHOR_CONTAINER}'s network namespace)"

wait_healthy "${BASE}" 60 2 || { compose logs wardynd | tail -80; die "the laptop's wardynd did not become healthy"; }
pass "laptop up and healthy (${BASE})"

step "confirming enrolment landed (/healthz.org_federation.enrolled=true)"
enrolled=""
for _ in $(seq 1 30); do
  h="$(curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz" 2>/dev/null || true)"
  [[ "$(jq -r '.org_federation.enrolled // empty' <<<"${h}")" == "true" ]] && { enrolled=1; break; }
  sleep 1
done
[[ -n "${enrolled}" ]] || { compose logs wardynd | tail -80; die "the laptop never reported org_federation.enrolled=true"; }
DEVICE_ID="$(org_api GET "/api/v1/admin/devices" >/dev/null; jq -r --arg n "${DEVICE_NAME}" '.[] | select(.name==$n) | .id' "${TMPDIR}/org-resp.json")"
[[ -n "${DEVICE_ID}" ]] || die "could not find ${DEVICE_NAME} in the org's device inventory after enrolment"
pass "enrolled — device_id=${DEVICE_ID}"

# lag -> the laptop's own wardyn_org_federation_lag reading, or empty on any
# failure to reach it (the caller decides what that means). /metrics is
# operator-gated (routes.go), so the scrape carries the laptop's admin token.
lag() {
  curl -sf --max-time "${CURL_MAX_TIME}" -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/metrics" 2>/dev/null \
    | awk '/^wardyn_org_federation_lag /{print $2}'
}

# accrue_local_rows N -> create and immediately kill N throwaway runs on the
# laptop, purely to advance its own local audit_events table (each create +
# kill is at least two audited rows). Never asserts on run outcome — a
# refused create (e.g. no agent standard declared) still writes an audit row,
# which is all this needs.
accrue_local_rows() {
  local n="$1" i rid
  for ((i = 0; i < n; i++)); do
    api POST /api/v1/runs '{"agent":"claude-code","repo":"local:hybrid","interactive":true,
      "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}' >/dev/null
    rid="$(jq -r '.id // empty' "${TMPDIR}/resp.json")"
    [[ -n "${rid}" ]] && curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1
  done
}

# ── 2. partition: rows accrue while unreachable, then drain ─────────────────
step "partitioning the laptop from the org (killing the relay) and accruing local audit rows"
stop_relay
before_lag="$(lag)"
accrue_local_rows 5
sleep 20 # at least one 15s forwarder tick with the relay down
after_lag="$(lag)"
[[ -n "${after_lag}" && -n "${before_lag}" ]] || die "could not read wardyn_org_federation_lag from ${BASE}/metrics"
awk -v b="${before_lag}" -v a="${after_lag}" 'BEGIN{exit !(a>b)}' \
  && pass "lag grew while partitioned (${before_lag} -> ${after_lag})" \
  || fail "expected lag to grow while partitioned; before=${before_lag} after=${after_lag}"

# THE UNREACHABLE SIGNATURE, ASSERTED ON THE ORG SIDE TOO: an unreachable
# organisation produces NO device.audit.ingest rows at all (it never saw the
# batch) — a refusal would. Corroborates that the growing lag above is really
# "unreachable", not a refusal this walk is misreading.
audit_code="$(org_api GET "/api/v1/audit?action=device.audit.ingest&limit=50")"
[[ "${audit_code}" == "200" ]] || { cat "${TMPDIR}/org-resp.json" >&2; die "GET /api/v1/audit answered ${audit_code}"; }
# `jq -r 'length'` on a non-array (an object, or null) still returns a small
# integer — 0 for {} or null — which would read as "zero rows" exactly like
# a genuinely empty array. Require the body to actually BE a JSON array first.
jq -e 'type=="array"' "${TMPDIR}/org-resp.json" >/dev/null 2>&1 \
  || die "GET /api/v1/audit did not answer a JSON array: $(cat "${TMPDIR}/org-resp.json")"
ingest_events="$(jq -r 'length' "${TMPDIR}/org-resp.json")"
[[ "${ingest_events}" =~ ^[0-9]+$ ]] || die "GET /api/v1/audit's length was not a number: ${ingest_events}"
[[ "${ingest_events}" == "0" ]] \
  && pass "no device.audit.ingest rows on the org while partitioned (unreachable, not refused)" \
  || fail "expected zero device.audit.ingest rows while partitioned; got ${ingest_events}"

step "draining: restarting the relay and waiting for lag to fall back to 0"
start_relay
drained=""
for _ in $(seq 1 60); do
  [[ "$(lag)" == "0" ]] && { drained=1; break; }
  sleep 2
done
[[ -n "${drained}" ]] || { compose logs wardynd | tail -80; die "lag never drained back to 0 after the relay came back (last read: $(lag))"; }
pass "drained — lag back to 0"

# ── 3. laptop restart mid-forward keeps acked_seq ───────────────────────────
step "accruing more rows, then killing wardynd mid-forward (before the next tick acks them)"
accrue_local_rows 5
seq_before_restart="$(psql1 "SELECT last_forwarded_seq FROM org_federation WHERE singleton")"
# The relay stays attached to the netns-anchor container throughout, never to
# wardynd's own container, so killing and recreating wardynd here does not
# touch the relay at all — no stop/start choreography needed around this
# restart (contrast the partition/drain pair above, which stops and starts
# the relay deliberately, as the test itself).
docker kill "${PROJECT}-api" >/dev/null || die "docker kill ${PROJECT}-api failed"
sleep 3
[[ "$(docker inspect -f '{{.State.Running}}' "${PROJECT}-api" 2>/dev/null)" == "false" ]] || die "${PROJECT}-api is still running after docker kill"
compose up -d wardynd || die "compose up -d wardynd (restart) failed"
wait_healthy "${BASE}" 60 2 || { compose logs wardynd | tail -80; die "wardynd did not come back healthy after restart"; }
seq_after_restart="$(psql1 "SELECT last_forwarded_seq FROM org_federation WHERE singleton")"
# psql1 merges stderr (:203), so a psql/docker-exec error string would
# otherwise read as a number to awk (both sides equal, a>=b true) — require
# both reads to actually be integers before comparing them.
if [[ "${seq_before_restart}" =~ ^[0-9]+$ && "${seq_after_restart}" =~ ^[0-9]+$ ]]; then
  awk -v b="${seq_before_restart}" -v a="${seq_after_restart}" 'BEGIN{exit !(a>=b)}' \
    && pass "org_federation.last_forwarded_seq did not reset on restart (${seq_before_restart} -> ${seq_after_restart})" \
    || fail "expected last_forwarded_seq to never go backwards across a restart; before=${seq_before_restart} after=${seq_after_restart}"
else
  fail "could not read last_forwarded_seq as a number around the restart; before='${seq_before_restart}' after='${seq_after_restart}'"
fi
resumed=""
for _ in $(seq 1 60); do
  [[ "$(lag)" == "0" ]] && { resumed=1; break; }
  sleep 2
done
[[ -n "${resumed}" ]] || fail "forwarding never resumed to lag=0 after the restart (last read: $(lag))"
[[ -n "${resumed}" ]] && pass "forwarding resumed after the restart"

# ── 4. revocation: 503 + enrolled=false, durable across a restart ──────────
step "revoking the device at the org"
code="$(org_api DELETE "/api/v1/admin/devices/${DEVICE_ID}")"
[[ "${code}" == "204" || "${code}" == "200" ]] || { cat "${TMPDIR}/org-resp.json" >&2; die "DELETE /admin/devices/${DEVICE_ID} answered ${code}"; }
pass "revoked at the org"

step "waiting for the laptop's next tick to observe the revocation"
revoked=""
for _ in $(seq 1 30); do
  h="$(curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz" 2>/dev/null || true)"
  [[ "$(jq -r '.org_federation.enrolled // empty' <<<"${h}")" == "false" ]] && { revoked=1; break; }
  sleep 2
done
[[ -n "${revoked}" ]] || { compose logs wardynd | tail -80; die "the laptop never reported org_federation.enrolled=false after revocation"; }
pass "laptop observed the revocation (/healthz.org_federation.enrolled=false)"

step "confirming POST /runs on the laptop now answers 503 (org_revocation.go's errOrgRevoked)"
code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:hybrid","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}')
[[ "${code}" == "503" ]] \
  && pass "POST /runs refuses 503 once revoked" \
  || fail "expected POST /runs to answer 503 after revocation; got ${code}: $(cat "${TMPDIR}/resp.json")"

step "confirming the revoked mark survives a laptop restart (durable, not re-derived from reachability)"
docker kill "${PROJECT}-api" >/dev/null || die "docker kill ${PROJECT}-api failed"
sleep 3
compose up -d wardynd || die "compose up -d wardynd (restart) failed"
wait_healthy "${BASE}" 60 2 || { compose logs wardynd | tail -80; die "wardynd did not come back healthy after the post-revocation restart"; }
code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:hybrid","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}')
[[ "${code}" == "503" ]] \
  && pass "still refuses 503 after a restart — the revoked mark is durable" \
  || fail "expected 503 to survive a restart; got ${code}"

step "evidence (including wardynd.log and relay.log, saved by teardown) is in ${EVIDENCE_DIR}"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "hybrid-walk: FAILED — see ${EVIDENCE_DIR}" >&2
  exit 1
fi
echo "hybrid-walk: PASS — evidence in ${EVIDENCE_DIR}"
