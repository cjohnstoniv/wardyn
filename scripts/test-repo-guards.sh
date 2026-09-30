#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Static repo-conformance guards: three claims that CI made nowhere, each of
# which had already drifted. Daemon-free, network-free, seconds to run.
#
#   1. nightly.yml's failure notification covers every lane it is allowed to
#      cover — a new job added to the workflow and forgotten in `needs:` fails
#      silently forever otherwise (six of eight lanes did).
#   2. release.yml pins the cosign version on EVERY cosign-installer step, to
#      one version — the action's default floats, and `sign-blob`'s
#      --output-signature/--output-certificate flags exit 1 on cosign v3.
#   3. ARCHITECTURE.md's compose topology names every service the compose file
#      declares, in the right bucket (default vs profile) — the doc claimed a
#      two-service control plane and a three-member build-only profile.
#   4. no file still names WARDYN_STAGE_CLAUDE, a knob with no reader anywhere.
#   5. the L0 metadata block's own evidence is a connection fact, not a bare
#      curl exit code (see guard 5 below for the full story).
#   6. every `docker run`/`docker create` publish under scripts/ (-p, -p=, or
#      --publish) binds loopback-only — wardyn-test-pg (scripts/up.sh cmd_pg)
#      was the one 0.0.0.0 publish in the repo (B12b-F5).
#   7. deploy/kind/quickstart.sh's generated values state their own
#      confinement floor rather than inheriting the image's (R-01).
#   8. docker-compose.yaml's WARDYN_OIDC_ROLE_MAP stays a plain passthrough,
#      and deploy/compose/.env.example still seeds the demo/member pair for a
#      fresh install (R-03).
#   9. .gitleaksignore never grows a 4th un-scoped per-commit fingerprint for
#      the same path — that's a churning fixture that belongs in
#      .gitleaks.toml's path-scoped [allowlist] instead (SF-15).
#  10. every actions/upload-artifact step in .github/workflows/*.yml has a
#      non-empty `with.path` — a step landing between a `path: |` block and
#      its own globs folds the globs into ITS run: string instead, and the
#      upload silently gets an empty path forever (#372/SD-8; actionlint
#      does not catch this, since a present-but-empty `path:` is still a
#      valid input).
#  11. no Go name OPERATIONS.md's "Renamed in 0.8" table retires is still cited
#      outside that table, CHANGELOG.md or docs/design/ (#617).
#  12. every job whose name starts `notify-` in .github/workflows/*.yml calls
#      gh with GH_REPO set — none of them check out the repo, so without it
#      `gh` fails with "failed to run git: fatal: not a git repository" (#511,
#      #1069).
#  13. release.yml's publishing jobs (images, binaries, chart,
#      images-ui-sandbox, release-assets) all depend on preflight-green,
#      directly or transitively, and preflight-green has no `|| true` /
#      `continue-on-error` escape hatch (T-06, #666).
#  14. no demo/live spec or demo-take verifier still names an audit action
#      docs/AUDIT-ACTIONS.md's "Renamed in 0.8" table retires (#1020).
#  15. no script that boots a wardynd (the e2e backend, the kind SSO walk,
#      ci-run.sh, the Entra kind deploy, the survival walk and its compose
#      override) sets a model variable 0.8.2 retired — wardynd refuses to boot
#      on one (#549, #672). The compose files and Helm values are rendered by
#      `make compose-config` / `make helm-lint` instead.
#  16. nightly.yml's staged images are release.yml's publish set: every
#      published image has a staged row with the same name and Dockerfile, every
#      staged row is published, and the `images` rows' build-args are byte-equal.
#      A local-only image (claude-code, oracle, full) can never be staged. The
#      build step is pinned too: same context, file, platforms and provenance,
#      build-args from the matrix, and no target or cache on either side.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ── 1. nightly notification coverage ─────────────────────────────────────────
# e2e-live is the ONE deliberate exemption (known-fail, tracked in #965 and
# #1181 — see nightly.yml's own comment). notify-new-lanes cannot need itself.
# migration-merge-check is exempt too: it is red from its first run
# and will stay red for as long as the lead renumbers migrations at merge
# time (a live dry run found 0069 claimed by several open PRs) — its own red
# X and step summary are its signal, not a "Still failing" comment on the
# shared e2e-lane issue, which would mask a real e2e regression as queue
# hygiene noise.
# daemon-proxy-secret-kind (T-59, #719) went green on its first real nightly
# run (workflow_dispatch, 2026-09-28, run 36377937351) and is now in
# notify-new-lanes.needs + release.yml's watched= (guard 13) instead of here.
# test-e2e-concurrent (#697, T-37): new tonight, never run — watching it from
# day one would block a release on a job that might fail deterministically the
# first time it actually meets a hosted runner's docker/compose. Add it back to
# notify-new-lanes' needs: once it has gone green on a real nightly (same
# posture PR #1245 used for daemon-proxy-secret-kind).
# kind-survival-walk (#700), hybrid-walk (#701) and kind-upgrade-walk (#690)
# are exempt for the SAME reason PR #1245 gave daemon-proxy-secret-kind: none
# of the three has ever run on a real nightly yet, so watching them from day
# one would block every release on a job that might fail deterministically
# for an environment reason this authoring pass could not see (no kind cluster
# or Docker execution was available to prove them here — see each script's
# own header). Add all three back to notify-new-lanes' needs (and drop them
# from here) once each has gone green on a real scheduled run.
# provider-subscription-docker-pg (#677 T-17) went green on its first real
# nightly run (workflow_dispatch, 2026-09-28, run 36393687863) and is now in
# notify-new-lanes.needs + release.yml's watched= (guard 13) instead of here.
# managed-settings-drift (#1279, #1395) went green on hosted run 36786358051 and is now in
# notify-new-lanes.needs + release.yml's watched= (guard 13) instead of here.
# ci-mode-dogfood-model-fake (#681, T-21): new, never run on a hosted runner — a
# kind cluster plus the fake's image and a model-provider seed, any of which can
# fail for an environment reason on the first run. Same promotion rule as above.
# published-image-scan (and its own notify-published-image-scan): scans the images
# already published, so it must never block the next release — and the next
# release is the fix for a red here. It is NOT in release.yml's watched= (so not in
# notify-new-lanes.needs, which guard 13 pins to it); it opens its own issue.
NIGHTLY=.github/workflows/nightly.yml
NOTIFY_EXEMPT="e2e-live notify-new-lanes migration-merge-check test-e2e-concurrent kind-survival-walk hybrid-walk kind-upgrade-walk ci-mode-dogfood-model-fake published-image-scan notify-published-image-scan"
jobs="$(awk '/^jobs:/{j=1;next} j && /^  [a-z0-9-]+:$/{gsub(/[ :]/,"");print}' "$NIGHTLY" | tr '\n' ' ')"
needs="$(awk '/^  notify-new-lanes:$/{n=1;next} n && /^    needs:/{print;exit}' "$NIGHTLY")"
[ -n "$needs" ] || bad "$NIGHTLY: notify-new-lanes has no needs: line"
for job in $jobs; do
    case " $NOTIFY_EXEMPT " in *" $job "*) continue ;; esac
    case "$needs" in
        *"$job"*) ;;
        *) bad "$NIGHTLY: job '$job' is not in notify-new-lanes' needs: — it fails silently. Add it, or add it to NOTIFY_EXEMPT here with the reason." ;;
    esac
