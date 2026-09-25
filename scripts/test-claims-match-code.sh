#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-claims-match-code.sh — daemon-free, network-free assertions that what
# Wardyn SAYS matches what it DOES. #209 (lane 2a) cut this script down from 34
# `grep -q` checks (C1-C9) to the ones that either compare two files no other
# test watches, or drive a shell function against a stub (a behaviour a `grep`
# cannot pin). What that pass did with each original check:
#
#   KEPT here, unchanged:
#     C3   docs/VERIFY.md's verification commands stay parameterised on
#          $WARDYN_VERSION, never a hard-coded release number.
#     C6   RELEASING.md's required-status-checks `contexts` cover `notices`
#          and every cell of ci.yml's `trivy` matrix (F121).
#     C7   ENV.md/VERIFY.md/THREAT-MODEL/OPERATIONS.md each match the code or
#          compose file they describe (F081/F091/F155).
#     C8a  docs/ci/github-actions.yml and azure-pipelines.yml pin the wardyn
#          checkout they EXECUTE at the shipped version (F007, security: an
#          unpinned pin runs tip-of-default-branch shell in a secret-bearing
#          job).
#     C8c  docs/OPERATIONS.md's migrator/app-role rationale names every
#          migration that needs pre-existing-object ownership (F034).
#     C9   every fenced "helm install wardyn" recipe across the front-door
#          docs carries an age-key source and, with k8s.enabled=true, a
#          runs-namespace choice (X1a-F1/X1c-F1).
#     R-02 every internal/runner/k8s `.List(` call has a matching "list" verb
#          on the chart's Role — the one thing a fake-clientset k8s test can
#          never catch.
#
#   MOVED to scripts/test-up-probes.sh (they drive a scripts/up.sh function
#   against a stub — behavioural tests that belong beside that file's other
#   up.sh extractions, not prose greps):
#     C1   wardynd_probe() reports the probe container's own failure instead
#          of discarding its stderr (perf2-C3).
#     C4   up.sh's cosign_verify() pins the certificate identity and checks
#          the SBOM attestation before up.sh claims an image is signed
#          (F005/F100) — security: that binary then runs ON THE HOST.
#     C5   no service the pull-first block retags to :local is rebuilt
#          unconditionally afterwards (F099).
#
#   REMOVED (prose-only pins with no derived fact behind them, and no Go guard
#   under cmd/wardynd/*_test.go covering the same fact either — see PR #952
#   and its follow-up for the per-check search): C2 (WARDYN_SUBSCRIPTION_TOKEN
#   doc wording), C8b (release-asset doc enumeration), C8d (README hero /
#   TRY-IT.md), C8e (SSH.md recipe), C8f (POLICIES.md fenced examples), C8g
#   (DESKTOP.md Podman socket).
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

# ── C8a: both pasteable CI pipelines pin the wardyn checkout they EXECUTE ───
#
# F007 — both pasteable pipelines fetch cjohnstoniv/wardyn and then EXECUTE that
# checkout's scripts/ci-run.sh inside a job the same files tell you to seed with
# the consumer's ANTHROPIC_API_KEY. Unpinned, that is every push to Wardyn's
# default branch running shell in front of a stranger's secrets. The expected
# pin is DERIVED from the shipped version, so a release bump that forgets these
# two files fails here rather than shipping a pipeline pinned to last release.
GH_CI="${ROOT}/docs/ci/github-actions.yml"
AZ_CI="${ROOT}/docs/ci/azure-pipelines.yml"
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

# ── C8c: the migrator rationale names every ownership-requiring migration ───
#
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

# ── C9: every "helm install wardyn" recipe carries its render-time refusals ─
#
# X1a-F1 / X1c-F1 (v0.7.4) — three front-door "helm install wardyn" recipes
# rendered non-zero: values.yaml ships a non-empty postgres.dsn.secretRef.name
# default, so templates/secret.yaml `fail`s any install that doesn't wire an
# age identity; a k8s.enabled=true install with no runs-namespace choice
# `fail`s at templates/rbac.yaml; and a k8s.enabled=true install with no
# CC2/CC3 RuntimeClass pin and no default-policy override `fail`s at
# templates/deployment.yaml (deploy-scripts' B12b-F7 guard — D-1, v0.7.4
# review). Nothing caught any of this, because a fenced shell recipe is prose
# to every other guard here. Extracts every fenced ```sh/```bash block across
# the front-door docs that pastes a `helm install`/`helm upgrade --install
# wardyn` command and checks it against all three render-time refusals.
# Ceiling: text-only (does not actually `helm template` the block) — an
# elided `...` placeholder snippet is excluded, not validated.
helm_recipe_docs="README.md docs/VERIFY.md .claude/skills/wardyn-k8s-setup/SKILL.md deploy/helm/wardyn/README.md"
n_blocks=0
for relpath in ${helm_recipe_docs}; do
  doc="${ROOT}/${relpath}"
  [ -f "${doc}" ] || fail "${relpath} moved — this guard would check nothing (C9)"
  rm -f "${WORK}"/c9-block-*
  awk -v out="${WORK}/c9-block-" '
    /^[[:space:]]*```/ {
      if (fenced) { fenced = 0; print block > (out n); close(out n); n++; block = "" }
      else { fenced = 1; block = "" }
      next
    }
    fenced { block = block $0 "\n" }
  ' "${doc}"
  for b in "${WORK}"/c9-block-*; do
    [ -f "${b}" ] || continue
    grep -qE 'helm (install|upgrade --install) wardyn([[:space:]]|$)' "${b}" || continue
    grep -qF '...' "${b}" && continue   # elided partial snippet, not a pasteable recipe
    n_blocks=$((n_blocks + 1))
    grep -qE 'secrets\.(ageKeyFromSecret|allowEphemeralAgeKey)|postgres\.dsn\.secretRef\.name=""' "${b}" \
      || fail "${relpath} has a fenced 'helm install wardyn' block with no age-key source (secrets.ageKeyFromSecret / secrets.allowEphemeralAgeKey / an explicit postgres.dsn.secretRef.name=\"\") — postgres.dsn.secretRef.name defaults non-empty (values.yaml), so this render fails closed at templates/secret.yaml (C9): $(head -1 "${b}")"
    if grep -qE 'k8s\.enabled=true' "${b}"; then
      grep -qE 'k8s\.(runsNamespace|allowRunsInReleaseNamespace)' "${b}" \
        || fail "${relpath} has a fenced k8s.enabled=true 'helm install wardyn' block with no runs-namespace choice (k8s.runsNamespace or k8s.allowRunsInReleaseNamespace) — templates/rbac.yaml fails this render (C9): $(head -1 "${b}")"
    fi
  done
done
rm -f "${WORK}"/c9-block-*
[ "${n_blocks}" -ge 3 ] || fail "found only ${n_blocks} fenced 'helm install wardyn' blocks across the front-door docs — this guard would check nothing (C9)"
pass "C9 every fenced 'helm install wardyn' block carries an age-key source and, with k8s.enabled=true, a runs-namespace choice and a CC2/CC3 pin or default-policy override"

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
