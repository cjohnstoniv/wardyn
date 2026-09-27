#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# T-13 / #673's remaining half: the fake-kind SSO walk (scripts/kind-sso-walk.sh)
# still configures a Bedrock run through the LEGACY operator boot knobs
# (WARDYN_BEDROCK_REGION/MODEL/BASE_URL, set once via `helm upgrade --set`) —
# the pre-0.8 lane resolveBedrockAuth reads (runs_bedrock.go). Since MP-9
# (feat/530-bedrock-provider-dispatch) landed, a run may instead be credentialed
# by an admin-configured PROVIDER RECORD: PUT /model-providers seeds the
# bedrock_sso/bedrock_bearer row, PUT /agent-providers names it an agent's
# DEFAULT, and each person's own credential is captured UID-keyed
# (wardyn-provider-<uid>-{key,sso}, never the admin-chosen id — a provider
# deleted and re-added under the same id starts with nobody's credential; see
# internal/api/model_provider_credentials.go). resolveProviderLane then reaches
# Bedrock on the PROVIDER's own region/model/base_url (provider_bedrock.go),
# never s.cfg.BedrockRegion/Model/BaseURL — the walk's real migration target.
#
# Rebuilding the whole kind cluster walk (deploy/kind/quickstart.sh +
# deploy/kind/sso/overlay.sh + five rebuilt images + a live AWS-SSO Playwright
# capture re-pointed at a provider's UID) is a materially larger, riskier change
# than this issue's remaining scope affords in one pass — see LEDGER.md's #673
# row for the explicit deferral. What was actually missing, and what THIS
# script proves against the real compiled daemon (no kind, no docker sandbox,
# WARDYN_RUNNER=none — headless API-only, exactly what seeding needs), is that
# the provider-record path a rebuilt walk would seed:
#
#   1. WRITES: PUT /model-providers + PUT /agent-providers wire a working
#      roster default, read back correctly (server-minted UID and all).
#   2. NEVER TOUCHES THE LEGACY ENV: none of WARDYN_BEDROCK_REGION/MODEL/
#      BASE_URL reach the daemon's own environment — read back off
#      /proc/<pid>/environ, not asserted by absence-of-a-flag.
#   3. CAPTURES UID-BACKED, NOT ID-BACKED: a person's own credential PUT
#      against the provider id shows up as that provider's connected_people —
#      and, the distinguishing case, deleting the provider and re-adding it
#      under the SAME id (a fresh server-minted UID, the id unchanged) leaves
#      that old capture unreachable. id-keying and UID-keying agree until the
#      UID changes under a fixed id; only this case tells them apart.
#   4. PROPAGATES FAILURE: a roster default naming no real provider, and
#      clearing a provider a roster still defaults to, are BOTH refused
#      (400) — proven against the live doors, not asserted from reading the
#      source.
#
# The HTTP test hatch (WARDYN_ALLOW_TEST_ENDPOINTS, MP-9's own pin —
# internal/api/llm_gateway_test.go::TestValidateModelProviders_BedrockHTTPNeedsTestHatch)
# is exercised as a rebuilt walk would need it: the seeded provider's
# bedrock.base_url is plain http://, accepted only because this daemon sets
# the hatch — never disabled here, never routed around.
#
# Everything this script starts is uniquely named/ported so it cannot collide
# with another concurrent lane or with kind-sso-walk.sh's own fixed cluster/
# ports: the Postgres container is wardyn-cl-673-pg-<pid>, published on an
# ephemeral 127.0.0.1 port; wardynd's HTTP port is picked free (pick_free_port);
# the fake Bedrock host is a non-resolving DNS name, never an IP literal or a
# real listener (only its config-time acceptance is under test, no live dial).
# No kind cluster, no docker image, no agent-image map: WARDYN_RUNNER=none
# never dispatches a sandbox, so none of those exist for this script to
# isolate.
#
# GUARD: needs Docker for the throwaway Postgres. Self-skips unless
# WARDYN_TEST_DOCKER=1, the same knob run-e2e-live.sh uses.
# Usage: WARDYN_TEST_DOCKER=1 scripts/test-kind-sso-provider-seed.sh   (exit 0 = PASS)
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "test-kind-sso-provider-seed: set WARDYN_TEST_DOCKER=1 to run (needs Docker for a throwaway Postgres; skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
WARDYN_LOG_TAG="[provider-seed]"
# shellcheck source=lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
command -v docker >/dev/null 2>&1 || die "docker not found"
command -v python3 >/dev/null 2>&1 || die "python3 not found (pick_free_port)"

fail=0
bad() { printf '\033[1;31m[FAIL]\033[0m %s\n' "$*" >&2; fail=1; }
ok()  { printf '[ok] %s\n' "$*"; }

