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
#   C8  the R6 docs-ops wave: the two pasteable CI pipelines pin the wardyn
#       checkout they EXECUTE (F007); the release-asset prose covers what
#       release.yml actually requires (F016); the migrator/app-role rationale
#       names the real ALTER TABLEs (F034); README's inline hero policy is the
#       same spec as examples/policies/sandbox.yaml (F038); the four-command
#       SSH recipe parses as four commands (F043); no fenced RunPolicySpec
#       carries a run-create field (F045); DESKTOP.md's Podman socket is
#       Podman's (F054). Each derived from the file it describes.
#
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

# ── C8: the R6 docs-ops wave — every claim derived from what it describes ──
GH_CI="${ROOT}/docs/ci/github-actions.yml"
AZ_CI="${ROOT}/docs/ci/azure-pipelines.yml"
RELEASE_YML="${ROOT}/.github/workflows/release.yml"

# F007 — both pasteable pipelines fetch cjohnstoniv/wardyn and then EXECUTE that
# checkout's scripts/ci-run.sh inside a job the same files tell you to seed with
# the consumer's ANTHROPIC_API_KEY. Unpinned, that is every push to Wardyn's
# default branch running shell in front of a stranger's secrets. The expected
# pin is DERIVED from the shipped version, so a release bump that forgets these
# two files fails here rather than shipping a pipeline pinned to last release.
shipped_ver="$(sed -n 's/.*Version = "\([^"]*\)".*/\1/p' "${ROOT}/internal/version/version.go" | head -1)"
[ -n "${shipped_ver}" ] || fail "could not read the shipped version out of internal/version/version.go — this guard would compare against nothing"
gh_ref="$(grep -oE '^[[:space:]]+ref:[[:space:]]*\S+' "${GH_CI}" | awk '{print $2}' | head -1 || true)"
[ -n "${gh_ref}" ] || fail "docs/ci/github-actions.yml's wardyn checkout has no \`ref:\` — it resolves to tip of the default branch, and the next step EXECUTES that checkout's scripts/ci-run.sh in a job holding the consumer's ANTHROPIC_API_KEY (F007)"
[ "${gh_ref}" = "v${shipped_ver}" ] \
  || fail "docs/ci/github-actions.yml pins the wardyn checkout at '${gh_ref}', but this tree ships v${shipped_ver} — bump it with the other version strings (RELEASING.md step 1b) so a pasted pipeline runs the release it documents (F007)"
az_branch="$(grep -oE 'git clone[^|]*--branch[[:space:]]+\S+' "${AZ_CI}" | awk '{for(i=1;i<=NF;i++) if ($i=="--branch") print $(i+1)}' | head -1 || true)"
[ -n "${az_branch}" ] || fail "docs/ci/azure-pipelines.yml clones cjohnstoniv/wardyn with no --branch — it clones the default branch, and the next step EXECUTES that clone's scripts/ci-run.sh in a job holding the consumer's secrets (F007)"
[ "${az_branch}" = "v${shipped_ver}" ] \
  || fail "docs/ci/azure-pipelines.yml pins the wardyn clone at '${az_branch}', but this tree ships v${shipped_ver} (F007)"
grep -q 'Pin the wardyn checkout' "${ROOT}/docs/CI.md" \
  || fail "docs/CI.md never explains why the wardyn checkout is pinned — the pin without the reason is the thing a consumer deletes as noise (F007)"
pass "C8a both CI examples pin the wardyn checkout they execute, at the shipped version"

# F016 — release.yml hard-fails unless a fixed asset list landed on the Release.
# Both release-facing docs enumerated a SHORTER set, so a reader checking a
# release against the prose would not notice install.sh or the CLI binaries
# missing. The required set is read out of the workflow's own assertion.
want_assets="$(awk '/Assert every required asset actually landed/{f=1} f&&/for want in/{g=1} g{print} g&&/; do/{exit}' "${RELEASE_YML}" \
                | tr ' \\\n' '\n\n\n' | grep -vE '^(for|want|in|do|;|)$' | sort -u || true)"
[ -n "${want_assets}" ] || fail "could not derive release.yml's required-asset list — this guard would check nothing (F016)"
for doc in "${ROOT}/docs/VERIFY.md" "${ROOT}/RELEASING.md"; do
  for a in ${want_assets}; do
    case "${a}" in
      # The four CLI binaries and the three SHA256SUMS parts are enumerated in
      # prose by family, not one by one — check the family name instead.
      wardyn-*-*)        needle='wardyn-<os>-<arch>' ;;
      SHA256SUMS.sig|SHA256SUMS.pem) continue ;;
      *)                 needle="${a}" ;;
    esac
    grep -qF "${needle}" "${doc}" \
      || fail "${doc#"${ROOT}/"} never names '${needle}', which release.yml's release-assets job hard-fails the release without — the doc enumerates a smaller asset set than the pipeline requires (F016)"
  done
done
pass "C8b VERIFY.md/RELEASING.md enumerate every asset release.yml requires"

