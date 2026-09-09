#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-image-pins.sh — two Dockerfile/compose distribution invariants:
#   1. every image a Dockerfile FROM or a compose file pulls is digest-pinned;
#   2. every Dockerfile that produces a PUBLISHED image carries its licence files
#      and OCI labels.
#
# FAILS if a Dockerfile `FROM` (a registry image, not a build-stage alias) or a
# compose registry image lacks an @sha256 digest. A floating tag lets an
# attacker who controls the upstream tag swap the image contents; the digest
# freezes exactly what we build/run.
#
# SCOPE is those two file sets plus the `make setup` shell path (see the third
# section) — images pulled from anywhere else (e.g. the Makefile's throwaway
# test infra: registry:2, postgres:17, alpine:latest, and the test scripts'
# own helper containers) are deliberately outside this gate, so do not read a
# green run as "nothing floats".
#
# Exempt:
#   - compose `*:local` tags   — locally BUILT images (they carry a `build:` stanza),
#                                 including behind a `${VAR:-...:local}` override knob
#   - $ALLOWLIST_REFS           — the documented local retags of :local images
# Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

# Matched on the REF, not the file: exempting a whole Dockerfile would silently
# waive every OTHER FROM in it (full/ also pulls digest-pinned toolchain stages).
# Matched on the REF, one per line. Both entries are locally BUILT images, so no
# upstream digest exists to pin them to — a digest would have to be recomputed on
# every rebuild of the base, which is neither stable nor meaningful.
#   wardyn/agent-claude-code:local  deploy/images/{full,vscode}/Dockerfile's base
#   wardyn/agent-base:local         deploy/images/novnc/Dockerfile's base — the
#                                   noVNC image is FROM agent-base deliberately
#                                   (an X stack needs no language runtime), which
#                                   is why it needs its own entry rather than
#                                   riding the one above.
ALLOWLIST_REFS="wardyn/agent-claude-code:local
wardyn/agent-base:local"
fail=0

# ── Dockerfile FROMs ────────────────────────────────────────────────────────
while IFS= read -r df; do
  # Build-stage aliases (FROM <img> AS <alias>) get re-referenced by name later
  # (FROM <alias>); those refs are not external images and need no digest.
  mapfile -t aliases < <(grep -iE '^FROM .+ [Aa][Ss] ' "$df" | sed -E 's/.* [Aa][Ss] +([^ ]+).*/\1/')
  while IFS= read -r line; do
    # The ref is the first token after FROM that is not a FLAG: a build stage
    # may carry `FROM --platform=$BUILDPLATFORM <image>` (cross-compiling Go
    # stages pin themselves to the builder's arch). Taking $2 blindly read the
    # flag AS the image and failed every such stage as unpinned.
    ref=$(echo "$line" | awk '{for (i=2;i<=NF;i++) if ($i !~ /^--/) {print $i; exit}}')
    # `FROM ${VAR}` is BuildKit's global-ARG stage selector (e.g. UI_STAGE picks
    # ui-build vs ui-prebuilt). Resolve it against the Dockerfile's own
    # `ARG VAR=default` so the alias check below sees the stage name. Only the
    # default is knowable here; if it resolves to a registry image rather than a
    # stage alias, the digest requirement below still applies.
    if [[ "$ref" =~ ^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$ ]]; then
      argdef=$(grep -iE "^ARG ${BASH_REMATCH[1]}=" "$df" | head -1 | sed -E 's/^[^=]*=//' || true)
      [[ -n "$argdef" ]] && ref="$argdef"
    fi
    skip=0
    for a in "${aliases[@]:-}"; do
      if [[ -n "$a" && "$ref" == "$a" ]]; then skip=1; break; fi
    done
    ((skip)) && continue
    if [[ "$ref" != *"@sha256:"* ]]; then
      # Membership, not equality: ALLOWLIST_REFS is newline-separated, and a
      # bare == against it silently matched NOTHING once it held more than one
      # entry — every allowlisted ref would then fail the gate.
      if grep -qxF -- "$ref" <<<"$ALLOWLIST_REFS"; then continue; fi
      echo "FAIL: $df: FROM '$ref' is not digest-pinned (@sha256). Pin it: FROM $ref@sha256:<digest>." >&2
      fail=1
    fi
  done < <(grep -iE '^FROM ' "$df")