PG_CONTAINER="wardyn-cl-673-pg-$$"
WARDYND_PID=""
WORKDIR="$(mktemp -d)"

cleanup() {
  [[ -n "${WARDYND_PID}" ]] && kill "${WARDYND_PID}" >/dev/null 2>&1
  docker rm -f "${PG_CONTAINER}" >/dev/null 2>&1 || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

# ── 1. a throwaway, uniquely-named Postgres on an ephemeral loopback port ────
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
step "starting a throwaway Postgres (${PG_CONTAINER})"
docker run -d --name "${PG_CONTAINER}" -p 127.0.0.1::5432 \
  -e POSTGRES_USER=wardyn -e POSTGRES_PASSWORD=wardyn-dev -e POSTGRES_DB=wardyn \
  postgres:17 >/dev/null || die "could not start ${PG_CONTAINER}"
for _ in $(seq 1 30); do
  docker exec "${PG_CONTAINER}" pg_isready -U wardyn >/dev/null 2>&1 && break
  sleep 1
done
PG_PORT="$(docker port "${PG_CONTAINER}" 5432/tcp | head -1 | cut -d: -f2)"
[[ -n "${PG_PORT}" ]] || die "could not read ${PG_CONTAINER}'s published port"

# ── 2. build wardynd (no -tags docker: WARDYN_RUNNER=none, headless API-only —
#      this proves the seeding doors, not sandbox dispatch, so no docker
#      substrate, no agent images, nothing else to isolate) ─────────────────
step "building wardynd"
go build -o "${WORKDIR}/wardynd" ./cmd/wardynd || die "go build wardynd failed"
AGE_KEY="$("${WORKDIR}/wardynd" -gen-age-key | sed -n 's/^AGE-SECRET-KEY-1[A-Z0-9]*$/&/p')"
[[ -n "${AGE_KEY}" ]] || die "could not generate an age key"

HTTP_PORT="$(pick_free_port)"
# wardynd always opens a second, internal-CA TLS listener for the proxy hop
# (WARDYN_INTERNAL_LISTEN, default :8443 — fixed and NOT this script's to
# reuse on a shared box with other lanes' daemons). Picked free for the same
# isolation reason as HTTP_PORT; never dialed here since WARDYN_RUNNER=none
# never starts a sandbox proxy to use it.
INTERNAL_PORT="$(pick_free_port)"
BASE_URL="http://127.0.0.1:${HTTP_PORT}"
ADMIN_TOKEN="walk-673-$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
# A non-resolving DNS name, never an IP literal — ValidateBedrockBaseURL (T-13's
# own pin) refuses a loopback/link-local IP literal outright, and this script
# tests config ACCEPTANCE, not a live dial (WARDYN_RUNNER=none dispatches
# nothing).
FAKE_BEDROCK_URL="http://wardyn-cl-673-fake.internal.test:8090"

step "starting wardynd on ${BASE_URL} (WARDYN_RUNNER=none; NO WARDYN_BEDROCK_* set)"
# THE EXACT ENVIRONMENT wardynd is launched with, as an array rather than an
# inline `env -i ... &` — so step 5 below can assert against the very list
# that launched it, instead of reading /proc/<pid>/environ (permission-
# dependent under some sandboxes/PID-namespace setups, and this script must
# work under all of them). If a legacy WARDYN_BEDROCK_* var is ever added to
# this array, the launch and the assertion see the identical change.
WARDYND_ENV=(
  WARDYN_PG_DSN="postgres://wardyn:wardyn-dev@127.0.0.1:${PG_PORT}/wardyn?sslmode=disable"
  WARDYN_LISTEN="127.0.0.1:${HTTP_PORT}"
  WARDYN_INTERNAL_LISTEN="127.0.0.1:${INTERNAL_PORT}"
  WARDYN_ADMIN_TOKEN="${ADMIN_TOKEN}"
  WARDYN_ALLOW_TEST_ENDPOINTS=true
  WARDYN_AGE_KEY="${AGE_KEY}"
  WARDYN_DEFAULT_POLICY="${ROOT}/examples/policies/default.json"
)
env -i PATH="${PATH}" HOME="${HOME}" "${WARDYND_ENV[@]}" \
  "${WORKDIR}/wardynd" >"${WORKDIR}/wardynd.log" 2>&1 &
WARDYND_PID=$!
wait_healthy "${BASE_URL}" 30 || { tail -40 "${WORKDIR}/wardynd.log" >&2; die "wardynd did not become healthy"; }

auth() { curl -s -H "Authorization: Bearer ${ADMIN_TOKEN}" "$@"; }
put_json() { # <path> <body> -> HTTP code (body on stdout via $RESP_FILE)
  auth -o "${RESP_FILE}" -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d "$2" "${BASE_URL}/api/v1$1"
}
RESP_FILE="${WORKDIR}/resp.json"

# ── 3. PUT /model-providers: seed the Bedrock provider record ───────────────
step "PUT /model-providers (bedrock_bearer, isolated fake host, http:// under the test hatch)"
mp_body=$(cat <<JSON
{"providers":[{"id":"walk-673-bedrock","name":"Walk Bedrock","kind":"bedrock_bearer",
  "bedrock":{"region":"us-east-1","base_url":"${FAKE_BEDROCK_URL}"},
  "harnesses":[{"harness":"claude-code","model":"us.anthropic.claude-sonnet-4-5-20250929-v1:0"}]}]}
JSON
)
uid=""
code=$(put_json /model-providers "${mp_body}")
if [[ "${code}" != "200" ]]; then
  bad "PUT /model-providers = ${code}: $(cat "${RESP_FILE}")"
else
  uid=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["providers"][0].get("uid",""))' "${RESP_FILE}")
  if [[ -n "${uid}" ]]; then
    ok "PUT /model-providers = 200, server minted uid=${uid}"
  else
    bad "PUT /model-providers = 200 but no uid was minted for the stored provider: $(cat "${RESP_FILE}")"
  fi
