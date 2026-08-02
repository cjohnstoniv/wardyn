#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Missing-test INVENTORY. Turns the unioned coverage profile (cover-union.sh's
# coverage-func.txt) into a TRIAGED list of the exported functions that show
# 0.0% coverage in the number the per-PR CI job enforces, split into:
#
#   PG-gated       — 0.0% in the union but COVERED by the Postgres lane
#                    (`make test-report-pg`, the `ci test-pg` job). PROVEN, not
#                    guessed: the func is >0% in test/reports/go/pg. The union
#                    can't set WARDYN_TEST_PG, so these land uncovered there by
#                    construction — they are exercised, just not on every PR.
#   Docker-gated   — in a package whose real coverage needs a live Docker daemon
#                    (WARDYN_TEST_DOCKER=1); the fakeDocker tests self-skip the
#                    real-daemon funcs, so they read 0.0% everywhere the daemon
#                    is absent. Classified by package (no daemon here to prove it).
#   Untested       — genuinely no test reaches it (including PG-package funcs the
#                    PG lane ALSO leaves at 0.0%).
#
# This is an INVENTORY, not a promise or a TODO list: an exported func here is
# not automatically a bug — a thin pass-through, a driver only real hardware
# exercises, or a helper covered indirectly can legitimately read 0.0%. It exists
# so the "genuinely untested exported surface" is a number someone can watch,
# distinct from the service-gated code the per-PR run structurally can't touch.
#
# Only EXPORTED funcs (leading uppercase) are inventoried — the package's public
# surface. Unexported helpers are implementation detail and are not listed.
#
# Usage:
#   scripts/test-gaps.sh [union-func.txt] [pg-func.txt] [out.md]
#   scripts/test-gaps.sh --self-test
# Defaults:
#   union: test/reports/go/union/coverage-func.txt   (written by cover-union.sh)
#   pg:    test/reports/go/pg/coverage-func.txt       (written by `make test-report-pg`)
#   out:   docs/TEST-GAPS.md
#
# Refresh:
#   make cover-check        # regenerate the union profile (both shipped builds)
#   make test-report-pg     # optional (needs WARDYN_TEST_PG): refresh the PG cross-check
#   ./scripts/test-gaps.sh  # regenerate docs/TEST-GAPS.md
set -euo pipefail

MODULE="github.com/cjohnstoniv/wardyn/"
# Packages whose real coverage requires a live Docker daemon (WARDYN_TEST_DOCKER).
DOCKER_RE='^(internal/runner/docker|internal/envbuild|cmd/wardyn-runner)(/|$)'

# classify emits: <category>\t<pkg>\t<func>\t<file:line>
# args: <union-func.txt> <pg-func.txt-or-empty>
classify() {
  local union="$1" pg="${2:-}"
  awk -v mod="$MODULE" -v dockerre="$DOCKER_RE" '
    # Phase 1: keys (file<TAB>func) COVERED (>0%) by the PG lane.
    FNR==NR {
      if ($2 ~ /^[A-Z]/ && $3 != "0.0%") {
        f=$1; sub(mod,"",f); sub(/:[0-9]+:$/,"",f); pgcov[f "\t" $2]=1
      }
      next
    }
    # Phase 2: the union profile — inventory exported funcs at 0.0%.
    $2 ~ /^[A-Z]/ && $3=="0.0%" {
      loc=$1; sub(mod,"",loc); sub(/:$/,"",loc)     # relpath:line
      file=loc; sub(/:[0-9]+$/,"",file)             # relpath
      pkg=file; sub(/\/[^\/]+$/,"",pkg)             # dir
      key=file "\t" $2
      if (key in pgcov)          cat="PG"
      else if (pkg ~ dockerre)   cat="DOCKER"
      else                       cat="UNTESTED"
      print cat "\t" pkg "\t" $2 "\t" loc
    }
  ' "$pg" "$union"
  # Note: passing an empty/absent pg file to awk is fine — it reads zero records,
  # so every func falls through to DOCKER/UNTESTED (honest downgrade, see header).
}

self_test() {
  local d; d="$(mktemp -d)"; trap 'rm -rf "$d"' RETURN
  # Union: three exported 0.0% funcs across a PG pkg, a docker pkg, and a plain pkg,
  # plus a covered func and an unexported one (both must be excluded).
  printf '%sinternal/store/store.go:43:\tCreateRun\t0.0%%\n' "$MODULE"  > "$d/u"
  printf '%sinternal/store/store.go:84:\tUpdateRunState\t0.0%%\n' "$MODULE" >> "$d/u"
  printf '%sinternal/runner/docker/driver.go:10:\tCreateSandbox\t0.0%%\n' "$MODULE" >> "$d/u"
  printf '%sinternal/api/runs.go:9:\tComposeRun\t0.0%%\n' "$MODULE" >> "$d/u"
  printf '%sinternal/api/runs.go:9:\thelperFn\t0.0%%\n' "$MODULE" >> "$d/u"
  printf '%sinternal/api/runs.go:9:\tAlreadyCovered\t80.0%%\n' "$MODULE" >> "$d/u"
  # PG lane covers CreateRun (=> PG-gated) but NOT UpdateRunState (=> untested).
  printf '%sinternal/store/store.go:41:\tCreateRun\t100.0%%\n' "$MODULE"  > "$d/pg"
  printf '%sinternal/store/store.go:80:\tUpdateRunState\t0.0%%\n' "$MODULE" >> "$d/pg"

  local got want
  got="$(classify "$d/u" "$d/pg" | LC_ALL=C sort)"
  want="$(printf '%s\n' \
    "DOCKER	internal/runner/docker	CreateSandbox	internal/runner/docker/driver.go:10" \
    "PG	internal/store	CreateRun	internal/store/store.go:43" \
    "UNTESTED	internal/api	ComposeRun	internal/api/runs.go:9" \
    "UNTESTED	internal/store	UpdateRunState	internal/store/store.go:84" | LC_ALL=C sort)"
  if [ "$got" != "$want" ]; then
    echo "test-gaps: self-test FAIL" >&2
    diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") >&2 || true
    exit 1
  fi
  echo "test-gaps: self-test PASS"
}