done < <(find . \( -name 'Dockerfile' -o -name 'Dockerfile.*' \) ! -path './.git/*' ! -path './ui/node_modules/*' ! -path './.claude/*')

# ── compose registry images (base AND every overlay: ci-run.sh starts the stack
#    with -f docker-compose.yaml -f docker-compose.ci.yaml) ──────────────────
for compose in ./deploy/compose/docker-compose*.yaml; do
  while IFS= read -r img; do
    # `${VAR:-default}` — the wardynd/proxy image override knobs. Judge the
    # DEFAULT, the way the Dockerfile arm above resolves `FROM ${VAR}`: only the
    # default is knowable here, and a floating registry default still fails.
    [[ "$img" =~ ^\$\{[A-Za-z_][A-Za-z0-9_]*:-(.+)\}$ ]] && img="${BASH_REMATCH[1]}"
    [[ "$img" == *:local ]] && continue        # locally-built stanza (has `build:`)
    if [[ "$img" != *"@sha256:"* ]]; then
      echo "FAIL: $compose: image '$img' is not digest-pinned (@sha256). Pin it: '$img@sha256:<digest>'." >&2
      fail=1
    fi
  done < <(grep -E '^[[:space:]]*image:' "$compose" | awk '{print $2}')
done

# ── published images carry their licence files and OCI labels ───────────────
#
# Apache-2.0 s4(a) requires a copy of the Licence to accompany the distribution
# and s4(d) requires the NOTICE contents to be carried. Publishing a container
# image IS distribution, and every image below shipped without either for three
# release generations (0.5.0, 0.6.0, 0.6.1) — the first thing any registry
# scanner reports.
#
# org.opencontainers.image.licenses must be present AND must not be a bare
# "Apache-2.0" on an image that also conveys GPL or vendor-licensed content;
# enterprise registries key off that field, so a wrong value is worse than none.
# Only the two distroless product images are legitimately Apache-2.0 alone.
# DERIVED from release.yml's matrix, never hand-listed: a second copy of "which
# images do we publish" is a second thing to forget. If the workflow is absent
# (this gate runs against a throwaway tree in test-image-pins.sh) there are no
# published images to check and the section is legitimately a no-op.
RELEASE_WF=.github/workflows/release.yml
PUBLISHED_DOCKERFILES=()
if [ -f "$RELEASE_WF" ]; then
  mapfile -t PUBLISHED_DOCKERFILES < <(grep -oE '^[[:space:]]+dockerfile: [^[:space:]]+' "$RELEASE_WF" | awk '{print $2}' | sort -u)
  if [ "${#PUBLISHED_DOCKERFILES[@]}" -eq 0 ]; then
    echo "FAIL: $RELEASE_WF exists but no 'dockerfile:' entries were found — the published-image list derivation has broken, and this gate would silently check nothing." >&2
    fail=1
  fi
fi
APACHE_ONLY_OK="deploy/compose/Dockerfile.wardynd deploy/compose/Dockerfile.proxy"

for df in "${PUBLISHED_DOCKERFILES[@]}"; do
  [ -f "$df" ] || { echo "FAIL: $df is listed as producing a published image but does not exist." >&2; fail=1; continue; }
  grep -qE '^COPY LICENSE NOTICE THIRD-PARTY-NOTICES\.md LICENSING\.md /usr/share/doc/wardyn/' "$df" \
    || { echo "FAIL: $df: no 'COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md /usr/share/doc/wardyn/'. Apache-2.0 s4(a)/(d) requires them to travel with the image." >&2; fail=1; }
  # The index alone is not the notice. MIT/BSD/ISC/OFL want the verbatim text.
  grep -qE '^COPY licenses/texts/ /usr/share/doc/wardyn/licenses/' "$df" \
    || { echo "FAIL: $df: no 'COPY licenses/texts/ /usr/share/doc/wardyn/licenses/'. A table of licence names is not the copyright and permission notice those licences require to accompany the copy." >&2; fail=1; }
  for label in title description licenses source; do
    grep -q "org.opencontainers.image.$label=" "$df" \
      || { echo "FAIL: $df: missing OCI label org.opencontainers.image.$label." >&2; fail=1; }
  done
  lic=$(grep -oE 'org\.opencontainers\.image\.licenses="[^"]*"' "$df" | head -1 | sed -E 's/.*="([^"]*)"/\1/')
  if [ "$lic" = "Apache-2.0" ] && [[ " $APACHE_ONLY_OK " != *" $df "* ]]; then
    echo "FAIL: $df: declares licenses=\"Apache-2.0\" but is not one of the distroless product images; it conveys base-image GPL and/or vendor-licensed content, so the expression must say so." >&2
    fail=1
  fi