fi

# ── 4. PUT /agent-providers: the roster default ──────────────────────────────
step "PUT /agent-providers (claude-code's default_provider = walk-673-bedrock)"
code=$(put_json /agent-providers '{"agents":[{"id":"claude-code","mechanism":"bedrock_bearer","default_provider":"walk-673-bedrock"}]}')
if [[ "${code}" != "200" ]]; then
  bad "PUT /agent-providers = ${code}: $(cat "${RESP_FILE}")"
else
  ok "PUT /agent-providers = 200"
fi
got_default=$(auth "${BASE_URL}/api/v1/agent-providers" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["agents"][0].get("default_provider",""))' 2>/dev/null)
if [[ "${got_default}" == "walk-673-bedrock" ]]; then
  ok "GET /agent-providers reflects the roster default"
else
  bad "GET /agent-providers default_provider = ${got_default@Q}, want walk-673-bedrock"
fi

# ── 5. no legacy Bedrock env reached the daemon's own environment ───────────
step "asserting no WARDYN_BEDROCK_* var reached wardynd's environment"
# Against the exact array that launched it (WARDYND_ENV above) — dynamic (it
# fails the moment a legacy var is added to that launch, not a source grep of
# this file) and portable: /proc/<pid>/environ is permission-gated under some
# sandboxes even for the parent that spawned the child, which would make a
# read failure there indistinguishable from "no such var" and pass vacuously.
legacy_env=""
for kv in "${WARDYND_ENV[@]}"; do
  [[ "${kv}" == WARDYN_BEDROCK_* ]] && legacy_env+="${kv} "
done
# Belt-and-braces, best-effort: when /proc/<pid>/environ IS readable here,
# cross-check the live process too — but only WARN on a read failure, never
# pass or fail on it (env -i's own guarantee above is what this test relies
# on).
if proc_env="$(tr '\0' '\n' < "/proc/${WARDYND_PID}/environ" 2>/dev/null)"; then
  proc_legacy="$(grep '^WARDYN_BEDROCK_' <<<"${proc_env}" || true)"
  [[ -n "${proc_legacy}" ]] && legacy_env+="${proc_legacy} "
else
  warn "could not read /proc/${WARDYND_PID}/environ (sandbox-dependent) — relying on the launch array alone"
fi
if [[ -z "${legacy_env}" ]]; then
  ok "wardynd was launched (env -i) with no WARDYN_BEDROCK_* — the provider record is the only Bedrock config"
else
  bad "wardynd's environment carries legacy Bedrock config this script never set: ${legacy_env}"
fi

# ── 6. UID-backed capture: a person's own credential lands under the UID ────
step "PUT /model-providers/walk-673-bedrock/credential (this caller's own bearer key)"
code=$(auth -o "${RESP_FILE}" -w '%{http_code}' -X PUT -H 'Content-Type: application/json' \
  -d '{"value":"fake-bedrock-bearer-token-not-a-real-secret"}' \
  "${BASE_URL}/api/v1/model-providers/walk-673-bedrock/credential")
if [[ "${code}" != "204" ]]; then
  bad "PUT /model-providers/{id}/credential = ${code}: $(cat "${RESP_FILE}")"
else
  ok "PUT /model-providers/{id}/credential = 204"
fi
connected=$(auth "${BASE_URL}/api/v1/model-providers" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("connected_people",{}).get("walk-673-bedrock",-1))' 2>/dev/null)
if [[ "${connected}" == "1" ]]; then
  ok "connected_people[walk-673-bedrock] = 1 — the capture landed under this provider's UID"
