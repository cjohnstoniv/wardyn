#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# doc-form.sh — the six-point form rule from #201: every runbook opens with a
# short summary, options/knobs/roles/states are tables, procedures are
# numbered steps, and paragraph share stays bounded (prose isn't allowed to
# creep back in one sentence at a time). Companion to check-diagrams.sh, which
# gates the mermaid half of the same rule; this gates the text half.
#
# MEASURES every doc in the #201 table (prints the same report the issue
# opened with, so the whole estate's drift stays visible), but only ASSERTS
# (fails the gate) on the docs actually brought into the six-point form so
# far: the named ASSERTED_DOCS below plus every path in scripts/doc-form.d/
# *.list (one path per line, one file per lane, merged in; '#' comments and
# blank lines ignored). The rest (OPERATIONS.md's own guarded sections,
# THREAT-MODEL.md, DESKTOP.md, SSH.md, RELEASING.md, the Helm README) are a
# separate, later pass — asserting on unconverted docs here would just turn
# the gate red for work not yet done. ARCHITECTURE.md is absent even from the
# measured list: the issue says it "keeps its 3" diagrams as-is.
#
# paragraph share = prose lines / all non-blank, non-fenced lines. Fenced code
# is excluded entirely (neither side of the ratio) since it isn't prose. A line
# is prose when it is
#   - a plain paragraph line; or
#   - a blockquote line, EXCEPT the marker line of a GitHub alert
#     ("> [!NOTE]", TIP, IMPORTANT, WARNING, CAUTION) and bare ">" spacers; or
#   - a line of a list item (marker line plus its indented continuation
#     lines) whose words exceed MAX_ITEM_WORDS.
# Bullets, numbered steps and table rows under the item cap are not prose
# (rule 4 wants content there).
#
# Why blockquotes and long items count as prose: an earlier version exempted
# every blockquote and every list line, which let a 700-word paragraph-
# in-disguise sit in a "> **...**" quote (docs/operations/member-mode.md, the
# "Four ceilings" block) or in a bullet with a dozen continuation lines and
# still score as "not paragraph". The share rule measured markup, not prose.
# Quoting a wall of text, or writing it as one giant bullet, no longer
# escapes the rule; a short alert body still passes.
#
# Per-block caps (every one fails the gate, listed as long_blocks):
#   MAX_PARAGRAPH_WORDS  one paragraph block (consecutive prose lines; a
#                        blockquote paragraph counts the same way)
#   MAX_ITEM_WORDS       one list item with its continuation lines, indented
#                        or lazy (an unindented line directly after the item
#                        renders inside it, so it counts; for the share it
#                        stays a paragraph line)
#   MAX_CELL_WORDS       one table cell
# plus MAX_SENTENCE_WORDS for any sentence, wherever it sits. Table cells and
# blockquote text still count against the sentence cap — a 39-word sentence
# hiding in a table cell is exactly the prose creep rule 5 is for.
#
# Known gap: text inside a code fence is outside every measure (caps, share,
# budget). Fenced prose is the one way left to hide words from this gate; the
# fact ledger (docs-overhaul tools/doc-facts.py) does not count fence text as
# rendered prose either, so a rewrite that fences a sentence cannot claim it
# as kept there.
#
# LEGACY TIER: the ten ASSERTED_DOCS that no *.list names keep the checks of
# the gate before the caps (sentence, the old paragraph share, summary), but
# through this parser, not the old one: it re-implements the old rules. On all
# 154 tracked .md files the old share and summary are identical; the sentence
# sets are identical on 151. The differences are all old false positives,
# where the old parser counted markup as words or joined blocks that render
# separately: a "[!IMPORTANT]" alert marker read as a word, a "-" marker of a
# list inside a quote read as a word, and quoted table rows joined into one
# sentence (member-mode.md, OPERATIONS.md, TRY-IT.md). The first review of
# this parser found 7 differing documents: three went away once lazy
# continuation lines join their item again (as the old parser did), and one
# (the Helm README) was this parser dropping a quote line that starts with
# "#141)" as a heading; inside a quote only "#" plus a space is a heading now.
#
# BUDGET (grow-fails): "--budget '<doc>=<N>[ share=<S>]'" (repeatable) and
# lines "path=N share=S" in scripts/doc-form.d/*.budget ("share=S" optional)
# fail when a doc's PROSE words exceed N, or when its STRICT paragraph share
# (the measure above, on any tier) exceeds S percent. Prose
# words = whitespace-separated words outside code fences, excluding table
# pipes, table separator rows, blockquote markers and list markers. wc -w is
# not the measure: converting prose to tables adds markup words, so wc -w can
# rise while the text a reader must read falls. A lane sets N to its
# before-count minus one, so a rewrite whose prose grew fails.
#
set -euo pipefail
cd "$(dirname "$0")/.."