done

# ── local-only recipes stay local ───────────────────────────────────────────
#
# claude-code/oracle/vscode/novnc/full are build recipes (`make agent-images`),
# deliberately NOT published, each for its own reason:
#   claude-code  bundles @anthropic-ai/claude-code, which is NOT open source
#                ("SEE LICENSE IN README.md", Anthropic's Commercial ToS) and
#                which Wardyn does not distribute — this is the claim
#                LICENSING.md makes to every evaluator ("No published image
#                bundles a proprietary AI coding CLI"), so it needs a gate and
#                not just a comment in release.yml's matrix.
#   vscode/full  layer ON the claude-code base, so they convey the same CLI, and
#                neither carries the licence-file COPYs the loop above requires.
#   novnc        same missing COPYs.
#   oracle       an e2e fixture that runs each task's scripted solution; it is
#                not an agent and has never been reviewed as a distributed
#                artifact.
# They would enter the matrix as agent-<name> (the Makefile's naming), so the
# prefixed form must be matched too — a bare-name pattern is vacuous.
if [ -f "$RELEASE_WF" ] && grep -qE '^[[:space:]]+- name: (agent-)?(claude-code|oracle|vscode|novnc|full)[[:space:]]*$' "$RELEASE_WF"; then
  echo "FAIL: $RELEASE_WF publishes a local-only image (claude-code/oracle/vscode/novnc/full). These are build recipes carrying vendor-licensed or unreviewed content; they must not enter the publish matrix." >&2
  fail=1
fi

