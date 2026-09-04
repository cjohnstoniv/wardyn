#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-claims-match-code.sh — daemon-free, network-free assertions that what
# Wardyn SAYS matches what it DOES. Three claims that had drifted apart, each
# with a different owner and no gate between them:
#
#   C1  scripts/up.sh's wardynd probe reports its own failures instead of
#       swallowing them (perf-2:perf2-C3). Behavioural: the probe is extracted
#       from the live script and driven against a `docker` stub that fails the
#       way a missing/unfetchable probe image fails.
#   C2  no document presents WARDYN_SUBSCRIPTION_TOKEN as a working headless
#       seed for `make setup` / `scripts/up.sh` (ops-1:C06). Neither compose
#       path honours it: up.sh warns and ignores, ci-run.sh exits non-zero.
#   C3  docs/VERIFY.md stays parameterised on $WARDYN_VERSION (research-1:c2),
#       which is why RELEASING.md step 1b does NOT list it as a file to bump.
#   C4  up.sh says "cosign-signed, SBOM-attested" only on a path where cosign
#       actually verified the images (F005/F100/F151). Behavioural: cosign_verify
#       is extracted from the live script and driven against a `cosign` stub.
#   C5  the pull-first block's "Skipped the local build" is true — no service
#       whose :local image it retags is rebuilt unconditionally afterwards
#       (F099). Derived from the two lists in the script, never hand-copied.
#   C6  RELEASING.md's required-status-checks `contexts` list covers the two
#       gates it BOLDS as gates — `notices` and every cell of ci.yml's `trivy`
#       matrix (F121). The trivy cells are derived from ci.yml, so adding an
#       image to the scan matrix and forgetting branch protection goes red here.
#   C7  three docs describe what the code does, each derived from the code:
#       ENV.md's WARDYN_IMPORT_AWS row names the SECRETS setup.sh writes (F081);
#       VERIFY.md/THREAT-MODEL say which services `docker compose pull` covers
#       rather than "the images" (F091); OPERATIONS.md's second-user recipe
#       names the loopback services that flank it, password included (F155).
#
# Run (no docker, no network, no writes outside mktemp):
#   bash scripts/test-claims-match-code.sh
#
# Wired into `make test-scripts`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UP_SH="${ROOT}/scripts/up.sh"