done
# …and the exemption list must not rot into a licence for a job that is gone.
for job in $NOTIFY_EXEMPT; do
    case " $jobs " in *" $job "*) ;; *) bad "$NIGHTLY: NOTIFY_EXEMPT names '$job', which is not a job any more — drop it" ;; esac
done
if [ "$fail" = 0 ]; then ok "nightly.yml notification covers every non-exempt lane"; fi

# ── 2. cosign pin parity ─────────────────────────────────────────────────────
# Every `uses: sigstore/cosign-installer` must be followed by an explicit
# cosign-release, and all of them must agree: one release, one signer.
REL=.github/workflows/release.yml
installers=$(grep -c 'uses: sigstore/cosign-installer' "$REL" || true)
pins=$(grep -c '^ *cosign-release:' "$REL" || true)
if [ "${installers:-0}" -lt 1 ]; then
    bad "$REL: no cosign-installer step found — this guard has lost its subject"
elif [ "${pins:-0}" != "${installers:-0}" ]; then
    bad "$REL: ${installers} cosign-installer step(s) but ${pins} cosign-release pin(s) — an unpinned step signs with whatever the action defaults to (cosign v3 rejects sign-blob's --output-signature/--output-certificate)"
else
    distinct=$(grep '^ *cosign-release:' "$REL" | tr -d ' "' | sort -u | wc -l)
    if [ "$distinct" != 1 ]; then
        bad "$REL: cosign-release pins disagree ($(grep '^ *cosign-release:' "$REL" | tr -d ' "' | sort -u | tr '\n' ' ')) — one release must not be signed by two cosign versions"
    else
        ok "release.yml pins one cosign version on all ${installers} installer steps"
    fi
fi

# ── 3. ARCHITECTURE.md compose topology ──────────────────────────────────────
# Parse the compose file with yq (already a repo tool) and check each service is
# named in ARCHITECTURE.md's compose section, in the bucket its profile implies.
COMPOSE=deploy/compose/docker-compose.yaml
ARCH=ARCHITECTURE.md
if ! command -v yq >/dev/null 2>&1; then
    echo "skip: yq not installed — ARCHITECTURE.md topology check needs it"
else
    # The compose section runs from its heading to the deployment-status note.
    section="$(awk '/^The compose stack \(/{s=1} s; /^> \*\*Deployment status:\*\*/{exit}' "$ARCH")"
    [ -n "$section" ] || bad "$ARCH: could not find the compose-stack section — re-point this guard"
    while IFS='|' read -r svc profiles; do
        [ -n "$svc" ] || continue
        # Case-insensitive whole-word: the doc writes the product name (Dex) for
        # some services and the compose key (`registry`) for others.
        printf '%s' "$section" | tr 'A-Z' 'a-z' | grep -qw -- "$svc" \
            || bad "$ARCH: compose service '$svc' (profiles: ${profiles:-<default>}) is not named in the compose-stack section"
    done <<EOF
$(yq -r '.services | to_entries[] | .key + "|" + ((.value.profiles // []) | join(","))' "$COMPOSE")
EOF
    # The default set is the load-bearing half: a service with no profile starts
    # on every `compose up`, so the doc must not describe a smaller control plane
    # than the file declares.
    defaults=$(yq -r '.services | to_entries[] | select((.value.profiles // []) | length == 0) | .key' "$COMPOSE" | sort | tr '\n' ' ')
    case "$defaults" in
        "postgres registry wardynd ") ok "ARCHITECTURE.md names every compose service; default set unchanged ($defaults)" ;;
        *) bad "$ARCH: the compose default service set changed to '$defaults' — update the Control plane bullet and this expectation together" ;;
    esac
fi