# ── every published image is vulnerability-scanned ──────────────────────────
#
# These are two hand-edited lists in two workflows, so they WILL drift: before
# 0.6.2 the scan covered exactly one image, and it was the one we stopped
# publishing. An image shipped to a public registry having never been scanned is
# the failure this catches.
CI_WF=.github/workflows/ci.yml
if [ -f "$RELEASE_WF" ] && [ -f "$CI_WF" ]; then
  # INDENT-AGNOSTIC, like the `scanned` derivation beside it and the
  # `dockerfile:` one above: anchoring `published` to an exact 10-space indent
  # meant a publish job written at any other depth — a second matrix, a
  # reformat — dropped out of the comparison entirely and its images shipped
  # to a public registry never scanned, with the gate still green.
  #
  # `|| true` because a `grep` that matches nothing exits 1, and under `set -e`
  # a failing command substitution kills the script — which reads as a FAILING
  # gate but detects nothing and prints no reason. The emptiness check below is
  # what turns that into a stated failure.
  published=$(grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' "$RELEASE_WF" | awk '{print $3}' | sort -u || true)
  scanned=$(awk '/^  trivy:/{f=1} f&&/^  [a-z]/&&!/^  trivy:/{f=0} f' "$CI_WF" \
            | grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' | awk '{print $3}' | sort -u || true)
  if [ -z "$published" ]; then
    echo "FAIL: $RELEASE_WF exists but no '- name:' image entries were found — the scan-coverage cross-check's published-image list derivation has broken, and this gate would silently compare an empty list against the trivy matrix." >&2
    fail=1
  fi
  missing=$(comm -23 <(printf '%s\n' "$published") <(printf '%s\n' "$scanned"))
  if [ -n "$missing" ]; then
    echo "FAIL: image(s) published by $RELEASE_WF but never scanned in $CI_WF's trivy matrix:" >&2
    printf '  %s\n' $missing >&2
    fail=1
  fi
fi

# ── the `make setup` shell path pulls no floating third-party image ─────────
#
# scripts/up.sh puts a container ON wardyn-internal to ask wardynd questions —
# the network where WARDYN_LOCAL_TRUST_FORWARDER makes the control-plane API
# answer with NO credential — three times per `up`. It reached for a community
# Docker Hub `:latest` there, which is re-pointable by whoever holds that
# account and was re-resolved on every `make setup`, and neither section above
# can see a `docker run` (F006/F010/F152).
#
# Judged: NAMESPACED refs (`<repo>/<name>:<tag>`) in the setup path. Exempt:
#   - wardyn/*            locally BUILT by this same script (no upstream digest)
#   - ghcr.io/cjohnstoniv/*  our own release images, pinned by the RELEASE TAG
#                            this run resolved, not by a tag an outsider owns
# Bare official names (alpine:3.20 in doctor's socket probe) are NOT judged
# here: that container joins no network and mounts one directory read-only.
SETUP_PATH_SH=(./scripts/up.sh ./scripts/up-reset.sh ./scripts/lib/*.sh)
setup_refs=$(grep -ohE '[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._/-]*(@sha256:[0-9a-f]{64}|:[A-Za-z0-9._-]+)' \
             "${SETUP_PATH_SH[@]}" 2>/dev/null | sort -u || true)
while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  case "$ref" in
    wardyn/*|ghcr.io/cjohnstoniv/*) continue ;;
    *"@sha256:"*) continue ;;
  esac
  echo "FAIL: the \`make setup\` shell path runs '$ref' by TAG. Pin it by digest ('${ref%%:*}@sha256:<digest>') and run it --pull=never, so no floating third-party tag is resolved on an operator's box (F006/F010/F152)." >&2
  fail=1
done <<<"$setup_refs"

# ── every `npm install -g` in a published agent image pins an exact version ──
#
# The FROM digests above freeze the base layer and say nothing about what the
# build then installs on top of it. `npm install -g @openai/codex` resolves to
# whatever the registry serves that minute, so two builds of the same commit
# produce different agent images and no digest anywhere records which — the same
# floating-upstream problem the FROM rule exists for, one layer up (F165).
#
# A SCOPED package carries its own leading `@` (`@openai/codex`), so the version
# marker is an `@` after the FIRST character. `npm@${NPM_VERSION}` is the npm
# self-upgrade and is pinned by its own ARG.
while IFS= read -r spec; do
  [ -n "$spec" ] || continue
  case "$spec" in npm@*) continue ;; esac
  # strip a leading scope `@`, then require an `@` in what remains
  case "${spec#@}" in
    *@*) continue ;;
    *) echo "FAIL: deploy/images: 'npm install -g $spec' pins no version. Two builds of the same commit install different code and nothing records which; pin it (e.g. via an ARG, as CLAUDE_CODE_VERSION / CODEX_VERSION do) — F165." >&2
       fail=1 ;;
  esac
done < <(grep -h -v '^[[:space:]]*#' ./deploy/images/*/Dockerfile 2>/dev/null \
         | grep -oE 'npm install -g +"?[^"$ ;\\]+' | sed -E 's/npm install -g +"?//' | sort -u || true)

# ── the desktop enrolment image says out loud that it floats ────────────────
#
# The setup-path section below exempts `ghcr.io/cjohnstoniv/*` because those
# refs are pinned by the RELEASE TAG the run resolved. That reasoning does NOT
# carry to deploy/desktop/install.sh, whose default enrolment image is the
# CONTINUOUS `:latest` publish-image.yml pushes on every merge to main and never
# cosign-signs — and it runs AS ROOT to mint the device's age identity
# (F113/F184). Hard-failing on the default itself would need a release digest,
# which is a network value this gate cannot resolve; what IS checkable offline is
# that the installer still tells the person running it.
DESKTOP_INSTALL=./deploy/desktop/install.sh
if [ -f "$DESKTOP_INSTALL" ]; then
  grep -qF 'is a MUTABLE tag, not a digest' "$DESKTOP_INSTALL" \
    || { echo "FAIL: $DESKTOP_INSTALL no longer warns that a non-digest WARDYN_INSTALL_IMAGE floats. Its default is the unsigned continuous :latest, run AS ROOT to mint the device's age identity, and no pin gate can catch a \`docker run\` in a shell script — the warning is the only control there is (F113/F184)." >&2; fail=1; }
fi

if ((fail)); then
  exit 1
fi
echo "check-image-pins: OK (digest pins; published images carry their licences + OCI labels; every published image is scanned)"