fail() { echo "test-claims-match-code: FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

WORK="$(mktemp -d)"; trap 'rm -rf "${WORK}"' EXIT

# shellcheck source=lib/common.sh
. "${ROOT}/scripts/lib/common.sh"   # env_get, which wardynd_probe calls

# ── C1: the probe container's own failure is reported, not discarded ────────
#
# `-m 5` bounds the CURL. It does not bound the image acquisition `docker run`
# performs first on a box that has never run this probe, and docker reports
# that acquisition — "Unable to find image … locally", a failed pull, "No such
# image" — ONLY on stderr. With that stderr sent to /dev/null, an unbounded
# registry fetch and an outright failure both arrived at the call sites as a
# bare HTTP `000`, which they go on to report as a wardynd problem ("check
# 'docker compose … logs wardynd'"). The operator is then reading daemon logs
# for a container-image problem.
#
# The stub below is that failure: exit 125 with docker's own message on stderr
# and nothing on stdout. The assertion is that the message SURVIVES.
mkdir -p "${WORK}/bin"
cat > "${WORK}/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker: Error response from daemon: No such image: curlimages/curl:latest" >&2
exit 125
STUB
chmod +x "${WORK}/bin/docker"

probe_body="$(sed -n '/^wardynd_probe() {/,/^}/p' "${UP_SH}")"
[ -n "${probe_body}" ] || fail "scripts/up.sh has no wardynd_probe() to extract"
eval "${probe_body}"

: > "${WORK}/.env"
PATH="${WORK}/bin:${PATH}" wardynd_probe "${WORK}/.env" /api/v1/setup/status \
  >"${WORK}/probe.out" 2>"${WORK}/probe.err" || true

# stdout is still the documented fallback the call sites parse with `tail -n1`.
[ "$(tail -n1 "${WORK}/probe.out")" = "000" ] \
  || fail "wardynd_probe stdout was '$(tr -d '\n' <"${WORK}/probe.out")', want '000' — llm_ready_from_probe and the gate smoke both read the last stdout line"

# …and the CAUSE is no longer thrown away.
grep -q 'No such image' "${WORK}/probe.err" \
  || fail "wardynd_probe discarded the probe container's stderr — docker reports a missing/unfetchable curlimages/curl image ONLY there, so an unbounded pull and a failed one both reach cmd_up as a bare HTTP 000 it blames wardynd for (perf2-C3)"

# Pin the mechanism too: a re-added 2>/dev/null on the probe's docker run puts
# the whole defect straight back, and the behavioural case above would still
# pass against a stub that wrote nothing.
if printf '%s' "${probe_body}" | grep -q '2>/dev/null'; then
  fail "wardynd_probe redirects the probe container's stderr to /dev/null again (perf2-C3)"
fi
pass "C1 wardynd_probe reports the probe container's own failure and still yields 000"

# ── C2: no doc sells WARDYN_SUBSCRIPTION_TOKEN as a make-setup seed ─────────
#
# It was documented as a working headless seed in three places while every
# named consumer refused it: scripts/up.sh warns and ignores it (a shared
# subscription credential is a single-user desktop setting — see
# WARDYN_ALLOW_SHARED_SUBSCRIPTION and cmd/wardynd/boot_posture.go), and
# scripts/ci-run.sh exits non-zero on it. The rule is not "never mention the
# variable" — docs/ENV.md's row exists precisely to say it does NOT work here —
# it is that the refusal must travel WITH the mention.
#
# Scoped to a 4-line window around each mention rather than the line or the
# file. A line-scoped rule is disarmed by a reflow that pushes `make setup` onto
# the next line; a file-scoped one is vacuous, because a doc of any length
# contains the word "not" somewhere. Four lines is one wrapped markdown bullet.
den='ignore|ignored|ignores|not supported|[Nn]either|refus|exits non-zero|[*][*][Nn]ot[*][*]'
offenders="$(find "${ROOT}/docs" -name '*.md' -print0 \
  | xargs -0 awk -v den="${den}" '
      /WARDYN_SUBSCRIPTION_TOKEN/ {
        w = $0; ln = FNR   # getline below moves FNR past the window
        for (i = 0; i < 3; i++) { if ((getline nx) > 0) w = w "\n" nx; else break }
        if (w !~ den) print FILENAME ":" ln
      }' || true)"
[ -z "${offenders}" ] || {
  printf '%s\n' "${offenders}" >&2
  fail "a doc names WARDYN_SUBSCRIPTION_TOKEN without the refusal anywhere near it — up.sh warns and drops it and ci-run.sh exits non-zero, so a reader takes it for a working headless seed (ops-1:C06)"
}
# The counterweight: the refusal must be documented SOMEWHERE, or the check above
# passes vacuously the moment every mention is deleted instead of corrected.
grep -rq 'WARDYN_SUBSCRIPTION_TOKEN' "${ROOT}/docs" --include='*.md' \
  || fail "no doc mentions WARDYN_SUBSCRIPTION_TOKEN at all — the variable is still read by scripts/record-demo.sh and refused by up.sh/ci-run.sh, and an operator who exports it deserves to find out why nothing happened"
# …and up.sh must still be the thing that refuses it, or C2 is guarding a
# claim about behaviour that no longer exists.
grep -q 'WARDYN_SUBSCRIPTION_TOKEN is ignored' "${UP_SH}" \
  || fail "scripts/up.sh no longer warns that WARDYN_SUBSCRIPTION_TOKEN is ignored — if it now honours the variable, the docs above are the thing to change (ops-1:C06)"
pass "C2 no doc sells WARDYN_SUBSCRIPTION_TOKEN as a make-setup seed"

# ── C3: docs/VERIFY.md stays parameterised on $WARDYN_VERSION ───────────────
#
# Every command in it resolves the version from step 0's $WARDYN_VERSION, which
# is why RELEASING.md step 1b lists four files to bump and NOT this one. A
# literal version pasted into one of these lines is invisible at review time and
# verifies the wrong release for everyone who follows the doc afterwards.
VERIFY="${ROOT}/docs/VERIFY.md"
hardcoded="$(grep -nE 'ghcr\.io|helm (install|pull)|gh release' "${VERIFY}" \
             | grep -vE 'WARDYN_VERSION=' \
             | grep -E '[0-9]+\.[0-9]+\.[0-9]+' || true)"
[ -z "${hardcoded}" ] || {
  printf '%s\n' "${hardcoded}" >&2
  fail "docs/VERIFY.md hard-codes a release version in a verification command — every one of them is parameterised on \$WARDYN_VERSION and must stay that way (RELEASING.md step 1b; research-1:c2)"
}
grep -q 'WARDYN_VERSION' "${VERIFY}" \
  || fail "docs/VERIFY.md no longer references \$WARDYN_VERSION at all — the parameterisation this guard protects is gone"
pass "C3 docs/VERIFY.md verification commands stay parameterised on \$WARDYN_VERSION"

# ── C4: the "cosign-signed, SBOM-attested" claim is made only when true ────
#
# The pull-first path retagged five images fetched by TAG and then told the
# operator they were "cosign-signed, SBOM-attested — see docs/VERIFY.md", while
# that same page says "the installer verifies none of them". A binary is then
# extracted from one of those images and executed ON THE HOST (seed_host_proxy),
# so the claim is not decorative. Two halves, both asserted here: the check
# EXISTS and does what VERIFY.md s1/s2 tell an operator to do, and the claim is
# printed only where that check returned 0.
cv_body="$(sed -n '/^cosign_verify() {/,/^}/p' "${UP_SH}")"
[ -n "${cv_body}" ] || fail "scripts/up.sh has no cosign_verify() — the pull path announces 'cosign-signed, SBOM-attested' and nothing in the script verifies anything (F005/F100)"
printf '%s' "${cv_body}" | grep -q 'certificate-identity-regexp' \
  || fail "cosign_verify() does not pin the certificate IDENTITY — a keyless signature verified against no identity says only 'somebody signed this' (docs/VERIFY.md s1)"
printf '%s' "${cv_body}" | grep -q 'verify-attestation' \
  || fail "cosign_verify() checks the signature but not the SBOM attestation — 'SBOM-attested' is half the claim being made (docs/VERIFY.md s2)"

# Drive it. cosign absent -> 2 (an ABSENT check, not a failed one); cosign
# present and refusing -> 1; cosign present and happy -> 0.
mkdir -p "${WORK}/cosign-bin"
eval "${cv_body}"
( PATH="${WORK}/empty"; export PATH; cosign_verify ghcr.io/x/y:1 ) && cv_rc=0 || cv_rc=$?
[ "${cv_rc}" = 2 ] || fail "cosign_verify returned ${cv_rc} with no cosign on PATH, want 2 — a missing tool must be distinguishable from a passing check, or 'verified' is announced on every box that has no cosign"

cat > "${WORK}/cosign-bin/cosign" <<'STUB'
#!/usr/bin/env bash
[ -f "$(dirname "$0")/refuse" ] && exit 1
exit 0
STUB
chmod +x "${WORK}/cosign-bin/cosign"
( PATH="${WORK}/cosign-bin:${PATH}"; export PATH; cosign_verify ghcr.io/x/y:1 ) && cv_rc=0 || cv_rc=$?
[ "${cv_rc}" = 0 ] || fail "cosign_verify returned ${cv_rc} when cosign verified both the signature and the attestation, want 0"
: > "${WORK}/cosign-bin/refuse"
( PATH="${WORK}/cosign-bin:${PATH}"; export PATH; cosign_verify ghcr.io/x/y:1 ) && cv_rc=0 || cv_rc=$?
[ "${cv_rc}" = 1 ] || fail "cosign_verify returned ${cv_rc} when cosign REFUSED the image, want 1 — a refusal that reads as success is worse than no check"

# ...and the claim itself is inside the branch that check gates.
claim_line="$(grep -n 'cosign-signed, SBOM-attested' "${UP_SH}" | head -1 | cut -d: -f1 || true)"
[ -n "${claim_line}" ] || fail "the 'cosign-signed, SBOM-attested' claim is gone from scripts/up.sh — if it was reworded rather than made true, this guard is now pointing at nothing"
guard_line="$(grep -n '^    if \$verified_all; then' "${UP_SH}" | head -1 | cut -d: -f1 || true)"
[ -n "${guard_line}" ] && [ "${guard_line}" -lt "${claim_line}" ] \
  || fail "scripts/up.sh prints 'cosign-signed, SBOM-attested' outside the \$verified_all branch — the claim is made again on boxes where cosign never ran (F005/F100)"
pass "C4 up.sh verifies with cosign (identity + CycloneDX attestation) before it claims the images are signed"

# ── C5: "Skipped the local build" means the pulled images are the ones used ──
#
# The pull-first block fetched five published images and retagged them to the
# :local names compose uses, then unconditionally rebuilt four of them a few
# hundred lines later — so only wardynd's build was actually skipped, and the
# proxy and agent images an operator was told came from a signed release were
# local compiles. Both lists are DERIVED from the script here; a hand-copied
# second copy is what drifted in the first place.
pulled_local="$(sed -n '/^      for pair in /,/; do$/p' "${UP_SH}" \
                | grep -oE '"[a-z0-9-]+:wardyn/[a-z0-9-]+:local"' \
                | sed -E 's/.*:(wardyn\/[a-z0-9-]+:local)"/\1/' | sort -u)"
[ -n "${pulled_local}" ] || fail "could not derive the pull-first image list from scripts/up.sh — the 'for pair in' loop changed shape and this guard would check nothing"

# What the run-components section builds WHEN the pull succeeded, derived from
# the `if $pulled_all` arm itself.
pulled_branch="$(awk '/^    if \$pulled_all; then$/{g=1;next} g&&/^    else$/{g=0} g' "${UP_SH}")"
[ -n "${pulled_branch}" ] || fail "scripts/up.sh's run-components section is not gated on \$pulled_all at all — every image the pull-first block fetched is rebuilt over on the next \`make setup\` (F099)"
built_when_pulled="$( { printf '%s\n' "${pulled_branch}" | grep -oE 'compose --profile build-only build [a-z0-9-]+' | awk '{print $NF}'
                        printf '%s\n' "${pulled_branch}" | sed -nE 's/^ *_svcs="([a-z0-9 -]+)".*/\1/p' | tr ' ' '\n'; } | sort -u)"

