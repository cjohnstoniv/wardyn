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

echo "test-claims-match-code: self-test PASS"