# F034 — the enumeration justifying the one-way migrator/app-role split named
# 0053 (whose table the same upgrade CREATES two migrations earlier) and omitted
# 0052, 0058 and 0060. Derived: on the 0.6 -> 0.7 path, an ALTER TABLE is a
# hazard exactly when its table was created by a migration from an EARLIER
# release, and a CREATE OR REPLACE FUNCTION always is.
MIGDIR="${ROOT}/internal/db/migrations"
# Tables created at or before the last migration 0.6.x shipped (0049).
old_tables="$(for f in "${MIGDIR}"/00[0-4]*.sql; do
                [ -f "${f}" ] || continue
                grep -ioE 'CREATE TABLE (IF NOT EXISTS )?[a-z_]+' "${f}" || true
              done | awk '{print tolower($NF)}' | sort -u)"
[ -n "${old_tables}" ] || fail "no pre-0.7 CREATE TABLE found under ${MIGDIR#"${ROOT}/"} — this guard would check nothing (F034)"
ops_para="$(awk '/Which role becomes which is the whole procedure/{f=1} f{print} f&&/^Run this as the role you have today/{exit}' "${ROOT}/docs/OPERATIONS.md")"
[ -n "${ops_para}" ] || fail "docs/OPERATIONS.md no longer carries the migrator/app-role rationale — re-anchor this guard (F034)"
for f in "${MIGDIR}"/00[5-9]*.sql "${MIGDIR}"/0[1-9]*.sql; do
  [ -f "${f}" ] || continue
  num="$(basename "${f}" | cut -c1-4)"
  hazard=no
  while read -r tbl; do
    [ -n "${tbl}" ] || continue
    printf '%s\n' "${old_tables}" | grep -qx "${tbl}" && hazard=yes
  done <<< "$(grep -ioE '^[[:space:]]*ALTER TABLE [a-z_]+' "${f}" | awk '{print tolower($NF)}' | sort -u || true)"
  grep -qi 'CREATE OR REPLACE FUNCTION' "${f}" && hazard=yes
  [ "${hazard}" = yes ] || continue
  printf '%s' "${ops_para}" | grep -qF "\`${num}\`" \
    || fail "migration ${num} ALTERs a table an earlier release created (or replaces a function it created), so it needs the migrator to OWN that object — and docs/OPERATIONS.md's migrator/app-role rationale never names it. That enumeration is the whole justification for the one-way role split (F034)"
done
pass "C8c the migrator rationale names every ownership-requiring migration on the upgrade path"

# F038 — README's hero handed the reader `--policy-file examples/policies/…`
# immediately after the no-clone install, which ships no examples/ tree, so the
# pasted command exited 1 on a missing file. It now writes the policy inline;
# this asserts the inline copy is the SAME spec as the committed example rather
# than a second policy that can drift from it.
hero="$(awk '/^cat > sandbox.yaml <</{f=1;next} f&&/^YAML$/{exit} f' "${ROOT}/README.md" \
        | sed 's/#.*//' | sed -E 's/[[:space:]]+$//' | grep -v '^$' | sort)"
[ -n "${hero}" ] || fail "README.md's hero no longer writes a policy inline — if it went back to --policy-file examples/…, the one-line install (which ships no examples/ tree) cannot paste it (F038)"
example="$(sed 's/#.*//' "${ROOT}/examples/policies/sandbox.yaml" | sed -E 's/[[:space:]]+$//' | grep -v '^$' | sort)"
[ "${hero}" = "${example}" ] \
  || fail "README.md's inline hero policy and examples/policies/sandbox.yaml have drifted apart — the page says they are the same four keys (F038):
--- README inline ---
${hero}
--- examples/policies/sandbox.yaml ---
${example}"
grep -qF 'cd ~/.wardyn && docker compose down' "${ROOT}/README.md" \
  || fail "README.md's stop advice is clone-only (\`make compose-down\`) — the install it follows writes no Makefile, and install.sh's own closing banner says \`cd \${HOME_DIR} && docker compose down\` (F038)"
grep -qF 'demo-admin-token' "${ROOT}/docs/TRY-IT.md" \
  || fail "docs/TRY-IT.md no longer warns that demo-admin-token is the compose stack's literal, not the installer's — README routes one-line-install users straight here (F038)"
pass "C8d the README hero pastes on the no-clone path and matches the shipped example"

# F043 — the 0.7 external-tool lane's only start-to-finish recipe could not be
# pasted: a stray escaped space made line 2 pass a literal " " positional, and a
# trailing comment with no continuation split the command in two. Behavioural:
# the block is extracted and PARSED, with a `wardyn` stub counting commands.
ssh_block="$(awk '/^wardyn ssh-key ensure/{f=1} f{print} f&&/^wardyn ssh </{exit}' "${ROOT}/docs/SSH.md")"
[ -n "${ssh_block}" ] || fail "docs/SSH.md no longer carries the four-command external-tool recipe — re-anchor this guard (F043)"
printf '%s\n' "${ssh_block}" | sed 's/<[a-z-]*>/PLACEHOLDER/g' > "${WORK}/ssh-recipe.sh"
bash -n "${WORK}/ssh-recipe.sh" \
  || fail "docs/SSH.md's external-tool recipe does not parse as shell — it is the lane's only start-to-finish recipe and it is meant to be pasted (F043)"
