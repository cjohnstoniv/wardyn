#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-doc-form.sh — doc-form.sh still SEES every shape of prose it claims to
# cap, and still passes the pages it is meant to pass, in a throwaway tree.
#
# Why this exists: the gate's caps (paragraph, list item, table cell, quote)
# and its --budget mode live in an embedded Python parser with no test of its
# own. A parser change that stops counting continuation lines, or quote lines,
# leaves every real page green while the gate catches nothing — a gate that
# stops detecting is worse than no gate, so this pins BOTH directions: shapes
# that must FAIL (with the message that names them) and shapes that must stay
# OK. It also pins the legacy/strict split: the ten original pages keep their
# original checks until a scripts/doc-form.d/*.list file names them.
#
# Daemon-free, network-free: it runs the real gate against a throwaway tree.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts/doc-form.d" "$TMP/docs/operations" "$TMP/docs/scratch"
cp "$ROOT/scripts/doc-form.sh" "$TMP/scripts/"
cp "$ROOT"/docs/operations/*.md "$TMP/docs/operations/"

fail() { echo "FAIL: $*" >&2; exit 1; }

# The gate resolves its own repo root from its location, so running the COPY
# scans $TMP and nothing else.
gate() { (cd "$TMP" && ./scripts/doc-form.sh "$@" 2>&1); }
# run_gate — status only; gate_says — output regardless of status. Failing
# cases assert on the MESSAGE, not just the status: a crash would satisfy a
# bare non-zero assertion while detecting nothing.
run_gate() { gate "$@" >/dev/null; }
gate_says() { gate "$@" || true; }

# words N — N words in sentences of ten, so the sentence cap never masks the
# cap under test.
words() { awk -v n="$1" 'BEGIN { for (i = 1; i <= n; i++) printf "w%s%s", i, (i % 10 == 0 ? ". " : " "); }'; }

# scratch NAME — name docs/scratch/NAME.md in a lane list file, which puts it
# on every cap. The caller writes the file body.
LIST="$TMP/scripts/doc-form.d/t.list"
scratch() { echo "docs/scratch/$1.md" > "$LIST"; }

# page — an H1, a one-line summary, an H2, then stdin.
page() { printf '# Scratch\n\nA short summary line.\n\n## Section\n\n'; cat; }

check_fails() { # description, expected message fragment
  local out
  if run_gate; then fail "$1 must FAIL"; fi
  out="$(gate_says)"
  grep -qF -- "$2" <<<"$out" || fail "$1 failed without naming it ($2): $out"
  echo "ok  $1 fails"
}

# 1. The real ten pages pass untouched (the legacy tier).
run_gate || fail "the copied docs/operations pages must PASS"
echo "ok  the ten original pages pass"

# 2. A compliant page, strict: bullets, a table, a short alert.
scratch good
page > "$TMP/docs/scratch/good.md" <<DOC
- $(words 20)
- $(words 30)

| Option | Meaning |
| --- | --- |
| \`a\` | $(words 20) |

> [!NOTE]
> $(words 20)
DOC
run_gate || fail "a compliant strict page must PASS: $(gate_says)"
echo "ok  a compliant strict page passes"

# 3. Each cap, failing and named.
scratch para
page > "$TMP/docs/scratch/para.md" <<DOC
$(words 90)
DOC
check_fails "a 90-word paragraph" "90-word paragraph (max 80)"

scratch item
page > "$TMP/docs/scratch/item.md" <<DOC
- $(words 30)
  $(words 40)
DOC
check_fails "a 70-word list item with a continuation line" "70-word list item (max 60)"

scratch bigitem
page > "$TMP/docs/scratch/bigitem.md" <<DOC
- $(words 40)
  $(words 40)
  $(words 30)
  $(words 30)
DOC
check_fails "a 140-word bullet with continuation lines" "140-word list item (max 60)"

scratch cell
page > "$TMP/docs/scratch/cell.md" <<DOC
| Option | Meaning |
| --- | --- |
| \`a\` | $(words 45) |
DOC
check_fails "a 45-word table cell" "45-word table cell (max 40)"

scratch quote
page > "$TMP/docs/scratch/quote.md" <<DOC
> $(words 50)
> $(words 50)
DOC
check_fails "a 100-word quote" "100-word paragraph (in a quote) (max 80)"

scratch quoteitems
page > "$TMP/docs/scratch/quoteitems.md" <<DOC
> 1. $(words 70)
> 2. short.
DOC
check_fails "a 70-word list item inside a quote" "70-word list item (in a quote) (max 60)"

# 4. Quote lines count as prose for the share: a page of short quote lines is
#    mostly prose and fails the share cap although every block is under its
#    word cap.
scratch quoteshare
{
  printf -- '- one.\n\n'
  for _ in 1 2 3 4 5 6 7 8; do echo "> short quoted line number."; done
} | page > "$TMP/docs/scratch/quoteshare.md"
check_fails "a page of short quote lines" "paragraph share"

# 5. An alert's marker line and bare '>' spacers are markup, not prose.
scratch alert
page > "$TMP/docs/scratch/alert.md" <<DOC
- one.
- two.
- three.
- four.

> [!WARNING]
>
> A short warning body.
DOC
run_gate || fail "an alert marker and spacer must not count as prose: $(gate_says)"
echo "ok  an alert's marker line and spacer are exempt"

# 6. The legacy tier keeps its original checks only: the same 90-word
#    paragraph passes in a page that is still on the ASSERTED_DOCS list, and
#    fails the moment a lane list names the page.
rm -f "$LIST"
printf '\n%s\n' "$(words 90)" >> "$TMP/docs/operations/launch-presets.md"
run_gate || fail "a legacy page keeps only its original checks: $(gate_says)"
echo "ok  a legacy page is not held to the new caps"
echo "docs/operations/launch-presets.md" > "$LIST"
check_fails "the same page once a lane lists it" "90-word paragraph (max 80)"
rm -f "$LIST"
cp "$ROOT/docs/operations/launch-presets.md" "$TMP/docs/operations/launch-presets.md"

# 7. Budget: prose words = words outside fences, minus table pipes, separator
#    rows, quote markers, list markers and heading hashes. This page is 11:
#    Scratch(1) + 3 + Section(1) + 3 + x(1) + y z(2) = 11; the fenced line
#    must not count.
cat > "$TMP/docs/scratch/budget.md" <<'DOC'
# Scratch

Summary line here.

## Section

- one two three

| x | y z |
| --- | --- |

```
fenced words never count
```
DOC
run_gate --budget docs/scratch/budget.md=11 ||
  fail "a doc AT its budget must PASS: $(gate_says --budget docs/scratch/budget.md=11)"
echo "ok  a doc at its budget passes"
out="$(gate_says --budget docs/scratch/budget.md=10)"
grep -qF "11 prose words (budget 10, over by 1)" <<<"$out" ||
  fail "a doc one word over its budget must FAIL and say so: $out"
if run_gate --budget docs/scratch/budget.md=10; then
  fail "a doc one word over its budget must exit non-zero"
fi
echo "ok  a doc that grew past its budget fails"

# 8. A budget file works the same; a repeated flag checks every entry; a
#    missing doc or a malformed spec is refused.
echo "docs/scratch/budget.md=10  # grew by one" > "$TMP/scripts/doc-form.d/t.budget"
if run_gate; then fail "a *.budget line over budget must FAIL"; fi
echo "docs/scratch/budget.md=11" > "$TMP/scripts/doc-form.d/t.budget"
run_gate || fail "a *.budget line at the budget must PASS"
rm -f "$TMP/scripts/doc-form.d/t.budget"
if run_gate --budget docs/scratch/budget.md=11 --budget docs/scratch/budget.md=3; then
  fail "a repeated --budget must check every entry"
fi
if run_gate --budget docs/scratch/missing.md=5; then fail "a budget on a missing doc must FAIL"; fi
if run_gate --budget not-a-budget; then fail "a malformed --budget must be refused"; fi
echo "ok  budget files, repeated flags, missing docs and bad specs"

echo "doc-form tests: PASS"
