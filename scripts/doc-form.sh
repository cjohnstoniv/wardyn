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
# far: docs/operations/*.md. The rest (OPERATIONS.md's own guarded sections,
# THREAT-MODEL.md, DESKTOP.md, SSH.md, RELEASING.md, the Helm README) are a
# separate, later pass — asserting on unconverted docs here would just turn
# the gate red for work not yet done. ARCHITECTURE.md is absent even from the
# measured list: the issue says it "keeps its 3" diagrams as-is.
#
# paragraph share = non-blank, non-fenced lines that are not a bullet,
# numbered step, table row, heading or blockquote, divided by all non-blank,
# non-fenced lines. Fenced code is excluded entirely (neither side of the
# ratio) since it isn't prose.
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
ASSERTED_DOCS=(docs/operations/*.md)
MAX_PARAGRAPH_SHARE=50
MAX_SENTENCE_WORDS=35

python3 - "${MAX_SENTENCE_WORDS}" "${MAX_PARAGRAPH_SHARE}" "${#ASSERTED_DOCS[@]}" \
  "${ASSERTED_DOCS[@]}" "${MEASURED_ONLY[@]}" <<'PYEOF'
import re
import sys

max_words = int(sys.argv[1])
max_share = float(sys.argv[2])
n_asserted = int(sys.argv[3])
rest = sys.argv[4:]
asserted_paths = rest[:n_asserted]
measured_paths = rest[n_asserted:]

BULLET = re.compile(r'^\s*([-*+]|\d+\.)\s')
TABLE = re.compile(r'^\s*\|')
HEADING = re.compile(r'^#')
QUOTE = re.compile(r'^\s*>')
FENCE = re.compile(r'^\s*```')
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
    table_rows = 0
    mermaid_blocks = 0
    total_nonblank = 0
    in_fence = False
    block_lines = []
    long_sentences = []
    seen_h1 = False
    seen_h2_after_h1 = False
    lead_lines = 0
    prev_kind = None

    def flush_block():
        if not block_lines:
            return
        text = ' '.join(block_lines)
        for s in sentences_from_block(text):
            words = [w for w in s.split() if w]
            if len(words) > max_words:
                long_sentences.append((len(words), s[:100]))
        block_lines.clear()

    for raw in lines:
        if FENCE.match(raw):
            in_fence = not in_fence
            if in_fence and raw.strip().strip('`').strip().lower() == 'mermaid':
                mermaid_blocks += 1
            flush_block()
            prev_kind = None
            continue
        if in_fence:
            continue

        kind = classify(raw, prev_kind)
        if kind == 'blank':
            flush_block()
            prev_kind = None
            continue
        prev_kind = kind

        total_nonblank += 1
        if kind == 'paragraph':
            paragraph_lines += 1
        elif kind == 'table':
            table_rows += 1

        if kind == 'heading':
            if raw.startswith('# ') and not seen_h1:
                seen_h1 = True
            elif raw.startswith('## ') and seen_h1 and not seen_h2_after_h1:
                seen_h2_after_h1 = True
        elif seen_h1 and not seen_h2_after_h1:
            lead_lines += 1

        if kind in ('paragraph', 'bullet'):
            # A line that IS a bullet/numbered marker starts a new list
            # item — sentence analysis must not run its text together with
            # the previous item's, even though both are 'bullet' kind and
            # no blank line separates them in the source.
            if kind == 'bullet' and BULLET.match(raw):
                flush_block()
            block_lines.append(prose_text(raw))
        else:
            flush_block()
    flush_block()

    share = (100.0 * paragraph_lines / total_nonblank) if total_nonblank else 0.0
    has_summary = seen_h1 and seen_h2_after_h1 and 0 < lead_lines <= 5
    return {
        'lines': len(lines), 'share': share, 'table_rows': table_rows,
        'mermaid': mermaid_blocks, 'long_sentences': long_sentences,
        'has_summary': has_summary,
    }


print("=== doc-form report (paragraph share / table rows / mermaid blocks) ===")
fail = 0
for path in asserted_paths + measured_paths:
    try:
        r = analyze(path)
    except FileNotFoundError:
        print(f"  SKIP {path} (not found)")
        continue
    print(f"{path}: {r['lines']} lines, paragraph share {r['share']:.0f}%, "
          f"{r['table_rows']} table rows, {r['mermaid']} mermaid, "
          f"summary={'yes' if r['has_summary'] else 'no'}, "
          f"long_sentences={len(r['long_sentences'])}")

print()
print(f"=== doc-form assertions ({len(asserted_paths)} docs brought into form) ===")
for path in asserted_paths:
    r = analyze(path)
    ok = True
    if r['share'] > max_share:
        print(f"  FAIL {path}: paragraph share {r['share']:.0f}% (max {max_share:.0f}%)")
        ok = False
    if not r['has_summary']:
        print(f"  FAIL {path}: no <=5-line summary between the H1 title and the first H2 heading")
        ok = False
    for words, snippet in r['long_sentences'][:5]:
        print(f"  FAIL {path}: {words}-word sentence (max {max_words}): \"{snippet}...\"")
        ok = False
    if ok:
        print(f"  ok   {path} (paragraph share {r['share']:.0f}%)")
    else:
        fail = 1

print()
print("doc-form gate: " + ("PASS" if fail == 0 else "FAIL"))
sys.exit(1 if fail else 0)
PYEOF