COMPOSE_YAML="${ROOT}/deploy/compose/docker-compose.yaml"
svc_image() { awk -v s="  $1:" '$0==s{f=1;next} f&&/^  [a-z]/{f=0} f&&/^    image:/{print $2;exit}' "${COMPOSE_YAML}"; }
for svc in ${built_when_pulled}; do
  [ -n "${svc}" ] || continue
  img="$(svc_image "${svc}")"
  case "${img}" in \$\{*:-*\}) img="${img#*:-}"; img="${img%\}}" ;; esac
  printf '%s\n' "${pulled_local}" | grep -qxF "${img}" \
    && fail "scripts/up.sh pulls ${img} and then rebuilds service '${svc}' anyway — the published image is discarded and 'Skipped the local build' is false for it (F099)"
done

# The counterweight: the NOT-pulled arm must still build every one of them, or
# this guard would be satisfied by simply building nothing.
notpulled_branch="$(awk '/^    if \$pulled_all; then$/{g=1;next} g==1&&/^    else$/{g=2;next} g==2&&/^    fi$/{g=0} g==2' "${UP_SH}")"
built_when_built="$( { printf '%s\n' "${notpulled_branch}" | grep -oE 'compose --profile build-only build [a-z0-9-]+' | awk '{print $NF}'
                       printf '%s\n' "${notpulled_branch}" | sed -nE 's/^ *_svcs="([a-z0-9 -]+)".*/\1/p' | tr ' ' '\n'; } | sort -u)"
