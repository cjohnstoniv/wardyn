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
# FRONT MATTER: a YAML block that starts on line 1 (line 1 is exactly "---",
# the block ends at the next line that is exactly "---", both included) is
# metadata, not prose. It is no paragraph block and holds no sentences, so it
# meets no cap, no summary rule and no share; a skill's long "description:" line
# is the case. It is front matter only if every line between the two "---"
# lines is blank, starts with whitespace, or starts with a YAML key
# ("name:", "allowed-tools:"); a free prose line inside makes the whole block
# ordinary page text. Its words still count in the prose-word budget, which is
# unchanged. A "---" rule anywhere else in a page is not front matter.
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
# WAIVERS: scripts/doc-form.d/*.waive (tab-separated, '#' comments) name the
# few over-cap items a lane keeps on purpose, so the gate counts them and says
# so instead of failing on them or never seeing them. Two kinds of line:
#   sentence<TAB>path<TAB>sha1<TAB>words<TAB>reason
#       one sentence of 36-40 words with no clause joiner (';', ' — ', ', so ',
#       ', and ', ': '). sha1 is of the sentence as sentences_from_block()
#       returns it (code spans, URLs and emphasis marks removed), with its
#       whitespace folded to single spaces. A failing, waivable sentence prints
#       the line to paste.
#   table<TAB>path<TAB>header line<TAB>reason
#       every over-cap cell of the one table whose first row equals the header
#       line; the reason must cite a "PLAN §" section. Sentences inside those
#       cells still need their own sentence waiver.
# A malformed waiver, a refused one, and one that matches nothing all fail the
# gate. The report prints "long_sentences: N (M waived)" and "long_blocks: N
# (M waived)"; only the unwaived items fail.
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
import glob
import hashlib
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
YAML_KEY = re.compile(r'^[A-Za-z0-9_-]+:')


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


SPAN_GAP = '\u2003'


def sentences_from_block(text):
    # An em space, not a plain one: it splits and counts exactly like a space
    # (\s, str.split), but the joiner check can tell a removed code span from
    # the real spaces around it (", `0-9` and `-`" is not ", and ").
    text = CODESPAN.sub(SPAN_GAP, text)
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


JOINERS = (';', ' \u2014 ', ', so ', ', and ', ': ')
WAIVE_MIN, WAIVE_MAX = 36, 40


def joiner_in(text):
    """text keeps SPAN_GAP where a code span was; only runs of plain spaces
    collapse, so a joiner counts only when written out in the prose."""
    text = re.sub(r'[ \t]+', ' ', text)
    return next((j for j in JOINERS if j in text), None)


def fold(sentence):
    return ' '.join(sentence.split())


def sentence_sha1(sentence):
    return hashlib.sha1(fold(sentence).encode('utf-8')).hexdigest()


def load_waivers():
    """scripts/doc-form.d/*.waive -> ({path: [waiver]}, [(where, message)]).
    Fields split on tabs only; the last field (the reason) keeps any text."""
    by_path, errors = {}, []
    for f in sorted(glob.glob('scripts/doc-form.d/*.waive')):
        with open(f, encoding='utf-8') as fh:
            for no, raw in enumerate(fh.read().splitlines(), 1):
                if not raw.strip() or raw.lstrip().startswith('#'):
                    continue
                where = f"{f}:{no}"
                kind = raw.split('\t', 1)[0]
                if kind == 'sentence':
                    parts = raw.split('\t', 4)
                    if len(parts) != 5 or not parts[4].strip():
                        errors.append((where, 'want sentence<TAB>path<TAB>sha1<TAB>words<TAB>reason'))
                        continue
                    _, path, sha, words, reason = parts
                    if not re.fullmatch(r'[0-9a-f]{40}', sha):
                        errors.append((where, f'sha1 "{sha}" is not 40 lowercase hex digits'))
                        continue
                    if not words.isdigit() or not WAIVE_MIN <= int(words) <= WAIVE_MAX:
                        errors.append((where, f'words "{words}": only {WAIVE_MIN}-{WAIVE_MAX}-word sentences can be waived; longer ones split or become a table'))
                        continue
                    w = {'kind': kind, 'where': where, 'sha': sha, 'words': int(words)}
                elif kind == 'table':
                    parts = raw.split('\t', 3)
                    if len(parts) != 4 or not parts[2].strip() or not parts[3].strip():
                        errors.append((where, 'want table<TAB>path<TAB>header line<TAB>reason'))
                        continue
                    _, path, header, reason = parts
                    if 'PLAN \u00a7' not in reason:
                        errors.append((where, 'a table waiver must cite a plan section ("PLAN \u00a7") in its reason'))
                        continue
                    w = {'kind': kind, 'where': where, 'header': header.strip()}
                else:
                    errors.append((where, 'first field must be "sentence" or "table"'))
                    continue
                by_path.setdefault(path, []).append(w)
    return by_path, errors


WAIVERS, WAIVER_PARSE_ERRORS = load_waivers()


