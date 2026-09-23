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
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ── 1. nightly notification coverage ─────────────────────────────────────────
# e2e-live is the ONE deliberate exemption (pre-existing, uncharacterised
# failures — see nightly.yml's own comment). notify-new-lanes cannot need
# itself. migration-merge-check is exempt too: it is red from its first run
# and will stay red for as long as the lead renumbers migrations at merge
# time (a live dry run found 0069 claimed by several open PRs) — its own red
# X and step summary are its signal, not a "Still failing" comment on the
# shared e2e-lane issue, which would mask a real e2e regression as queue
# hygiene noise.
NIGHTLY=.github/workflows/nightly.yml
NOTIFY_EXEMPT="e2e-live notify-new-lanes migration-merge-check"
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
if grep -qE '^WARDYN_OIDC_ROLE_MAP=demo@wardyn\.local=admin,member@wardyn\.local=member\s*$' deploy/compose/.env.example; then
    ok "deploy/compose/.env.example still seeds the demo/member role-map pair for a fresh .env"
else
    bad "deploy/compose/.env.example no longer carries an UNCOMMENTED WARDYN_OIDC_ROLE_MAP=demo@wardyn.local=admin,member@wardyn.local=member row — a fresh compose stack would lose the second identity the member-mode rider added, and this is the ONLY safe place for it (R-03: a docker-compose.yaml runtime default would apply to upgrades too)"
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

if [ "$fail" = 0 ]; then echo "--- test-repo-guards: PASS ---"; else echo "--- test-repo-guards: FAIL ---"; fi
exit "$fail"