# ── 4. no references to knobs nothing reads ──────────────────────────────────
# WARDYN_STAGE_CLAUDE had an ENV.md row, an exemption on the env-doc guard's
# list, and no reader anywhere — not in Go, not in a script. The row and the
# exemption are gone; two prose mentions (Makefile, internal/api/setup.go) were
# the last thing keeping the fiction alive. The only files allowed to name it
# are the ones that RECORD its removal, this guard included.
DEAD_KNOB="WARDYN_STAGE_CLAUDE"
DEAD_KNOB_ALLOW="cmd/wardynd/envdoc_guard_test.go scripts/test-repo-guards.sh"
stale=""
# Tracked files only: a repo guard judges the repository, not the checkout. Generated
# artifacts (test/reports/**, gitignored) can carry the name for weeks after the source
# dropped it and would fail every developer while CI stays green.
for f in $(git ls-files -z | xargs -0 grep -l "$DEAD_KNOB" 2>/dev/null); do
    case " $DEAD_KNOB_ALLOW " in *" $f "*) continue ;; esac
    stale="$stale $f"
done
if [ -n "$stale" ]; then
    bad "$DEAD_KNOB has no reader anywhere but is still named in:$stale — implement it (and document it in docs/ENV.md) or drop the mention"
else
    ok "no live references to the reader-less $DEAD_KNOB knob"
fi

# ── 5. L0 block evidence reads a CONNECTION FACT, not a bare curl exit code ──
# curl exits 28 for two opposite facts: a connect() that never completed (what
# no route looks like) and a transfer that timed out AFTER the TCP handshake
# completed. So `rc != 0` is not proof of a block — an accept-and-hold listener
# on the metadata IP (a tarpit, an intercepting middlebox, a slow host) made
# BOTH of the release's L0 metadata checks report a connection the sandbox had
# actually established as "unreachable, no route". %{num_connects} is the fact
# that separates them (1 whenever a connection was made), so every script that
# certifies the L0 metadata block must record it and refuse a non-zero count.
# The same reading is already shipped in the redirect probe
# (internal/api/site_config_probe.go, redirectProbeScript).
l0_evidence_fail=0
for f in test/e2e/e2e.sh test/e2e/tasks/egress-boundary/solution.sh; do
    grep -q 'num_connects' "$f"         || { bad "$f: the metadata probe must record %{num_connects} — curl's exit code alone cannot tell 'never connected' from 'connected, then timed out', so an accept-and-hold host grades as blocked"; l0_evidence_fail=1; }
done
for f in test/e2e/e2e.sh test/e2e/tasks/egress-boundary/grade.sh; do
    grep -q 'ACCEPTED a TCP connection' "$f"         || { bad "$f: the metadata verdict must FAIL on a non-zero connect count (the 'ACCEPTED a TCP connection' arm) — recording the count and not judging it proves nothing"; l0_evidence_fail=1; }
done
grep -q 'metadata_connects.txt' test/e2e/tasks/egress-boundary/solution.sh     || { bad "test/e2e/tasks/egress-boundary/solution.sh: the connect count must be written as evidence (metadata_connects.txt) — grade.sh reads only the workspace"; l0_evidence_fail=1; }
if [ "$l0_evidence_fail" = 0 ]; then ok "the L0 metadata checks read %{num_connects}, and fail on a connection that was actually made"; fi

# ── 6. the TEST endpoint knobs are never shippable deployment config ────────
# WARDYN_AWS_SSO_ENDPOINT_OVERRIDE re-points AWS IAM Identity Center and
# WARDYN_ALLOW_TEST_ENDPOINTS unlocks it (plus plain-http WARDYN_BEDROCK_BASE_URL).
# They are boot-time TEST hatches — THREAT-MODEL residual #45 — and the kind SSO
# walk sets them through the chart's GENERIC `env:` passthrough with --set, which
# is deliberate: a named value, a documented example or a compose default would
# put a one-line edit between any operator and a daemon that trusts an arbitrary
# server as AWS. The plan's "helm/ci renders unchanged" requirement, as a gate.
test_knob_fail=0
for knob in WARDYN_AWS_SSO_ENDPOINT_OVERRIDE WARDYN_ALLOW_TEST_ENDPOINTS; do
    hits=$(grep -rln "$knob" deploy/helm deploy/compose 2>/dev/null || true)
    if [ -n "$hits" ]; then
        bad "$knob appears in a SHIPPED deployment artifact:$(printf ' %s' $hits) — it is a test hatch (THREAT-MODEL residual #45); the kind SSO walk sets it with \`--set env.<name>\` through the chart's generic passthrough, never as chart/compose config"
        test_knob_fail=1
    fi
done
if [ "$test_knob_fail" = 0 ]; then ok "neither AWS SSO test-endpoint knob is renderable from deploy/helm or deploy/compose"; fi