def apply_waivers(path, long_sentences, long_blocks, tables):
    """Mark the items this doc's waivers cover; return [(where, message)] for
    every waiver that is refused or matches nothing."""
    errors = []
    for w in WAIVERS.get(path, []):
        if w['kind'] == 'sentence':
            hit = next((s for s in long_sentences if s['sha'] == w['sha'] and not s['waived']), None)
            if hit is None:
                errors.append((w['where'], 'matches no over-cap sentence in ' + path))
            elif hit['n'] != w['words']:
                errors.append((w['where'], f"declares {w['words']} words but the sentence has {hit['n']}"))
            elif hit['joiner']:
                errors.append((w['where'], f"the {hit['n']}-word sentence contains the clause joiner \"{hit['joiner'].strip()}\"; split it instead"))
            else:
                hit['waived'] = True
        else:
            n_tables = tables.count(w['header'])
            cells = [b for b in long_blocks if b['what'] == 'table cell' and b['table'] == w['header'] and not b['waived']]
            if n_tables == 0:
                errors.append((w['where'], 'no table in ' + path + ' has this header line'))
            elif n_tables > 1:
                errors.append((w['where'], f'{n_tables} tables in {path} share this header line'))
            elif not cells:
                errors.append((w['where'], 'no cell of this table is over the cap'))
            else:
                for b in cells:
                    b['waived'] = True
    return errors


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
    tables = []
    table_head = None
    saw_table_row = False
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

    def block(n, what, cap, text, table=None):
        return {'n': n, 'what': what, 'cap': cap, 'snippet': text[:100],
                'table': table, 'waived': False}

    def check_sentences(text):
        for s in sentences_from_block(text):
            n = nwords(s)
            if n > max_words:
                long_sentences.append({'n': n, 'snippet': s.replace(SPAN_GAP, ' ')[:100],
                                       'text': fold(s), 'joiner': joiner_in(s),
                                       'sha': sentence_sha1(s), 'waived': False})

    def flush_block():
        nonlocal cur, paragraph_lines
        if cur is None:
            return
        text = ' '.join(cur['texts'])
        check_sentences(text)
        where = ' (in a quote)' if cur['quote'] else ''
        if cur['kind'] == 'paragraph' and cur['words'] > max_para:
            long_blocks.append(block(cur['words'], 'paragraph' + where, max_para, text))
        if cur['kind'] == 'item' and cur['words'] > max_item:
            long_blocks.append(block(cur['words'], 'list item' + where, max_item, text))
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
        nonlocal table_head, saw_table_row
        flush_block()
        saw_table_row = True
        if table_head is None:
            table_head = text.strip()
            tables.append(table_head)
        for cell in table_cells(text):
            n = nwords(cell)
            if n > max_cell:
                long_blocks.append(block(n, 'table cell', max_cell, cell, table_head))
            # Each cell is checked on its own: a table row is data, not a
            # flowing paragraph, so joining cells together (or joining rows)
            # would manufacture sentences that were never written as one.
            check_sentences(cell)

    front_end = -1
    if lines and lines[0] == '---':
        front_end = next((i for i in range(1, len(lines)) if lines[i] == '---'), -1)
        if any(l.strip() and not l[0].isspace() and not YAML_KEY.match(l)
               for l in lines[1:front_end]):
            front_end = -1

    for lineno, raw in enumerate(lines):
        if lineno <= front_end:
            # Front matter: words stay in the budget; nothing else is measured.
            prose_words += line_prose_words(raw)
            continue
        # A table is the run of table rows; any other line (blank, fence, text)
        # ends it, and its first row is the header line a table waiver names.
        if not saw_table_row:
            table_head = None
        saw_table_row = False
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
    waiver_errors = apply_waivers(path, long_sentences, long_blocks, tables)
    return {
        'waiver_errors': waiver_errors,
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
          f"long_sentences: {len(r['long_sentences'])} ({sum(s['waived'] for s in r['long_sentences'])} waived), "
          f"strict: share {r['share']:.0f}% long_blocks: {len(r['long_blocks'])} "
          f"({sum(b['waived'] for b in r['long_blocks'])} waived), "
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
    for s in [s for s in r['long_sentences'] if not s['waived']][:5]:
        print(f"  FAIL {path}: {s['n']}-word sentence (max {max_words}): \"{s['snippet']}...\"")
        if WAIVE_MIN <= s['n'] <= WAIVE_MAX and not s['joiner']:
            print(f"       waivable as: sentence\t{path}\t{s['sha']}\t{s['n']}\t<reason>")
        ok = False
    for b in [b for b in r['long_blocks'] if not b['waived']][:5] if strict else []:
        print(f"  FAIL {path}: {b['n']}-word {b['what']} (max {b['cap']}): \"{b['snippet']}...\"")
        ok = False
    if ok:
        print(f"  ok   {path} (paragraph share {share:.0f}%)")
    else:
        fail = 1

if WAIVERS or WAIVER_PARSE_ERRORS:
    print()
    print(f"=== doc-form waivers ({sum(map(len, WAIVERS.values())) + len(WAIVER_PARSE_ERRORS)} lines; a refused or stale waiver fails) ===")
    for where, msg in WAIVER_PARSE_ERRORS:
        print(f"  FAIL {where}: {msg}")
        fail = 1
    for path, ws in WAIVERS.items():
        try:
            errs = dict(analyze(path)['waiver_errors'])
        except FileNotFoundError:
            for w in ws:
                print(f"  FAIL {w['where']}: {path} not found")
            fail = 1
            continue
        for w in ws:
            if w['where'] in errs:
                print(f"  FAIL {w['where']}: {errs[w['where']]}")
                fail = 1
            else:
                print(f"  ok   {w['where']} ({w['kind']} waiver, {path})")

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