else
  bad "connected_people[walk-673-bedrock] = ${connected@Q}, want 1 (the UID-keyed capture did not register)"
fi

# ── 7. THE DISTINGUISHING CHECK: delete + re-add under the SAME admin id ────
# id-keying and UID-keying agree on everything proven so far (both key by the
# one string this script has only ever used once). The only way to tell them
# apart is to change the UID while holding the id fixed: delete the provider
# (purging its stored credential, rule 8) and re-add it under the identical
# id. A UID-keyed store starts the new record with nobody's credential; an
# id-keyed store would still find the old capture, because "walk-673-bedrock"
# never changed.
step "clearing the roster default so walk-673-bedrock can be deleted"
code=$(put_json /agent-providers '{"agents":[{"id":"claude-code","mechanism":"bedrock_bearer","default_provider":""}]}')
[[ "${code}" == "200" ]] || bad "PUT /agent-providers (clear default) = ${code}: $(cat "${RESP_FILE}")"

step "PUT /model-providers {} — deleting walk-673-bedrock (rule 8 purges its credential)"
code=$(put_json /model-providers '{}')
[[ "${code}" == "200" ]] || bad "PUT /model-providers (delete) = ${code}: $(cat "${RESP_FILE}")"

step "PUT /model-providers — re-adding walk-673-bedrock under the SAME admin id"
new_uid=""
code=$(put_json /model-providers "${mp_body}")
if [[ "${code}" != "200" ]]; then
  bad "PUT /model-providers (re-add) = ${code}: $(cat "${RESP_FILE}")"
else
  new_uid=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["providers"][0].get("uid",""))' "${RESP_FILE}")
fi
if [[ -n "${new_uid}" && "${new_uid}" != "${uid}" ]]; then
  ok "re-add minted a NEW uid (${new_uid}, was ${uid}) for the unchanged id walk-673-bedrock"
else
  bad "re-add did not mint a fresh uid: got ${new_uid@Q}, previous was ${uid@Q}"
fi

step "restoring the roster default to walk-673-bedrock"
code=$(put_json /agent-providers '{"agents":[{"id":"claude-code","mechanism":"bedrock_bearer","default_provider":"walk-673-bedrock"}]}')
[[ "${code}" == "200" ]] || bad "PUT /agent-providers (restore default) = ${code}: $(cat "${RESP_FILE}")"

step "asserting the OLD capture did not survive under the re-added id"
connected=$(auth "${BASE_URL}/api/v1/model-providers" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("connected_people",{}).get("walk-673-bedrock",-1))' 2>/dev/null)
if [[ "${connected}" == "0" ]]; then
  ok "connected_people[walk-673-bedrock] = 0 after delete+re-add — credentials are keyed by UID, not id"
else
  bad "connected_people[walk-673-bedrock] = ${connected@Q}, want 0 — a credential captured under the OLD uid is still reachable under the re-added id (credentials are keyed by id, not UID)"
fi

# ── 8. failure propagation: two refusals a rebuilt walk must be able to trust ─
step "asserting a roster default naming NO real provider is refused"
code=$(put_json /agent-providers '{"agents":[{"id":"claude-code","mechanism":"bedrock_bearer","default_provider":"no-such-provider"}]}')
if [[ "${code}" == "400" ]]; then
  ok "an unknown default_provider is refused (400): $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["error"])' "${RESP_FILE}" 2>/dev/null || cat "${RESP_FILE}")"
else
  bad "an unknown default_provider was NOT refused: PUT /agent-providers = ${code}: $(cat "${RESP_FILE}")"
fi
# The roster default from step 4 must still be intact — a refused write must
# never have landed.
still_default=$(auth "${BASE_URL}/api/v1/agent-providers" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["agents"][0].get("default_provider",""))' 2>/dev/null)
[[ "${still_default}" == "walk-673-bedrock" ]] \
  && ok "the refused write did not clobber the stored roster default" \
  || bad "the roster default changed after a REFUSED write: now ${still_default@Q}"

step "asserting clearing a provider the roster still defaults to is refused"
code=$(put_json /model-providers '{}')
if [[ "${code}" == "400" ]]; then
  ok "clearing model-providers while a roster row still defaults to it is refused (400): $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["error"])' "${RESP_FILE}" 2>/dev/null || cat "${RESP_FILE}")"
else
  bad "clearing model-providers while still the roster default was NOT refused: PUT /model-providers = ${code}: $(cat "${RESP_FILE}")"
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "" >&2
  echo "FAIL: scripts/test-kind-sso-provider-seed.sh found a broken door above." >&2
  exit 1
fi
echo ""
echo "PASS: model-providers + agent-providers seeding, UID-backed capture and failure propagation all hold."