for svc in proxy-image agent-base agent-claude-code agent-codex-cli agent-aws-sso; do
  printf '%s\n' "${built_when_built}" | grep -qxF "${svc}" \
    || fail "with no published images to pull, scripts/up.sh no longer builds '${svc}' — the air-gapped / pre-push / WARDYN_BUILD_LOCAL path needs all five (F099)"
done
# ...and agent-claude-code is built on BOTH arms: Wardyn publishes no image
# carrying the vendor CLI, so there is nothing to pull for it (docs/VERIFY.md).
printf '%s\n' "${built_when_pulled}" | grep -qxF agent-claude-code \
  || fail "the pulled arm skips agent-claude-code, which is never published — the one agent that MUST be built locally would be missing after \`make setup\` (F099)"
pass "C5 no service whose image the pull-first block retags is rebuilt unconditionally"

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

# ── C7: three doc claims, each checked against the thing it describes ──────
COMPOSE_YAML="${ROOT}/deploy/compose/docker-compose.yaml"

# F081 — ENV.md called WARDYN_IMPORT_AWS an import of "~/.aws selectors", using
# a word ENV.md itself defines as explicitly NOT credentials, for a flag whose
# headless mode writes long-lived static AWS keys into the secret store. The
# secret NAMES are derived from setup.sh, so a new one must be documented too.
env_row="$(grep -n 'WARDYN_IMPORT_AWS' "${ROOT}/docs/ENV.md" | head -1 || true)"
[ -n "${env_row}" ] || fail "docs/ENV.md no longer documents WARDYN_IMPORT_AWS — it is the headless YES for writing static AWS keys into the secret store (F081)"
aws_secrets="$(grep -oE 'wardyn secret set aws-[a-z-]+' "${ROOT}/scripts/setup.sh" | awk '{print $4}' | sort -u || true)"
[ -n "${aws_secrets}" ] || fail "scripts/setup.sh no longer writes any aws-* secret — this guard would check nothing"
for sec in ${aws_secrets}; do
  printf '%s' "${env_row}" | grep -qF "${sec}" \
    || fail "docs/ENV.md's WARDYN_IMPORT_AWS row does not name '${sec}', which scripts/setup.sh writes into the secret store when it is set — the row described it as importing '~/.aws selectors', a word ENV.md defines as explicitly NOT credentials (F081)"