if [ "${1:-}" = "--self-test" ]; then self_test; exit 0; fi

UNION="${1:-test/reports/go/union/coverage-func.txt}"
PG="${2:-test/reports/go/pg/coverage-func.txt}"
OUT="${3:-docs/TEST-GAPS.md}"

if [ ! -f "$UNION" ]; then
  echo "test-gaps: union coverage profile not found: $UNION" >&2
  echo "  run: make cover-check   (writes test/reports/go/union/coverage-func.txt)" >&2
  exit 2
fi
PG_ARG="$PG"; PG_NOTE="cross-checked against the Postgres lane (\`$PG\`)"
if [ ! -f "$PG" ]; then
  PG_ARG=""; PG_NOTE="**PG lane profile absent** — PG-gated funcs could not be proven and fall into \"Untested\" (run \`make test-report-pg\` to separate them)"
fi

ROWS="$(classify "$UNION" "$PG_ARG")"
UNION_TOTAL="$(awk '/^total:/ {print $NF}' "$UNION")"
EXPORTED_TOTAL="$(awk '$2 ~ /^[A-Z]/ && $1 ~ /\.go:[0-9]+:$/ {n++} END{print n+0}' "$UNION")"
n_pg="$(printf '%s\n' "$ROWS"   | grep -c '^PG'       || true)"
n_dk="$(printf '%s\n' "$ROWS"   | grep -c '^DOCKER'   || true)"
n_ut="$(printf '%s\n' "$ROWS"   | grep -c '^UNTESTED' || true)"
n_gap=$((n_pg + n_dk + n_ut))

# Render one "- **pkg** (N): a, b, c" line per package for a category.
render_cat() {
  local cat="$1"
  printf '%s\n' "$ROWS" | awk -F'\t' -v c="$cat" '$1==c {print $2"\t"$3}' \
    | LC_ALL=C sort | awk -F'\t' '
      { if ($1!=p) { if(p!="") printf "\n"; printf "- **%s**: %s", $1, $2; p=$1; next }
        printf ", %s", $2 }
      END { if(p!="") printf "\n" }'
}

{
  echo "<!-- GENERATED by scripts/test-gaps.sh — DO NOT EDIT BY HAND. -->"
  echo "# Test gaps — untested exported surface (inventory)"
  echo
  echo "_Generated $(date -u +%Y-%m-%d) by \`scripts/test-gaps.sh\` from \`$UNION\`"
  echo "(union coverage total **${UNION_TOTAL}**), ${PG_NOTE}._"
  echo
  echo "This is an **inventory, not a promise**. An exported func listed here is not"
  echo "automatically a bug: a thin pass-through, a driver only a live daemon exercises,"
  echo "or a helper covered indirectly can legitimately read 0.0%. The list exists so the"
  echo "*genuinely untested* public surface stays a watched number, kept separate from the"
  echo "service-gated code a per-PR run structurally cannot reach. Only exported funcs are"
  echo "counted (the package's public surface); unexported helpers are omitted."
  echo
  echo "Refresh: \`make cover-check\` (+ \`make test-report-pg\` for the PG cross-check), then \`./scripts/test-gaps.sh\`."
  echo
  echo "## Summary"
  echo
  echo "| Bucket | Exported funcs at 0.0% in the union |"
  echo "|---|---:|"
  echo "| **PG-gated** (proven covered by \`ci test-pg\`) | ${n_pg} |"
  echo "| **Docker-gated** (needs \`WARDYN_TEST_DOCKER=1\`) | ${n_dk} |"
  echo "| **Untested** (no test reaches it) | ${n_ut} |"
  echo "| Total 0.0% exported | ${n_gap} |"
  echo "| _(of ${EXPORTED_TOTAL} exported funcs in the union)_ | |"
  echo
  echo "## Untested — genuinely no test reaches these"
  echo
  echo "The real backlog: exported funcs no lane covers. PG-package funcs appear here"
  echo "only when the Postgres lane ALSO leaves them at 0.0%."
  echo
  render_cat UNTESTED
  echo
  echo "## PG-gated — covered by the Postgres lane, not by the per-PR run"
  echo
  echo "0.0% in the enforced union (which can't set WARDYN_TEST_PG) but >0% in"
  echo "\`test/reports/go/pg\`. Exercised on the \`ci test-pg\` job / \`make test-report-pg\`."
  echo
  render_cat PG
  echo
  echo "## Docker-gated — need a live daemon (WARDYN_TEST_DOCKER=1)"
  echo
  echo "Real-daemon driver funcs; the fakeDocker tests self-skip them, so they read"
  echo "0.0% wherever no daemon is present. Classified by package."
  echo
  render_cat DOCKER
} > "$OUT"

echo "test-gaps: wrote $OUT — untested=${n_ut} pg-gated=${n_pg} docker-gated=${n_dk} (of ${EXPORTED_TOTAL} exported)"