# ── 7. every `docker run`/`docker create` publish in scripts/ binds loopback ─
# wardyn-test-pg (scripts/up.sh cmd_pg) used to publish `-p 55432:5432` — 0.0.0.0
# on every interface, the one non-loopback docker publish anywhere in the
# repo — for a throwaway dev/e2e Postgres with a fixed demo password. Every
# consumer (scripts/e2e-backend.sh's default DSN, cmd_pg's own readiness poll)
# already dials localhost/127.0.0.1, so the wide bind bought nothing but LAN
# reachability into it. Joins `\`-continuation lines first (up.sh's own call
# splits `-p` onto its own line), so a wrap cannot hide a bare port from a
# per-line grep the way it would from one.
#
# R-07: the short-form-only (`-p`), `docker run`-only version of this guard
# had three blind spots it did not advertise — the long form `--publish`, the
# `-p=host:container` shape Go's flag parsing also accepts, and `docker
# create` (never started via `run`). extract_bad_publishes below is the ONE
# extraction used by both the self-test right after it (a deliberately-bad
# fixture line, so the extractor is proven to catch what it claims BEFORE it
# is trusted repo-wide) and the real scan.
extract_bad_publishes() {  # $1 = one shell-command line -> one bad host:port per line
    printf '%s\n' "$1" | grep -oE -- '(^| )(-p|--publish)[ =][^ ]+' \
        | sed -E 's/^ *(-p|--publish)[ =]//' \
        | grep -vE '^127\.0\.0\.1:'
}
selftest_fail=0
[ -n "$(extract_bad_publishes 'docker create --publish 0.0.0.0:1234:1234 postgres:17')" ] \
    || { bad "guard 6 self-test: a long-form --publish on a docker create line was not caught — the extractor checks nothing for that shape"; selftest_fail=1; }
[ -n "$(extract_bad_publishes 'docker run -p=6379:6379 redis:7')" ] \
    || { bad "guard 6 self-test: a -p=host:container (no space) was not caught"; selftest_fail=1; }
[ -z "$(extract_bad_publishes 'docker run -p 127.0.0.1:55432:5432 postgres:17')" ] \
    || { bad "guard 6 self-test: a loopback -p false-flagged"; selftest_fail=1; }
if [ "$selftest_fail" = 0 ]; then ok "guard 6's extractor catches --publish, -p=, and docker create (self-test)"; fi

port_bind_fail=0
for f in $(git ls-files -- scripts | grep '\.sh$'); do
    [ "$f" = "scripts/test-repo-guards.sh" ] && continue   # this guard's own source, not a docker-run/create site
    joined="$(sed -e ':a' -e 'N' -e '$!ba' -e 's/\\\n[[:space:]]*/ /g' "$f")"
    docker_lines="$(printf '%s\n' "$joined" | grep -E 'docker (run|create)' || true)"
    [ -n "$docker_lines" ] || continue
    while IFS= read -r line; do
        for val in $(extract_bad_publishes "$line"); do
            bad "$f: a docker run/create publishes $val, not loopback (want 127.0.0.1:<host-port>:<container-port>) — every docker publish in scripts/ must stay loopback-only (B12b-F5)"
            port_bind_fail=1
        done
    done <<EOF
$docker_lines
EOF
done
if [ "$port_bind_fail" = 0 ]; then ok "every docker run/create publish in scripts/ binds loopback only"; fi

# ── 8. deploy/kind/quickstart.sh's generated values still state a floor —
#      R-01, rewritten for 0.7.8. The B12b-F7 helm guard this originally
#      enforced is gone (the baked default floors at CC1 now, so the render it
#      refused is legal), but the quickstart should still say which floor its
#      cluster runs under rather than inheriting whatever the image ships — a
#      Fence-only kind cluster reading its floor from a future image change is
#      how the original trap was born. Grepping the SCRIPT SOURCE, not a
#      render, so an edit that strips the line fails here rather than in CI.
quickstart_values_heredoc="$(awk '/values\.yaml" <<EOF/{f=1;next} f&&/^EOF$/{exit} f{print}' deploy/kind/quickstart.sh)"
if printf '%s' "$quickstart_values_heredoc" | grep -qE 'WARDYN_DEFAULT_POLICY|(CC2|CC3): *"?[A-Za-z0-9]' ; then
    ok "deploy/kind/quickstart.sh's generated values state their own confinement floor"
else
    bad "deploy/kind/quickstart.sh's generated values.yaml heredoc names neither WARDYN_DEFAULT_POLICY nor a runtimeClasses pin — the cluster then inherits the image's floor instead of stating its own (R-01); see deploy/compose/docker-compose.yaml's WARDYN_DEFAULT_POLICY override for the shape"
fi

# ── 9. compose WARDYN_OIDC_ROLE_MAP stays a plain passthrough, and
#      .env.example still carries the seeded pair — R-03: a runtime `:-`
#      default on docker-compose.yaml's WARDYN_OIDC_ROLE_MAP applies to every
#      EXISTING deployment whose .env does not set it (":-" substitutes for
#      unset OR empty alike), silently moving deriveRole from its no-map arm
#      to its map-present arm and denying logins arm 1 would have allowed —
#      internal/auth/oidc's TestDeriveRoleComposeDefaultDeniesUnlistedLogin
#      proves the mechanism; this guard proves neither half of the R-03 fix
#      regresses: the compose file stays a bare passthrough, and a fresh
#      install still gets the pair (from .env.example, which env_set/
#      ensure_env_file only ever copies into a .env that does not exist yet).
compose_role_map_line="$(grep -E '^\s*WARDYN_OIDC_ROLE_MAP:' deploy/compose/docker-compose.yaml || true)"
case "$compose_role_map_line" in
    *'${WARDYN_OIDC_ROLE_MAP:-}'*) ok "docker-compose.yaml's WARDYN_OIDC_ROLE_MAP is a plain passthrough (no runtime default)" ;;
    *) bad "docker-compose.yaml's WARDYN_OIDC_ROLE_MAP is not the bare passthrough \"\${WARDYN_OIDC_ROLE_MAP:-}\" any more (got: ${compose_role_map_line:-<no row found>}) — a non-empty \`:-\` default here silently denies logins on every upgraded deployment (R-03); see TestDeriveRoleComposeDefaultDeniesUnlistedLogin" ;;
