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

scratch lazy
page > "$TMP/docs/scratch/lazy.md" <<DOC
- $(words 50)
$(words 50)
DOC
check_fails "a 100-word list item with an unindented (lazy) continuation line" "100-word list item (max 60)"

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

# 6b. The legacy tier re-implements the old rules; it does not run the old
#     parser. Pinned quote-adjacent shape: a 35-word sentence under an alert
#     marker passes (the old parser counted "[!NOTE]" as a 36th word, a false
#     positive), and a real 36-word sentence in the same quote still fails.
sentence() { awk -v n="$1" 'BEGIN { for (i = 1; i <= n; i++) printf "w%s%s", i, (i == n ? "." : " "); }'; }
printf '\n> [!NOTE]\n> %s\n' "$(sentence 35)" >> "$TMP/docs/operations/launch-presets.md"
run_gate || fail "an alert marker must not count as a word of the legacy sentence pass: $(gate_says)"
echo "ok  a legacy page: the alert marker is not a word of its sentence"
printf '> %s\n' "$(sentence 36)" >> "$TMP/docs/operations/launch-presets.md"
check_fails "a legacy page with a 36-word sentence in a quote" "36-word sentence (max 35)"
cp "$ROOT/docs/operations/launch-presets.md" "$TMP/docs/operations/launch-presets.md"
#     A quoted sentence wrapped onto a line that starts with "#141)" is still
#     one sentence: '#' without a space is not a heading inside a quote.
printf '\n> %s\n> #141) %s\n' "$(words 20 | tr -d .)" "$(sentence 15)" >> "$TMP/docs/operations/launch-presets.md"
check_fails "a legacy page with a 36-word quoted sentence wrapped at '#141)'" "36-word sentence (max 35)"
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
if run_gate --budget 'docs/scratch/budget.md=11 share=x'; then fail "a malformed share= must be refused"; fi
echo "ok  budget files, repeated flags, missing docs and bad specs"

# 9. share=S gates the STRICT share. This page has 5 counted lines, 2 of
#    them prose (the summary and the quote line): strict share 40.0%. The
#    legacy measure would say 20% (it does not count quote lines), so a gate
#    on the wrong measure passes share=39.
cat > "$TMP/docs/scratch/share.md" <<'DOC'
# Scratch

Summary line here.

## Section

- one

> quoted.
DOC
run_gate --budget 'docs/scratch/share.md=7 share=40' ||
  fail "a doc AT its share budget must PASS: $(gate_says --budget 'docs/scratch/share.md=7 share=40')"
echo "ok  a doc at its share budget passes"
out="$(gate_says --budget 'docs/scratch/share.md=7 share=39')"
grep -qF "strict paragraph share 40.0% (budget share 39%)" <<<"$out" ||
  fail "a doc over its share budget must FAIL and say so: $out"
if run_gate --budget 'docs/scratch/share.md=7 share=39'; then
  fail "a doc over its share budget must exit non-zero"
fi
echo "ok  a doc over its share budget fails (strict share, not legacy)"
printf 'docs/scratch/share.md=7\tshare=39   # grew\n' > "$TMP/scripts/doc-form.d/t.budget"
out="$(gate_says)"
grep -qF "strict paragraph share 40.0% (budget share 39%)" <<<"$out" ||
  fail "a *.budget line 'path=N share=S' over its share must FAIL and say so: $out"
echo "docs/scratch/share.md=7 share=40" > "$TMP/scripts/doc-form.d/t.budget"
run_gate || fail "a *.budget line 'path=N share=S' at its share must PASS: $(gate_says)"
rm -f "$TMP/scripts/doc-form.d/t.budget"
echo "ok  a *.budget line 'path=N share=S' is read and gated"

# 10. Waivers: scripts/doc-form.d/*.waive keeps a named over-cap item visible
#     (counted, "(M waived)") instead of failing on it. Sentence waivers take
#     36-40 words and no clause joiner; table waivers take a PLAN § reason;
#     a waiver that matches nothing fails. sha1 is of the folded sentence.
WAIVE="$TMP/scripts/doc-form.d/t.waive"
sha() { printf '%s' "$1" | sha1sum | cut -d' ' -f1; }
waive_fails() { # description, expected message fragment
  local out
  if run_gate; then fail "$1 must FAIL"; fi
  out="$(gate_says)"
  grep -qF -- "$2" <<<"$out" || fail "$1 failed without naming it ($2): $out"
  echo "ok  $1 fails"
}

S38="$(sentence 38)"
scratch waived
page > "$TMP/docs/scratch/waived.md" <<DOC
$S38
DOC
rm -f "$WAIVE"
out="$(gate_says)"
grep -qF "38-word sentence (max 35)" <<<"$out" || fail "an unwaived 38-word sentence must FAIL: $out"
grep -qF "$(printf 'waivable as: sentence\tdocs/scratch/waived.md\t%s\t38' "$(sha "$S38")")" <<<"$out" ||
  fail "a waivable failing sentence must print the waiver line to paste: $out"
echo "ok  a doc with no waiver file fails as before and prints the line to paste"

printf '# lane waivers\n\nsentence\tdocs/scratch/waived.md\t%s\t38\tone clause, kept whole\n' "$(sha "$S38")" > "$WAIVE"
run_gate || fail "a waived 38-word sentence with no joiner must PASS: $(gate_says)"
grep -qF "long_sentences: 1 (1 waived)" <<<"$(gate_says)" || fail "the waived sentence must still be counted: $(gate_says)"
echo "ok  a 38-word sentence with no joiner, waived, passes and is counted"