# The stub prints one line per wardyn invocation, with each argv element in
# angle brackets, so an EMPTY positional (what `\ \` produces) is visible.
bash -c "wardyn() { printf 'CMD'; for a in \"\$@\"; do printf ' <%s>' \"\$a\"; done; printf '\n'; }; source '${WORK}/ssh-recipe.sh'" \
  > "${WORK}/ssh-argv" 2>/dev/null || true
n_cmds="$(grep -c '^CMD' "${WORK}/ssh-argv" || true)"
[ "${n_cmds}" = "4" ] \
  || fail "docs/SSH.md's recipe says 'Four commands, each doing one part' but parses as ${n_cmds} wardyn invocations — a trailing comment or a stray continuation has split or merged one (F043)"
grep -qE '<[[:space:]]*>' "${WORK}/ssh-argv" \
  && fail "docs/SSH.md's recipe passes a BLANK argument to wardyn — a stray escaped space before a line continuation (\`\\ \\\`) hands \`wardyn run\` a literal one-space positional (F043)"
run_line="$(grep -F 'CMD <run> <--agent>' "${WORK}/ssh-argv" | head -1 || true)"
[ -n "${run_line}" ] || fail "docs/SSH.md's recipe no longer launches a run — re-anchor this guard (F043)"
for flag in --policy-file --description --interactive --json; do
  printf '%s' "${run_line}" | grep -qF "<${flag}>" \
    || fail "docs/SSH.md's recipe never passes ${flag} to \`wardyn run\` — it is written on a continuation line the shell does not join, so it parses as its own command instead: ${run_line} (F043)"
done
pass "C8e docs/SSH.md's external-tool recipe parses as the four commands it claims"

# F045 — the policy reference's only tool_rules example opened with
# `"tool_approvals": "hold"`, which is a run-CREATE request field, so the
# validation command the SAME page recommends (`wardyn policy render -f`)
# rejected the block outright. The field set is derived from RunPolicySpec.
spec_fields="$(sed -n '/type RunPolicySpec struct/,/^}/p' "${ROOT}/internal/types/policy.go" \
               | grep -oE 'json:"[a-z_]+' | cut -d'"' -f2 | sort -u || true)"
[ -n "${spec_fields}" ] || fail "could not read RunPolicySpec's json field names out of internal/types/policy.go — this guard would check nothing (F045)"
awk '/^```json$/{f=1;next} /^```$/{f=0} f' "${ROOT}/docs/POLICIES.md" \
  | grep -oE '^[[:space:]]*"[a-z_]+":' | tr -d ' ":' | sort -u > "${WORK}/policy-doc-keys"
[ -s "${WORK}/policy-doc-keys" ] || fail "no json keys found in docs/POLICIES.md's fenced blocks — this guard would check nothing (F045)"
while read -r k; do
  [ -n "${k}" ] || continue
  printf '%s\n' "${spec_fields}" | grep -qx "${k}" && continue
  # Nested object keys (a grant's `kind`/`scope`, a tool rule's `tool`/`effect`)
  # are not top-level spec fields; only flag the run-create fields that make the
  # whole block invalid.
  case "${k}" in
    tool_approvals|task_mode|interactive_start|seed_auto_tools|agent|repo|task|title|description|drive)
      fail "docs/POLICIES.md presents \`${k}\` inside a fenced json policy block, but it is a POST /runs request field, not a RunPolicySpec field — \`wardyn policy render -f\`, which the same page recommends, rejects the whole block with: invalid RunPolicySpec: json: unknown field \"${k}\" (F045)" ;;
  esac
done < "${WORK}/policy-doc-keys"
pass "C8f no fenced policy block in docs/POLICIES.md carries a run-create field"

# F054 — the socket table collapsed rootless Docker and Podman into one row and
# handed Podman the DOCKER socket path. The right path is read out of this
# repo's own Podman test, which agrees with podman-system-service(1).
podman_sock="$(grep -oE 'podman/podman\.sock' "${ROOT}/scripts/test-podman.sh" | head -1 || true)"
[ -n "${podman_sock}" ] || fail "scripts/test-podman.sh no longer names podman/podman.sock — this guard would check nothing (F054)"
desk_row="$(grep -n 'Rootless Podman' "${ROOT}/docs/DESKTOP.md" || true)"
[ -n "${desk_row}" ] || fail "docs/DESKTOP.md has no 'Rootless Podman' socket row — it collapsed Podman into the rootless-Docker row and gave it docker.sock (F054)"
printf '%s' "${desk_row}" | grep -qF "${podman_sock}" \
  || fail "docs/DESKTOP.md's Rootless Podman row does not name ${podman_sock}, the socket scripts/test-podman.sh defaults to and podman-system-service(1) documents (F054)"
pass "C8g DESKTOP.md's Podman socket row matches this repo's own Podman default"

echo "test-claims-match-code: self-test PASS"