esac
if grep -qE '^WARDYN_OIDC_ROLE_MAP=demo@wardyn\.local=admin,member@wardyn\.local=user\s*$' deploy/compose/.env.example; then
    ok "deploy/compose/.env.example still seeds the demo/member role-map pair for a fresh .env"
else
    bad "deploy/compose/.env.example no longer carries an UNCOMMENTED WARDYN_OIDC_ROLE_MAP=demo@wardyn.local=admin,member@wardyn.local=user row — a fresh compose stack would lose the second identity the member-mode rider added, and this is the ONLY safe place for it (R-03: a docker-compose.yaml runtime default would apply to upgrades too)"
fi

# ── 9. .gitleaksignore fingerprint churn — a fixture whose flagged value
#      changes every commit (a uuid-suffixed fake token, say) mints a fresh
#      commit fingerprint for the same path every time, so .gitleaksignore
#      grows one entry per commit forever instead of being fixed once. A path
#      trips this only when BOTH hold: it carries >=4 fingerprints in total
#      (path-wide, since the fixture's own line drifts as unrelated edits
#      land above it — line alone under-counts) AND at least one single
#      file:line under that path repeats >=3 times (the churn signature: the
#      same spot re-fingerprinted commit after commit). A file legitimately
#      allowlisted at several distinct STABLE lines (docs/OPERATIONS.md, say)
#      never repeats any one line that often, so it does not trip. Past that,
#      it belongs in .gitleaks.toml's path-scoped [allowlist] instead (SF-15)
#      — this guard refuses to let a new one accumulate un-scoped the way
#      internal/egress/proxy/pat_broker_mask_test.go's did.
churn_fail=0
gitleaksignore_paths="$(grep -v '^#' .gitleaksignore | grep -v '^[[:space:]]*$' | awk -F: '{print $2}' | sort -u)"
for p in $gitleaksignore_paths; do
    total="$(grep -v '^#' .gitleaksignore | grep -v '^[[:space:]]*$' | awk -F: -v p="$p" '$2==p' | wc -l)"
    [ "$total" -ge 4 ] || continue
    hot_line_count="$(grep -v '^#' .gitleaksignore | grep -v '^[[:space:]]*$' | awk -F: -v p="$p" '$2==p{print $4}' | sort | uniq -c | sort -rn | head -1 | awk '{print $1+0}')"
    [ "${hot_line_count:-0}" -ge 3 ] || continue
    esc_p="$(printf '%s' "$p" | sed 's/\./\\./g')"
    grep -qF -- "$esc_p" .gitleaks.toml && continue   # already path-scoped: redundant fingerprints, not churn
    bad ".gitleaksignore: '$p' carries $total per-commit fingerprints ($hot_line_count at one file:line) and is not in .gitleaks.toml's path-scoped [allowlist] — a fixture whose flagged value changes every commit belongs there instead (SF-15), not a growing pile of fingerprints"
    churn_fail=1
done
if [ "$churn_fail" = 0 ]; then ok "no .gitleaksignore path has un-scoped fingerprint churn (>=4 entries, >=3 at one line)"; fi

# ── 10. every upload-artifact step has a non-empty path ─────────────────────
if ! command -v yq >/dev/null 2>&1; then
    echo "skip: yq not installed — upload-artifact path check needs it"