SJ="$(sentence 38 | sed 's/w19 /w19; /')"
page > "$TMP/docs/scratch/waived.md" <<DOC
$SJ
DOC
printf 'sentence\tdocs/scratch/waived.md\t%s\t38\treason\n' "$(sha "$SJ")" > "$WAIVE"
waive_fails "the same waiver on a 38-word sentence with a joiner" 'contains the clause joiner ";"'

S45="$(sentence 45)"
page > "$TMP/docs/scratch/waived.md" <<DOC
$S45
DOC
printf 'sentence\tdocs/scratch/waived.md\t%s\t45\treason\n' "$(sha "$S45")" > "$WAIVE"
waive_fails "a waiver on a 45-word sentence" "only 36-40-word sentences can be waived"
printf 'sentence\tdocs/scratch/waived.md\t%s\t38\treason\n' "$(sha "$S45")" > "$WAIVE"
waive_fails "a 45-word sentence waived as 38 words" "declares 38 words but the sentence has 45"

page > "$TMP/docs/scratch/waived.md" <<DOC
$S38
DOC
printf 'sentence\tdocs/scratch/waived.md\t%s\t38\treason\n' "$(sha "$S38 changed")" > "$WAIVE"
waive_fails "a sentence waiver that matches nothing" "matches no over-cap sentence"
printf 'sentence\tdocs/scratch/waived.md\tnot-a-hash\t38\treason\n' > "$WAIVE"
waive_fails "a sentence waiver with a malformed sha1" "is not 40 lowercase hex digits"
printf 'sentence\tdocs/scratch/waived.md\t%s\t38\n' "$(sha "$S38")" > "$WAIVE"
waive_fails "a sentence waiver with no reason" "want sentence<TAB>path<TAB>sha1<TAB>words<TAB>reason"

# A joiner written out in the prose is refused; one that only appears once code
# spans are removed (", `0-9` and `-`") is not. Both sentences are 38 words.
SA="$(sentence 37 | sed 's/w19 /w19, and /')"
page > "$TMP/docs/scratch/waived.md" <<DOC
$SA
DOC
printf 'sentence\tdocs/scratch/waived.md\t%s\t38\treason\n' "$(sha "$SA")" > "$WAIVE"
waive_fails "a 38-word sentence with a written-out ', and '" 'contains the clause joiner ", and"'
SC="$(sentence 37 | sed 's/w19 /w19, `0-9` and `-` /')"
page > "$TMP/docs/scratch/waived.md" <<DOC
$SC
DOC
rm -f "$WAIVE"
out="$(gate_says)"
grep -qF "waivable as: sentence" <<<"$out" || fail "code spans around 'and' are not a joiner; the paste line must print: $out"
printf 'sentence\tdocs/scratch/waived.md\t%s\t38\treason\n' "$(sha "$(sentence 37 | sed 's/w19 /w19, and /')")" > "$WAIVE"
run_gate || fail "a sentence whose ', and' is only code spans around 'and' must be waivable: $(gate_says)"
echo "ok  code spans around 'and' after a comma are not a clause joiner"

scratch tbl
page > "$TMP/docs/scratch/tbl.md" <<DOC
| Option | Meaning |
| --- | --- |
| \`a\` | $(words 45) |
| \`b\` | $(words 50) |

| Other | Meaning |
| --- | --- |
| \`c\` | short |
DOC
printf 'table\tdocs/scratch/tbl.md\t| Option | Meaning |\tkept as a table, PLAN §4.2\n' > "$WAIVE"
run_gate || fail "a table waiver with a PLAN § reason must PASS: $(gate_says)"
grep -qF "long_blocks: 2 (2 waived)" <<<"$(gate_says)" || fail "both over-cap cells must be counted and waived: $(gate_says)"
echo "ok  a table waiver with a PLAN § reason passes its over-cap cells"
printf 'table\tdocs/scratch/tbl.md\t| Option | Meaning |\tkept as a table\n' > "$WAIVE"
waive_fails "a table waiver without PLAN §" 'must cite a plan section ("PLAN §")'
printf 'table\tdocs/scratch/tbl.md\t| Nope | Meaning |\tPLAN §4.2\n' > "$WAIVE"
waive_fails "a table waiver whose header matches no table" "has this header line"
printf 'table\tdocs/scratch/tbl.md\t| Other | Meaning |\tPLAN §4.2\n' > "$WAIVE"
waive_fails "a table waiver for a table with no over-cap cell" "no cell of this table is over the cap"
rm -f "$WAIVE"

# Front matter: a YAML block on line 1 is metadata. An over-cap description:
# line there passes; the same line as the first paragraph of a body fails.
LONGLINE="description: $(words 90)"
scratch front
{ printf -- '---\nname: scratch\n%s\n---\n' "$LONGLINE"; page <<DOC
- $(words 20)
DOC
} > "$TMP/docs/scratch/front.md"
run_gate || fail "a page whose over-cap line is in leading front matter must PASS: $(gate_says)"
echo "ok  an over-cap line in leading front matter passes"
page > "$TMP/docs/scratch/front.md" <<DOC
$LONGLINE
DOC
check_fails "the same long line as a body paragraph" "word paragraph (max 80)"
{ page <<DOC
- $(words 20)
DOC
printf -- '\n---\nname: scratch\n%s\n---\n' "$LONGLINE"; } > "$TMP/docs/scratch/front.md"
check_fails "a --- block that does not start on line 1" "word paragraph (max 80)"

echo "doc-form tests: PASS"