done
printf '%s' "${env_row}" | grep -qi 'credential' \
  || fail "docs/ENV.md's WARDYN_IMPORT_AWS row never uses the word 'credential' — the whole defect was that it read as a non-secret selector import (F081)"

# F091 — "the images are pulled by `docker compose pull`" was true of three of
# them: everything else sits behind the build-only profile. The service list is
# derived from the compose file, so a profile change lands here.
buildonly="$(awk '/^  [a-z0-9-]+:/{svc=$1; sub(":","",svc)} /profiles: \["build-only"\]/{print svc}' "${COMPOSE_YAML}" | sort -u || true)"
[ -n "${buildonly}" ] || fail "no build-only services found in ${COMPOSE_YAML} — this guard would check nothing"
for doc in "${ROOT}/docs/VERIFY.md" "${ROOT}/threatmodel/THREAT-MODEL.md"; do
  grep -q 'default-profile\|DEFAULT-PROFILE' "${doc}" \
    || fail "${doc#"${ROOT}/"} says the images are pulled by \`docker compose pull\` without saying that only the DEFAULT-PROFILE services are — $(printf '%s ' ${buildonly})sit behind the build-only profile and arrive at the first run instead (F091)"
done

# F155 — the second-user recipe is a security boundary recipe, and the two
# loopback services that flank it carry a published password and no auth at
# all. Both facts are read out of the compose file, password literal included.
pg_pw="$(awk '/^  postgres:/{f=1} f&&/^  [a-z]/&&!/^  postgres:/{f=0} f&&/POSTGRES_PASSWORD:/{print $2;exit}' "${COMPOSE_YAML}")"
[ -n "${pg_pw}" ] || fail "could not read POSTGRES_PASSWORD out of ${COMPOSE_YAML}"
second_user="$(awk '/^## Second user, same host/{f=1;next} f&&/^## /{f=0} f' "${ROOT}/docs/OPERATIONS.md")"
[ -n "${second_user}" ] || fail "docs/OPERATIONS.md has no 'Second user, same host' section — C7's F155 half is pointing at nothing"
printf '%s' "${second_user}" | grep -qF "${pg_pw}" \
  || fail "docs/OPERATIONS.md's 'Second user, same host' recipe never mentions that Postgres publishes on loopback with the PUBLISHED literal password '${pg_pw}' — the second person psql's straight past every admin/member control the section is about (F155)"
printf '%s' "${second_user}" | grep -qi 'registry' \
  || fail "docs/OPERATIONS.md's 'Second user, same host' recipe never mentions the unauthenticated loopback registry — any local user can push a layer a later envbuild run executes (F155)"
pass "C7 ENV.md/VERIFY.md/THREAT-MODEL/OPERATIONS.md match the code they describe"

echo "test-claims-match-code: self-test PASS"