else
    empty_upload_fail=0
    for wf in .github/workflows/*.yml; do
        empty_steps="$(yq -r '
            .jobs[].steps[]?
            | select((.uses // "") | test("^actions/upload-artifact@"))
            | select((.with.path // "" | trim) == "")
            | (.name // .uses)
        ' "$wf")"
        [ -z "$empty_steps" ] && continue
        while IFS= read -r step; do
            bad "$wf: upload-artifact step '$step' has an empty with.path — nothing uploads, and the job stays green with no reports attached (#372/SD-8)"
        done <<<"$empty_steps"
        empty_upload_fail=1
    done
    if [ "$empty_upload_fail" = 0 ]; then ok "every upload-artifact step across .github/workflows/*.yml has a non-empty path"; fi
fi

# ── 11. retired 0.8 Go names stay retired (#617) ────────────────────────────
# The names come from the table's own "| Go:" rows (Pre-0.8 column), so a row
# added there is guarded without touching this script. docs/design/ is a
# point-in-time planning record and CHANGELOG.md is history; both keep them.
retired="$(awk -F'|' '/^## Renamed in 0.8/{f=1;next} f&&/^## /{exit} f&&$2~/^ Go:/{print $3}' docs/OPERATIONS.md \
    | grep -oE '`[A-Za-z_.]+`' | tr -d '`' | sed 's/.*\.//' | sort -u)"
[ -n "$retired" ] || bad "docs/OPERATIONS.md's 'Renamed in 0.8' table has no Go: rows — guard 11 is pointing at nothing"
stale=0
for name in $retired; do
    hits="$(git grep -nw "$name" -- ':!CHANGELOG.md' ':!docs/design/' | grep -v '^docs/OPERATIONS.md:[0-9]*:| Go:' || true)"
    [ -z "$hits" ] || { stale=1; bad "retired 0.8 name '$name' is still cited (see docs/OPERATIONS.md 'Renamed in 0.8'):
$hits"; }
done
if [ -n "$retired" ] && [ "$stale" = 0 ]; then ok "no retired 0.8 Go name is cited outside the rename table, CHANGELOG.md or docs/design/"; fi

# ── 12. every notify-* job carries GH_REPO ───────────────────────────────────
notify_gh_repo_fail=0
for wf in .github/workflows/*.yml; do
    for job in $(awk '/^jobs:/{j=1;next} j && /^  [a-z0-9-]+:$/{gsub(/[ :]/,"");print}' "$wf"); do
        case "$job" in
            notify-*) ;;
            *) continue ;;
        esac
        job_block="$(awk -v j="  $job:" '$0==j{f=1;next} f&&/^  [a-z0-9-]+:$/{exit} f{print}' "$wf")"
        printf '%s' "$job_block" | grep -qF 'GH_REPO: ${{ github.repository }}' \
            || { bad "$wf: $job calls gh without GH_REPO (no checkout)"; notify_gh_repo_fail=1; }
    done
done
if [ "$notify_gh_repo_fail" = 0 ]; then ok "every notify-* job carries GH_REPO"; fi

# ── 13. release.yml's publishing jobs all sit behind preflight-green ────────
# preflight-green (branch-protection required contexts + the watched nightly
# jobs, both checked on the tag commit) exists to stop `images`, `binaries`
# and `chart` from publishing on an unvetted commit — a `needs:` edge dropped
# by a later edit would silently remove that gate. `images-ui-sandbox` and
# `release-assets` carry no DIRECT `needs: preflight-green` (they need
# `images`/the built set instead), so this walks each publishing job's
# `needs:` graph and requires preflight-green to be reachable somewhere in
# it, not just spelled out on the job itself.
#
# R2-2 (review round 2): release.yml's `watched=` list (the nightly jobs
# preflight-green judges) is a hand-typed string, not derived from anything —
# it must stay identical to nightly.yml's notify-new-lanes.needs (minus the
# jobs named "multi-arch build (<matrix.name>)", which release.yml matches by
# job-name PREFIX instead, since their API job name is never the literal YAML
# key) or a lane added to nightly and to notify-new-lanes silently stops
# being release-gated. This guard is what makes that parity enforced instead
# of assumed; on CI (yq preinstalled on ubuntu-latest) a missing yq fails the
# guard rather than skipping it, since a skip in CI is not evidence of
# anything.
if ! command -v yq >/dev/null 2>&1; then
    if [ "${CI:-}" = "true" ]; then
        bad "yq not installed — guard 13 (preflight-green) cannot run in CI and a skip here proves nothing"
    else
        echo "skip: yq not installed — preflight-green guard needs it"
    fi
else
    preflight_fail=0
    PUBLISH_JOBS="images binaries chart images-ui-sandbox release-assets"
    reaches_preflight() {  # $1 = job name, $2 = space-separated jobs already visited (cycle guard)
        local job="$1" seen="$2" needs n
        case " $seen " in *" $job "*) return 1 ;; esac
        seen="$seen $job"
        needs="$(yq -r ".jobs[\"$job\"].needs // [] | ([.] | flatten) | .[]" "$REL" 2>/dev/null)"
        [ -n "$needs" ] || return 1
        for n in $needs; do
            [ "$n" = "preflight-green" ] && return 0
            reaches_preflight "$n" "$seen" && return 0
        done
        return 1
    }
    for job in $PUBLISH_JOBS; do
        reaches_preflight "$job" "" \
            || { bad "$REL: job '$job' does not depend on preflight-green, directly or transitively — a release could publish without the required-checks/nightly gate"; preflight_fail=1; }
    done
    # The job's own source text (steps' run: blocks are plain strings, not YAML
    # comments, so this catches a neutered check as-written, not just its shape).
    # `|| :` is the shell no-op colon builtin — an equally silent escape hatch
    # to `|| true`, and one review round found it untested.
    preflight_block="$(awk '$0=="  preflight-green:"{f=1;next} f&&/^  [a-z0-9-]+:$/{exit} f{print}' "$REL")"
    [ -n "$preflight_block" ] || { bad "$REL: no 'preflight-green:' job found — guard 13 is pointing at nothing"; preflight_fail=1; }
    if printf '%s' "$preflight_block" | grep -qE '\|\| *true|\|\| *:([[:space:]]|$)|continue-on-error'; then
        bad "$REL: preflight-green contains \`|| true\`, \`|| :\`, or \`continue-on-error\` — its gate can be satisfied without actually being green"
        preflight_fail=1
    fi
    # R2-2: watched= must equal notify-new-lanes.needs minus the multi-arch jobs —
    # neither list may drift from the other without this guard going red.
    rel_watched="$(printf '%s' "$preflight_block" | grep -m1 '^ *watched="' | sed -E 's/^ *watched="([^"]*)".*/\1/')"
    if [ -z "$rel_watched" ]; then
        bad "$REL: preflight-green has no \`watched=\"...\"\` line — guard 13's parity check is pointing at nothing"
        preflight_fail=1
    else
        nightly_needs_raw="$(yq -r '.jobs["notify-new-lanes"].needs[]' "$NIGHTLY" 2>/dev/null)"
        # Every nightly job named "multi-arch build (...)" is matched by that name
        # PREFIX in release.yml, never listed in watched=.
        nightly_multiarch="$(yq -r '.jobs | to_entries[] | select((.value.name // "") | test("^multi-arch build \\(")) | .key' "$NIGHTLY" 2>/dev/null)"
        printf '%s\n' "$nightly_multiarch" | grep -qx 'buildx-smoke' \
            || { bad "$NIGHTLY: buildx-smoke no longer has a \`multi-arch build (...)\` name — release.yml's separate matrix-row handling for it is now pointing at nothing"; preflight_fail=1; }
        printf '%s\n' "$nightly_needs_raw" | grep -qx 'buildx-smoke' \
            || { bad "$NIGHTLY: notify-new-lanes.needs no longer lists buildx-smoke — release.yml's separate matrix-row handling for it is now pointing at nothing"; preflight_fail=1; }
        nightly_watched_sorted="$(printf '%s\n' "$nightly_needs_raw" | grep -vxF -f <(printf '%s\n' "$nightly_multiarch") | sort)"
        rel_watched_sorted="$(printf '%s\n' $rel_watched | sort)"
        if [ "$rel_watched_sorted" != "$nightly_watched_sorted" ]; then
            bad "$REL: preflight-green's watched= list has drifted from $NIGHTLY's notify-new-lanes.needs (minus the multi-arch build jobs) — watched=[$(printf '%s ' $rel_watched_sorted)] vs needs=[$(printf '%s ' $nightly_watched_sorted)]. A lane added to (or renamed in) notify-new-lanes must be added to watched= too, or it silently stops gating a release."
            preflight_fail=1
        fi
    fi
    if [ "$preflight_fail" = 0 ]; then ok "images/binaries/chart/images-ui-sandbox/release-assets all depend on preflight-green (no silent-pass escape hatch), and its watched= list matches notify-new-lanes.needs"; fi
fi

# ── 14. demo/live specs keep up with the 0.8 audit action renames (#1020) ────
# The specs that film or walk a live console and the demo-take verifiers run
# in no CI gate, so a stale action name there fails only when a take is shot
# again (demo 07's search string, train 18). The old names come from the
# table's own rows, plus each one's underscore-joined segment that its new name
# and its "tells apart" value dropped — the fragment a search box is typed with
# (demo 07 typed "subscription_inject"). Matched whole, so a new name that
# extends an old one (session.recording.write) is not a hit.
old_actions="$(awk -F'|' '/^## Renamed in 0.8/{f=1;next} f&&/^## /{exit} f&&$2~/^ `/{o=$2; gsub(/[` ]/,"",o); print o; rest=$3 $4; n=split(o,seg,"."); for(i=1;i<=n;i++) if (seg[i]~/_/ && index(rest,seg[i])==0) print seg[i]}' docs/AUDIT-ACTIONS.md | sort -u)"
[ -n "$old_actions" ] || bad "docs/AUDIT-ACTIONS.md's 'Renamed in 0.8' table has no rows — guard 14 is pointing at nothing"
stale_actions=0
for name in $old_actions; do
    # git grep: 0 = hits, 1 = none, anything else = the grep itself failed
    # (no PCRE in this git, a bad pattern), which must never read as clean.
    hits="$(git grep -nP "(?<![\\w.])${name//./\\.}(?!\\.?\\w)" -- 'ui/e2e/demo/**' 'ui/e2e/live*/**' 'scripts/lib/verify-demo-take-*')" && rc=0 || rc=$?
    if [ "$rc" -gt 1 ]; then stale_actions=1; bad "guard 14: git grep failed (rc=$rc) for '$name'"; continue; fi
    [ -z "$hits" ] || { stale_actions=1; bad "retired audit action name '$name' is still used by a demo/live spec or demo-take verifier (see docs/AUDIT-ACTIONS.md 'Renamed in 0.8'):
$hits"; }
done
if [ -n "$old_actions" ] && [ "$stale_actions" = 0 ]; then ok "no demo/live spec or demo-take verifier names a retired 0.8 audit action"; fi
# ── 15. no wardynd-booting script sets a retired model variable ─────────────
retired_re='(WARDYN_(ANTHROPIC|OPENAI|BEDROCK)_[A-Z_]+|WARDYN_AGENT_ANTHROPIC_MODEL|WARDYN_SUBSCRIPTION_INJECT|WARDYN_ALLOW_SHARED_SUBSCRIPTION)'
retired_fail=0
for f in scripts/e2e-backend.sh scripts/kind-sso-walk.sh scripts/ci-run.sh scripts/survival-walk.sh \
         deploy/azure-entra-sso/05-kind-deploy.sh test/survival-walk/compose-override.yaml; do
    # An assignment (VAR=value, export VAR=), a Helm --set env.VAR=value, or a
    # YAML key (VAR: value) with a non-empty value other than false/off.
    hits="$(grep -nE "(^|[[:space:]\"'.])${retired_re}(=|: +)[\"']?[^\"'[:space:]]" "$f" \
        | grep -vE "${retired_re}(=|: +)[\"']?(false|off)[\"']?([[:space:]]|$)" || true)"
    if [ -n "$hits" ]; then
        bad "$f sets a retired model variable wardynd refuses to boot on (#549): $hits"
        retired_fail=1
    fi
done
[ "$retired_fail" = 0 ] && ok "no wardynd-booting script sets a retired model variable"

# ── 16. nightly's staged rows are release.yml's publish set ─────────────────
# A dispatched nightly pushes its `stage: "true"` rows (and the ui-sandbox job's
# two) to ghcr.io/cjohnstoniv/staging/, and release.yml promotes those digests.
# So a staged row that drifts from the release row's Dockerfile or build-args
# promotes an image release.yml would never have built, and an image that is
# published but not staged has nothing to promote. The publish set is the
# `- name:` idiom check-image-pins.sh uses. A missing yq fails in CI like guard 13.
if ! command -v yq >/dev/null 2>&1; then
    if [ "${CI:-}" = "true" ]; then
        bad "yq not installed — guard 16 (staging parity) cannot run in CI and a skip here proves nothing"
    else
        echo "skip: yq not installed — staging parity guard needs it"
    fi
else
    stage_fail=0
    published="$(grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' "$REL" | awk '{print $3}' | sort -u || true)"
    [ -n "$published" ] || { bad "$REL: no '- name:' image entries found — guard 16 is pointing at nothing"; stage_fail=1; }
    rel_pairs="$(yq -r '.jobs[].strategy.matrix.include[]? | select(.dockerfile) | .name + " " + .dockerfile' "$REL" | sort -u)"
    staged_pairs="$( { yq -r '.jobs["buildx-smoke"].strategy.matrix.include[] | select(.stage == "true") | .name + " " + .dockerfile' "$NIGHTLY"
                       yq -r '.jobs["buildx-smoke-ui-sandbox"].strategy.matrix.include[] | .name + " " + .dockerfile' "$NIGHTLY"; } | sort -u)"
    for n in $published; do
        want="$(printf '%s\n' "$rel_pairs" | grep -E "^$n " || true)"
        [ -n "$want" ] || { bad "$REL: published image '$n' has no matrix row with a dockerfile — guard 16 cannot compare it"; stage_fail=1; continue; }
        printf '%s\n' "$staged_pairs" | grep -qxF "$want" \
            || { bad "$NIGHTLY: published image '$n' has no staged row with release.yml's dockerfile ('$want') in buildx-smoke (stage: \"true\") or buildx-smoke-ui-sandbox — a release would have nothing to promote"; stage_fail=1; }
    done
    for n in $(printf '%s\n' "$staged_pairs" | awk '{print $1}'); do
        printf '%s\n' "$published" | grep -qx "$n" \
            || { bad "$NIGHTLY: staged image '$n' is not in $REL's publish set — a local-only or vendor-CLI image must never be pushed to staging"; stage_fail=1; }
    done
    for n in $(yq -r '.jobs.images.strategy.matrix.include[].name' "$REL"); do
        rel_args="$(yq -r ".jobs.images.strategy.matrix.include[] | select(.name == \"$n\") | .[\"build-args\"] // \"\"" "$REL")"
        stage_args="$(yq -r ".jobs[\"buildx-smoke\"].strategy.matrix.include[] | select(.name == \"$n\") | .[\"build-args\"] // \"\"" "$NIGHTLY")"
        [ "$rel_args" = "$stage_args" ] \
            || { bad "$NIGHTLY: '$n' build-args differ from $REL's images row — release: [$rel_args] vs nightly: [$stage_args]"; stage_fail=1; }
    done
    # The build STEP decides what a digest is, so it is compared too: a staged
    # image equals a release.yml build only if context, file, platforms,
    # provenance and build-args agree, and neither side carries a target,
    # cache-from, cache-to or any other key the other lacks.
    bps='.steps[] | select((.uses // "") | test("^docker/build-push-action@"))'
    with_norm="$bps | .with | del(.push) | del(.tags) | del(.labels) | .[\"build-args\"] |= (. // \"\" | sub(\"\\n+\$\"; \"\")) | sort_keys(.)"
    for pair in "images buildx-smoke" "images-ui-sandbox buildx-smoke-ui-sandbox"; do
        set -- $pair
        rel_job="$1"; ngt_job="$2"
        for jf in "$rel_job $REL" "$ngt_job $NIGHTLY"; do
            set -- $jf
            [ "$(yq -r ".jobs[\"$1\"] | [$bps] | length" "$2")" = 1 ] \
                || { bad "$2: job '$1' must have exactly one docker/build-push-action step — guard 16 cannot compare the build step"; stage_fail=1; }
        done
        rel_with="$(yq -o=json -I=0 ".jobs[\"$rel_job\"] | $with_norm" "$REL")"
        ngt_with="$(yq -o=json -I=0 ".jobs[\"$ngt_job\"] | $with_norm" "$NIGHTLY")"
        [ "$rel_with" = "$ngt_with" ] \
            || { bad "$NIGHTLY: $ngt_job's build step differs from $REL's $rel_job (push, tags and labels aside) — release: $rel_with vs nightly: $ngt_with. A staged digest must be built exactly as release.yml builds it: same platforms and provenance, no target or cache."; stage_fail=1; }
    done
    want_args='${{ matrix.build-args }}'
    got_args="$(yq -r ".jobs[\"buildx-smoke\"] | $bps | .with[\"build-args\"]" "$NIGHTLY")"
    [ "$got_args" = "$want_args" ] \
        || { bad "$NIGHTLY: buildx-smoke's build step must take build-args from the matrix row ('$want_args'), not '$got_args' — a hardcoded value would stage wardynd without RELEASE_BUILD=true"; stage_fail=1; }
    want_base='BASE_IMAGE=${{ steps.base.outputs.ref }}'
    got_base="$(yq -r ".jobs[\"buildx-smoke-ui-sandbox\"] | $bps | .with[\"build-args\"]" "$NIGHTLY")"
    [ "$got_base" = "$want_base" ] \
        || { bad "$NIGHTLY: buildx-smoke-ui-sandbox's build-args must be exactly '$want_base' (the staged agent-base digest), not '$got_base'"; stage_fail=1; }
    base_run="$(yq -r '.jobs["buildx-smoke-ui-sandbox"].steps[] | select(.id == "base") | .run' "$NIGHTLY")"
    printf '%s' "$base_run" | grep -qF '@sha256:[0-9a-f]{64}$' \
        || { bad "$NIGHTLY: buildx-smoke-ui-sandbox's base step no longer checks the ref is an @sha256 digest"; stage_fail=1; }
    if [ "$stage_fail" = 0 ]; then ok "nightly's staged rows and build steps match release.yml's publish set (names, dockerfiles, build-args, platforms, provenance, no cache), and no local-only image is staged"; fi
fi

if [ "$fail" = 0 ]; then echo "--- test-repo-guards: PASS ---"; else echo "--- test-repo-guards: FAIL ---"; fi
exit "$fail"