MEASURED_ONLY=(
  docs/OPERATIONS.md
  threatmodel/THREAT-MODEL.md
  deploy/helm/wardyn/README.md
  ROADMAP.md
  docs/DESKTOP.md
  docs/SSH.md
  RELEASING.md
  docs/ENV.md
  docs/AUDIT-ACTIONS.md
)
# Named, not a glob: a lane adding a new page under docs/operations/ should
# not find itself gated by this list until it is deliberately added here.
ASSERTED_DOCS=(
  docs/operations/build-images.md
  docs/operations/console-branding.md
  docs/operations/hybrid-laptops.md
  docs/operations/integrations.md
  docs/operations/kubernetes-known-gaps.md
  docs/operations/launch-presets.md
  docs/operations/member-mode.md
  docs/operations/monitoring.md
  docs/operations/run-lifetime.md
  docs/operations/secrets-and-keys.md
)
MAX_PARAGRAPH_SHARE=50
MAX_SENTENCE_WORDS=35
MAX_PARAGRAPH_WORDS=80
MAX_ITEM_WORDS=60
MAX_CELL_WORDS=40

# Per-lane membership: scripts/doc-form.d/*.list names the docs a lane has
# brought into the NEW form; they are asserted against every cap above. The
# ten ASSERTED_DOCS keep exactly the checks they had before the caps existed
# (sentence length, the original paragraph share, summary) until a lane lists
# them — a listed doc moves from ASSERTED_DOCS to the strict set. The report
# prints each legacy doc's strict numbers so the remaining debt stays visible.
# scripts/doc-form.d/*.budget ("path=N share=S") adds budgets. Both file
# kinds skip blank lines and '#' comments. nullglob keeps an empty directory
# from yielding a literal '*.list'.
shopt -s nullglob
STRICT_DOCS=()
for f in scripts/doc-form.d/*.list; do
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    line="${line//[[:space:]]/}"
    [[ -z "$line" ]] && continue
    already=0
    for d in "${STRICT_DOCS[@]+"${STRICT_DOCS[@]}"}"; do [[ "$d" == "$line" ]] && already=1; done
    [[ $already -eq 0 ]] && STRICT_DOCS+=("$line")
  done < "$f"
done
LEGACY_DOCS=()
for d in "${ASSERTED_DOCS[@]}"; do
  strict=0
  for t in "${STRICT_DOCS[@]+"${STRICT_DOCS[@]}"}"; do [[ "$d" == "$t" ]] && strict=1; done
  [[ $strict -eq 0 ]] && LEGACY_DOCS+=("$d")
done

# A budget is "<doc>=<N>" or "<doc>=<N> share=<S>"; runs of whitespace are
# collapsed to one space so a file line and a quoted --budget argument parse
# the same way.
BUDGETS=()
norm_budget() { local -a w; read -r -a w <<<"$1" || true; echo "${w[*]-}"; }
for f in scripts/doc-form.d/*.budget; do
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(norm_budget "${line%%#*}")"
    [[ -z "$line" ]] && continue
    BUDGETS+=("$line")
  done < "$f"
done
shopt -u nullglob

# --budget '<doc>=<N>[ share=<S>]' (repeatable), in addition to the *.budget files.
while [[ $# -gt 0 ]]; do
  case "$1" in
    --budget)
      [[ $# -ge 2 ]] || { echo "doc-form.sh: --budget needs <doc>=<N>[ share=<S>]" >&2; exit 2; }
      BUDGETS+=("$(norm_budget "$2")"); shift 2 ;;
    --budget=*)
      BUDGETS+=("$(norm_budget "${1#--budget=}")"); shift ;;
    *)
      echo "usage: doc-form.sh [--budget '<doc>=<N>[ share=<S>]']..." >&2; exit 2 ;;
  esac
done
for b in "${BUDGETS[@]+"${BUDGETS[@]}"}"; do
  [[ "$b" =~ ^[^=\ ]+=[0-9]+(\ share=[0-9]+)?$ ]] ||
    { echo "doc-form.sh: bad budget '$b' (want <doc>=<N>[ share=<S>])" >&2; exit 2; }
done

python3 - "${MAX_SENTENCE_WORDS}" "${MAX_PARAGRAPH_SHARE}" "${MAX_PARAGRAPH_WORDS}" \
  "${MAX_ITEM_WORDS}" "${MAX_CELL_WORDS}" "${#STRICT_DOCS[@]}" "${#LEGACY_DOCS[@]}" \
  "${#BUDGETS[@]}" "${STRICT_DOCS[@]+"${STRICT_DOCS[@]}"}" "${LEGACY_DOCS[@]}" \
  "${BUDGETS[@]+"${BUDGETS[@]}"}" "${MEASURED_ONLY[@]}" <<'PYEOF'
import re
import sys

max_words = int(sys.argv[1])
max_share = float(sys.argv[2])
max_para = int(sys.argv[3])
max_item = int(sys.argv[4])
max_cell = int(sys.argv[5])
n_strict = int(sys.argv[6])
n_legacy = int(sys.argv[7])
n_budgets = int(sys.argv[8])
rest = sys.argv[9:]
strict_paths = rest[:n_strict]
legacy_paths = rest[n_strict:n_strict + n_legacy]
budget_specs = rest[n_strict + n_legacy:n_strict + n_legacy + n_budgets]
measured_paths = rest[n_strict + n_legacy + n_budgets:]
asserted_paths = strict_paths + legacy_paths

BULLET = re.compile(r'^\s*([-*+]|\d+\.)\s')
TABLE = re.compile(r'^\s*\|')
HEADING = re.compile(r'^#')
ATX_HEADING = re.compile(r'^#{1,6}(\s|$)')
QUOTE = re.compile(r'^\s*>')
FENCE = re.compile(r'^\s*```')
# A GitHub alert's marker line: "> [!NOTE]" and friends. Markup, not prose.
ALERT = re.compile(r'^\s*>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]', re.IGNORECASE)
# Abbreviations and mid-number dots that must not read as sentence ends.
ABBREV = re.compile(r'\b(e\.g|i\.e|etc|vs|Mr|Mrs|Dr|Fig|No|approx)\.', re.IGNORECASE)
CODESPAN = re.compile(r'`[^`]*`')
LINKORURL = re.compile(r'\bhttps?://\S+')
VERSIONDOT = re.compile(r'(?<=\d)\.(?=\d)')


INDENTED = re.compile(r'^\s+\S')


def classify(line, prev_kind):
    """prev_kind is the kind of the last non-blank line in this block (reset
    at each blank line). A wrapped continuation line of a bullet/numbered
    item — indented, no marker of its own — is still that list item, not a
    new prose paragraph; only an unindented line starts fresh prose."""
    if not line.strip():
        return 'blank'
    if FENCE.match(line):
        return 'fence'
    if HEADING.match(line):
        return 'heading'
    if TABLE.match(line):
        return 'table'
    if QUOTE.match(line):
        return 'quote'
    if BULLET.match(line):
        return 'bullet'
    if prev_kind == 'bullet' and INDENTED.match(line):
        return 'bullet'
    return 'paragraph'


def prose_text(line):
    """Strip a bullet/numbered marker for the sentence-length pass. Table,
    heading, quote and fence lines never reach this — they aren't prose."""
    m = BULLET.match(line)
    return line[m.end():] if m else line


def quote_text(line):
    """Strip the blockquote marker(s) ('>' plus an optional space, repeated
    for nested quotes)."""
    return re.sub(r'^(?:\s*>\s?)+', '', line, count=1)


def table_cells(line):
    """Split a table row into its cell text, for independent sentence
    checks. Splits on unescaped '|' only (`\\|` inside a cell, e.g. an
    alternation like `-to=vaultkv\\|azurekv`, is not a cell boundary), and
    drops the empty leading/trailing cells a row's outer pipes produce."""
    parts = re.split(r'(?<!\\)\|', line)
    return [c.strip() for c in parts if c.strip() and not re.match(r'^:?-+:?$', c.strip())]


def nwords(text):
    return len(text.split())


def line_prose_words(raw):
    """Prose words on one non-fence line: words minus table pipes and
    separator rows, blockquote markers, alert marker lines, list markers and
    heading hashes."""
    if ALERT.match(raw):
        return 0
    if QUOTE.match(raw):
        return line_prose_words(quote_text(raw))
    if TABLE.match(raw):
        return sum(nwords(c) for c in table_cells(raw))
    if HEADING.match(raw):
        return nwords(re.sub(r'^#+\s*', '', raw))
    return nwords(prose_text(raw))


EMPHASIS = re.compile(r'\*{1,2}|_{1,2}')


def sentences_from_block(text):
    text = CODESPAN.sub(' ', text)
    text = LINKORURL.sub(' ', text)
    text = ABBREV.sub(lambda m: m.group(0).replace('.', '\u0000'), text)
    text = VERSIONDOT.sub('\u0000', text)
    # Bold/italic markers sit right before a sentence's capital letter often
    # enough ("**Not brandable**") that leaving them in place would hide the
    # split point from the lookahead below.
    text = EMPHASIS.sub('', text)
    # No lookahead constraint on the next character: a sentence in this
    # codebase's docs often starts with a lowercase code identifier
    # (`wardynd`, `-reconcile`) after a code span is stripped above, and an
    # uppercase-only lookahead silently failed to split those, undercounting
    # real run-on sentences. Abbreviations and mid-number dots are already
    # protected above, so a bare split on [.!?] + optional closing quote +
    # whitespace is the more accurate rule here.
    parts = re.split(r'(?<=[.!?])["\'’”)]?\s+', text)
    return [p.replace('\u0000', '.').strip() for p in parts if p.strip()]


def analyze(path):
    with open(path, encoding='utf-8') as f:
        lines = f.read().splitlines()

    paragraph_lines = 0
    legacy_par = 0
    legacy_total = 0
    table_rows = 0
    mermaid_blocks = 0
    total_nonblank = 0
    prose_words = 0
    in_fence = False
    long_sentences = []
    long_blocks = []
    seen_h1 = False
    seen_h2_after_h1 = False
    lead_lines = 0
    prev_kind = None
    prev_sub = None
    # cur is the open block: a paragraph (consecutive prose lines) or a list
    # item (marker line plus continuation lines). It is closed by a blank
    # line, a heading, a table row, a fence, a new item marker or a change
    # of kind, and then checked against the per-block caps.
    cur = None

    def check_sentences(text):
        for s in sentences_from_block(text):
            n = nwords(s)
            if n > max_words:
                long_sentences.append((n, s[:100]))

    def flush_block():
        nonlocal cur, paragraph_lines
        if cur is None:
            return
        text = ' '.join(cur['texts'])
        check_sentences(text)
        where = ' (in a quote)' if cur['quote'] else ''
        if cur['kind'] == 'paragraph' and cur['words'] > max_para:
            long_blocks.append((cur['words'], 'paragraph' + where, max_para, text[:100]))
        if cur['kind'] == 'item' and cur['words'] > max_item:
            long_blocks.append((cur['words'], 'list item' + where, max_item, text[:100]))
            # An item over the cap is prose in a list's clothing: its lines
            # join the paragraph share (quote lines are already counted, and
            # so are lazy continuation lines, which are paragraph lines).
            if not cur['quote']:
                paragraph_lines += cur['lines'] - cur['lazy']
        cur = None

    def open_block(kind, quote):
        nonlocal cur
        flush_block()
        cur = {'kind': kind, 'quote': quote, 'words': 0, 'lines': 0, 'lazy': 0, 'texts': []}

    def add_to_block(text):
        cur['words'] += nwords(text)
        cur['lines'] += 1
        cur['texts'].append(text)

    def in_item(quote):
        """A paragraph line right after an item's line (no blank line
        between) is that item's lazy continuation: CommonMark renders it
        inside the item, so it counts toward the item cap."""
        return cur is not None and cur['kind'] == 'item' and cur['quote'] == quote

    def do_table(text):
        flush_block()
        for cell in table_cells(text):
            n = nwords(cell)
            if n > max_cell:
                long_blocks.append((n, 'table cell', max_cell, cell[:100]))
            # Each cell is checked on its own: a table row is data, not a
            # flowing paragraph, so joining cells together (or joining rows)
            # would manufacture sentences that were never written as one.
            check_sentences(cell)

    for raw in lines:
        if FENCE.match(raw):
            in_fence = not in_fence
            if in_fence and raw.strip().strip('`').strip().lower() == 'mermaid':
                mermaid_blocks += 1
            flush_block()
            prev_kind = None
            prev_sub = None
            continue
        if in_fence:
            continue

        kind = classify(raw, prev_kind)
        if kind == 'blank':
            flush_block()
            prev_kind = None
            prev_sub = None
            continue
        prev_kind = kind
        if kind != 'quote':
            prev_sub = None

        prose_words += line_prose_words(raw)
        legacy_total += 1
        if kind == 'paragraph':
            legacy_par += 1

        counted = True
        if kind == 'quote':
            text = quote_text(raw)
            if ALERT.match(raw):
                # The marker line is markup: in the denominator, not prose.
                total_nonblank += 1
                flush_block()
                prev_sub = None
            elif not text.strip():
                # A bare ">" spacer: neither side of the ratio.
                counted = False
                flush_block()
                prev_sub = None
            else:
                total_nonblank += 1
                paragraph_lines += 1  # a quote is prose
                sub = classify(text, prev_sub)
                if sub == 'heading' and not ATX_HEADING.match(text):
                    # "> #141) — ..." wraps a sentence onto a line that starts
                    # with '#'; only '#' + space is a heading inside a quote.
                    sub = 'paragraph'
                prev_sub = sub
                if sub == 'bullet':
                    if BULLET.match(text) or cur is None or cur['kind'] != 'item':
                        open_block('item', True)
                    add_to_block(prose_text(text))
                elif sub == 'paragraph':
                    if not in_item(True) and (cur is None or cur['kind'] != 'paragraph' or not cur['quote']):
                        open_block('paragraph', True)
                    add_to_block(text)
                elif sub == 'table':
                    do_table(text)
                else:
                    flush_block()
        else:
            total_nonblank += 1
            if kind == 'paragraph':
                # Still a paragraph line for the share, even when it is a
                # lazy continuation of an item.
                paragraph_lines += 1
                if in_item(False):
                    cur['lazy'] += 1
                elif cur is None or cur['kind'] != 'paragraph' or cur['quote']:
                    open_block('paragraph', False)
                add_to_block(raw)
            elif kind == 'bullet':
                # A line that IS a bullet/numbered marker starts a new list
                # item — sentence analysis must not run its text together with
                # the previous item's, even though both are 'bullet' kind and
                # no blank line separates them in the source.
                if BULLET.match(raw) or cur is None or cur['kind'] != 'item':
                    open_block('item', False)
                add_to_block(prose_text(raw))
            elif kind == 'table':
                table_rows += 1
                do_table(raw)
            else:
                flush_block()

        if counted:
            if kind == 'heading':
                if raw.startswith('# ') and not seen_h1:
                    seen_h1 = True
                elif raw.startswith('## ') and seen_h1 and not seen_h2_after_h1:
                    seen_h2_after_h1 = True
            elif seen_h1 and not seen_h2_after_h1:
                lead_lines += 1
        elif seen_h1 and not seen_h2_after_h1:
            lead_lines += 1
    flush_block()

    share = (100.0 * paragraph_lines / total_nonblank) if total_nonblank else 0.0
    legacy_share = (100.0 * legacy_par / legacy_total) if legacy_total else 0.0
    has_summary = seen_h1 and seen_h2_after_h1 and 0 < lead_lines <= 5
    return {
        'lines': len(lines), 'share': share, 'legacy_share': legacy_share,
        'table_rows': table_rows,
        'mermaid': mermaid_blocks, 'long_sentences': long_sentences,
        'long_blocks': long_blocks, 'prose_words': prose_words,
        'has_summary': has_summary,
    }


print(f"=== doc-form caps: sentence {max_words}, paragraph {max_para}, list item {max_item}, "
      f"table cell {max_cell} words; paragraph share {max_share:.0f}% ===")
print("=== doc-form report (paragraph share / table rows / mermaid blocks) ===")
fail = 0
for path in asserted_paths + measured_paths:
    try:
        r = analyze(path)
    except FileNotFoundError:
        print(f"  SKIP {path} (not found)")
        continue
    shown = r['share'] if path in strict_paths else r['legacy_share']
    print(f"{path}: {r['lines']} lines, paragraph share {shown:.0f}%, "
          f"{r['table_rows']} table rows, {r['mermaid']} mermaid, "
          f"summary={'yes' if r['has_summary'] else 'no'}, "
          f"long_sentences={len(r['long_sentences'])}, "
          f"strict: share {r['share']:.0f}% long_blocks={len(r['long_blocks'])}, "
          f"prose_words={r['prose_words']}")

print()
print(f"=== doc-form assertions ({len(strict_paths)} docs on every cap, "
      f"{len(legacy_paths)} on the original checks) ===")
if not asserted_paths:
    print("  no pages named in ASSERTED_DOCS — nothing to assert")
for path in asserted_paths:
    try:
        r = analyze(path)
    except FileNotFoundError:
        print(f"  FAIL {path}: file not found")
        fail = 1
        continue
    ok = True
    strict = path in strict_paths
    share = r['share'] if strict else r['legacy_share']
    if share > max_share:
        print(f"  FAIL {path}: paragraph share {share:.0f}% (max {max_share:.0f}%)")
        ok = False
    if not r['has_summary']:
        print(f"  FAIL {path}: no <=5-line summary between the H1 title and the first H2 heading")
        ok = False
    for words, snippet in r['long_sentences'][:5]:
        print(f"  FAIL {path}: {words}-word sentence (max {max_words}): \"{snippet}...\"")
        ok = False
    for words, what, cap, snippet in (r['long_blocks'][:5] if strict else []):
        print(f"  FAIL {path}: {words}-word {what} (max {cap}): \"{snippet}...\"")
        ok = False
    if ok:
        print(f"  ok   {path} (paragraph share {share:.0f}%)")
    else:
        fail = 1

print()
print(f"=== doc-form prose-word budgets ({len(budget_specs)} docs; a doc whose prose or share grew fails) ===")
if not budget_specs:
    print("  no budgets set")
for spec in budget_specs:
    first, _, share_part = spec.partition(' ')
    path, _, limit = first.partition('=')
    limit = int(limit)
    # share=S gates the STRICT share (quote lines and over-cap items are
    # prose), whatever tier the doc is on.
    max_doc_share = int(share_part[len('share='):]) if share_part else None
    try:
        r = analyze(path)
    except FileNotFoundError:
        print(f"  FAIL {path}: file not found")
        fail = 1
        continue
    ok = True
    if r['prose_words'] > limit:
        print(f"  FAIL {path}: {r['prose_words']} prose words (budget {limit}, over by {r['prose_words'] - limit})")
        ok = False
    if max_doc_share is not None and r['share'] > max_doc_share:
        print(f"  FAIL {path}: strict paragraph share {r['share']:.1f}% (budget share {max_doc_share}%)")
        ok = False
    if ok:
        shown = f", strict share {r['share']:.1f}% (budget {max_doc_share}%)" if max_doc_share is not None else ''
        print(f"  ok   {path} ({r['prose_words']} prose words, budget {limit}{shown})")
    else:
        fail = 1

print()
print("doc-form gate: " + ("PASS" if fail == 0 else "FAIL"))
sys.exit(1 if fail else 0)
PYEOF
