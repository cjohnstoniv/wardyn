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
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ── 1. nightly notification coverage ─────────────────────────────────────────
# e2e-live is the ONE deliberate exemption (pre-existing, uncharacterised
# failures — see nightly.yml's own comment). notify-new-lanes cannot need itself.
NIGHTLY=.github/workflows/nightly.yml
NOTIFY_EXEMPT="e2e-live notify-new-lanes"
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

if [ "$fail" = 0 ]; then echo "--- test-repo-guards: PASS ---"; else echo "--- test-repo-guards: FAIL ---"; fi
exit "$fail"
