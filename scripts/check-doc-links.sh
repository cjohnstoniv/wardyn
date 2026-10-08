#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-doc-links.sh — every relative Markdown link, and every heading anchor
# it names, in a tracked doc must resolve; and in the docs a lane has brought
# into the linked form, a reference to a repo file must BE a link. Companion
# to doc-form.sh (the shape of the text) and check-diagrams.sh (the pictures);
# this gates the links between them. Nothing checked links before it, so a
# renamed heading or a moved file broke readers silently.
#
# Modes
#   (none)                  resolve every link in every tracked .md except the
#                           frozen files below; apply the must-link rule to the
#                           docs listed in scripts/doc-form.d/*.links.
#   --must-link <doc>...    additionally apply the must-link rule to <doc>...
#   --selftest              pin the slugger: see SELFTEST below. No tree scan.
#
# What counts as a link: inline [text](target) and images, reference
# definitions "[ref]: target", and href=/src=/srcset= in raw HTML. Fenced
# code, inline code spans and HTML comments are not links. External targets
# (a scheme or "//") and mailto: are skipped. A target resolves when the file
# or directory is TRACKED (git ls-files): an untracked file never counts,
# because a fresh clone will not have it. A directory target needs no anchor
# check; a non-Markdown file's #fragment (line anchors, #Symbol) is not
# checked. A Markdown target's #fragment must be the slug of one of its
# headings, or an id=/name= attribute in its raw HTML.
#
# The slugger is GitHub's (github-slugger): lowercase the heading's rendered
# text; keep letters, marks, decimal digits, letter numbers (Nl), '_' and
# '-'; drop everything else, other numbers (No: ², ½) included;
# spaces become '-'. NOTHING is collapsed or trimmed, so "A — B" is "a--b" and
# a heading that ends in a dash keeps it. A repeated slug gains -1, -2, ...
# Backticks and '*' emphasis are removed; so is '_' emphasis around a whole
# word, but a heading should not use it — the two renderers disagree. Unicode
# marks (combining accents) are kept, as GitHub keeps them; Python's \w would
# drop them, so the filter tests the character category instead.
#
# Frozen files (CHANGELOG.md, CHANGELOG-ARCHIVE.md, THIRD-PARTY-NOTICES.md,
# deploy/images/THIRD-PARTY*.md, deploy/images/third-party-gpl-historical.md)
# are history: their dead links are counted and shown, never failed.
#
# MUST-LINK rule (docs named in scripts/doc-form.d/*.links, one path per line;
# docs/design/** is never subject to it — it quotes frozen strings):
#   - a backticked span, or a bare word, that names a tracked FILE
#     (repo-relative or relative to the doc) and is not a link's text FAILS:
#     write it as a link. Names: dir/path.ext for go md sh ts tsx yaml yml
#     json sql toml py mjs css html example (optional #fragment), and
#     dir/Makefile, dir/Dockerfile, dir/LICENSE, dir/NOTICE. Not references:
#     directories (`scripts/`, or a token that resolves to one), names with
#     no '/' (`Makefile`, `main.go`), the doc's own path, a lone '/'.
#   - a span or word "path.go:123" (go md ts sh) is a line citation: FAIL
#     here, counted as a WARN everywhere else. Cite the symbol instead.
#   - link text that is one backtick span naming a tracked path must be that
#     path (repo-relative) and the link must point at it. Like the rule above,
#     it applies only to a span with a '/': link text `install.sh` (a bare file
#     name) may point at any file, for the same reason `install.sh` alone is
#     not a reference.
#   Skipped: fenced code, headings, spans that resolve to nothing (runtime
#   paths such as /etc/wardyn).
set -euo pipefail
cd "$(dirname "$0")/.."

MUST_LINK_DOCS=()
shopt -s nullglob
for f in scripts/doc-form.d/*.links; do
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    line="${line//[[:space:]]/}"
    [[ -n "$line" ]] && MUST_LINK_DOCS+=("$line")
  done < "$f"
done
shopt -u nullglob

MODE=scan
EXTRA=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --selftest) MODE=selftest; shift ;;
    --must-link)
      shift
      while [[ $# -gt 0 && "$1" != --* ]]; do EXTRA+=("$1"); shift; done ;;
    *) echo "usage: check-doc-links.sh [--selftest | --must-link <doc>...]" >&2; exit 2 ;;
  esac
done

python3 - "$MODE" "${MUST_LINK_DOCS[@]+"${MUST_LINK_DOCS[@]}"}" "--" "${EXTRA[@]+"${EXTRA[@]}"}" <<'PYEOF'
import bisect
import fnmatch
import html
import os
import posixpath
import re
import subprocess
import sys
import unicodedata
from urllib.parse import unquote

args = sys.argv[1:]
mode = args[0]
split = args.index('--')
listed_must = args[1:split]
extra_must = args[split + 1:]

FROZEN_GLOBS = [
    'CHANGELOG.md', 'CHANGELOG-ARCHIVE.md', 'THIRD-PARTY-NOTICES.md',
    'deploy/images/THIRD-PARTY*.md', 'deploy/images/third-party-gpl-historical.md',
]
POLICY_HELP_TS = 'ui/src/app/components/wardyn/policy-field-help.ts'
POLICY_DOC = 'docs/POLICIES.md'


def is_frozen(path):
    return any(fnmatch.fnmatchcase(path, g) for g in FROZEN_GLOBS)


def tracked_files():
    out = subprocess.run(['git', 'ls-files', '-z'], check=True, capture_output=True).stdout
    return [p for p in out.decode('utf-8').split('\0') if p]


# ---------------------------------------------------------------- slugger --

CODE_SPAN = re.compile(r'(`+)(.+?)\1')


def heading_plain(text):
    """The text of a heading as GitHub renders it, before slugging."""
    parts = []
    last = 0
    for m in CODE_SPAN.finditer(text):
        parts.append((False, text[last:m.start()]))
        parts.append((True, m.group(2)))
        last = m.end()
    parts.append((False, text[last:]))
    out = []
    for is_code, chunk in parts:
        if is_code:
            out.append(chunk)
            continue
        chunk = re.sub(r'!\[([^\]]*)\]\([^)]*\)', r'\1', chunk)
        chunk = re.sub(r'\[([^\]]*)\]\([^)]*\)', r'\1', chunk)
        chunk = re.sub(r'\[([^\]]*)\]\[[^\]]*\]', r'\1', chunk)
        chunk = re.sub(r'<[^>]+>', '', chunk)
        chunk = html.unescape(chunk)
        chunk = chunk.replace('*', '')
        chunk = re.sub(r'(?<![\w])_+(?=\S)(.+?)(?<=\S)_+(?![\w])', r'\1', chunk)
        out.append(chunk)
    return ''.join(out).strip()


def slug_base(text):
    keep = []
    for ch in heading_plain(text).lower():
        cat = unicodedata.category(ch)
        # Letters, marks (U+FE0F included), decimal digits and letter
        # numbers (Nl: Roman numerals). Other numbers (No: ², ½) are
        # stripped, as github-slugger's character ranges strip them.
        if cat[0] in 'LM' or cat in ('Nd', 'Nl') or ch in '_- ':
            keep.append('-' if ch == ' ' else ch)
    return ''.join(keep)


class Slugger:
    """github-slugger: a repeated slug gains -1, -2, ... and the suffixed
    form is itself taken, so "a", "a", "a-1" gives a, a-1, a-1-1."""

    def __init__(self):
        self.seen = {}

    def slug(self, text):
        base = slug_base(text)
        s = base
        if s in self.seen:
            n = self.seen[base]
            while s in self.seen:
                n += 1
                s = f'{base}-{n}'
            self.seen[base] = n
        self.seen[s] = 0
        return s


# ----------------------------------------------------------- doc parsing --

FENCE_OPEN = re.compile(r'^ {0,3}(`{3,}|~{3,})')
HTML_COMMENT = re.compile(r'<!--.*?-->', re.DOTALL)
ATX = re.compile(r'^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$')
ID_ATTR = re.compile(r'\b(?:id|name)\s*=\s*["\']([^"\']+)["\']')


def blank_noncontent(text):
    """Return text with fenced code and HTML comments replaced by blank
    lines/spaces of the same length, so offsets and line numbers survive."""
    lines = text.split('\n')
    out = []
    fence = None
    for ln in lines:
        if fence is None:
            m = FENCE_OPEN.match(ln)
            if m:
                fence = m.group(1)
                out.append('')
                continue
            out.append(ln)
        else:
            m = re.match(r'^ {0,3}(' + re.escape(fence[0]) + '{' + str(len(fence)) + r',})\s*$', ln)
            if m:
                fence = None
            out.append('')
    joined = '\n'.join(out)
    return HTML_COMMENT.sub(lambda m: re.sub(r'[^\n]', ' ', m.group(0)), joined)


class Doc:
    def __init__(self, path):
        self.path = path
        with open(path, encoding='utf-8') as f:
            raw = f.read()
        self.text = blank_noncontent(raw)
        self.newlines = [i for i, c in enumerate(self.text) if c == '\n']
        self.heading_lines = set()
        self._anchors = None

    def line_of(self, offset):
        return bisect.bisect_left(self.newlines, offset) + 1

    @property
    def anchors(self):
        if self._anchors is None:
            sl = Slugger()
            found = set()
            for i, ln in enumerate(self.text.split('\n'), 1):
                m = ATX.match(ln)
                if m:
                    self.heading_lines.add(i)
                    found.add(sl.slug(m.group(2)))
            for m in ID_ATTR.finditer(self.text):
                found.add(m.group(1))
            self._anchors = found
        return self._anchors

    def heading_line_set(self):
        self.anchors  # noqa: B018 — populates heading_lines
        return self.heading_lines


# A code span, or a link whose text may hold code spans and one level of
# nested brackets (an image inside a link). The code alternative sits first
# so a "[x](y)" written inside backticks stays code.
TARGET = r'(?:<[^>\n]*>|(?:[^()\s]|\([^()\s]*\))*)'
TOKEN = re.compile(
    r'(?P<code>(?P<ticks>`+)(?:(?!\n[ \t]*\n).)+?(?P=ticks))'
    r'|(?P<link>(?P<bang>!)?\[(?P<text>(?:[^\[\]`]|`[^`\n]*`|\[[^\[\]]*\])*)\]'
    r'\((?P<target>\s*' + TARGET + r')(?:\s+(?:"[^"]*"|\'[^\']*\'))?\s*\))',
    re.DOTALL)
REFDEF = re.compile(r'^ {0,3}\[[^\]\n]+\]:[ \t]*(<[^>\n]*>|\S+)', re.MULTILINE)
HTMLATTR = re.compile(r'\b(href|src|srcset)\s*=\s*(?:"([^"]*)"|\'([^\']*)\')', re.IGNORECASE)
SCHEME = re.compile(r'^(?:[A-Za-z][A-Za-z0-9+.\-]*:|//)')


def targets_in(doc):
    """Yield (line, target) for every link-ish target in doc, outside code."""
    text = doc.text

    def walk(chunk, base):
        for m in TOKEN.finditer(chunk):
            if m.group('code'):
                continue
            tgt = m.group('target').strip()
            if tgt.startswith('<') and tgt.endswith('>'):
                tgt = tgt[1:-1]
            yield doc.line_of(base + m.start()), tgt
            # An image (or a link) inside this link's text is a link too.
            yield from walk(m.group('text'), base + m.start('text'))

    yield from walk(text, 0)
    for m in REFDEF.finditer(text):
        tgt = m.group(1).strip()
        if tgt.startswith('<') and tgt.endswith('>'):
            tgt = tgt[1:-1]
        yield doc.line_of(m.start(1)), tgt
    # Raw HTML attributes, outside code spans.
    no_code = TOKEN.sub(lambda m: re.sub(r'[^\n]', ' ', m.group(0)) if m.group('code') else m.group(0), text)
    for m in HTMLATTR.finditer(no_code):
        val = m.group(2) if m.group(2) is not None else m.group(3)
        if m.group(1).lower() == 'srcset':
            for part in val.split(','):
                tok = part.strip().split()
                if tok:
                    yield doc.line_of(m.start()), tok[0]
        else:
            yield doc.line_of(m.start()), val.strip()


# ------------------------------------------------------------ resolution --

files = tracked_files()
fileset = set(files)
dirset = {'.'}
for p in files:
    d = posixpath.dirname(p)
    while d and d not in dirset:
        dirset.add(d)
        d = posixpath.dirname(d)

docs_cache = {}


def get_doc(path):
    if path not in docs_cache:
        docs_cache[path] = Doc(path)
    return docs_cache[path]


def resolve(src, target):
    """Return (kind, detail): kind is 'skip', 'ok', or 'fail'; for ok on a
    Markdown file detail is the (path, fragment) whose anchor is still to be
    checked."""
    if not target or SCHEME.match(target) or '{{' in target or '${' in target:
        return 'skip', None
    path, _, frag = target.partition('#')
    path = unquote(path.split('?', 1)[0])
    frag = unquote(frag)
    if not path:
        dest = src
    else:
        if path.startswith('/'):
            joined = path.lstrip('/')
        else:
            joined = posixpath.join(posixpath.dirname(src), path)
        dest = posixpath.normpath(joined)
        if dest == '..' or dest.startswith('../'):
            return 'fail', f'{target}: leaves the repository'
        if dest not in fileset and dest not in dirset:
            if os.path.exists(dest):
                return 'fail', f'{target}: exists but is not tracked'
            return 'fail', f'{target}: no such tracked file or directory ({dest})'
    if dest in dirset and dest not in fileset:
        return 'ok', None
    if frag and dest.endswith('.md'):
        return 'ok', (dest, frag)
    return 'ok', None


# ------------------------------------------------------------ must-link ---

FILE_EXT = (r'go|md|sh|ts|tsx|yaml|yml|json|sql|toml|py|mjs|css|html|example')
PATH_FILE = re.compile(r'^[A-Za-z0-9_./-]+\.(?:' + FILE_EXT + r')(?:#[A-Za-z0-9_.,-]+)?$')
PATH_NAMED = re.compile(r'^(?:[A-Za-z0-9_./-]+/)?(?:Makefile|Dockerfile|LICENSE|NOTICE)$')
PATH_DIR = re.compile(r'^[A-Za-z0-9_./-]*/[A-Za-z0-9_./-]*$')
LINE_CITE = re.compile(r'^[A-Za-z0-9_./-]+\.(?:go|md|ts|sh):\d+(?:-\d+)?$')


def named_path(src, token):
    """The tracked path a token names, or None. Tries repo-relative, then
    relative to the doc."""
    if not (PATH_FILE.match(token) or PATH_NAMED.match(token) or PATH_DIR.match(token)):
        return None
    bare = token.split('#', 1)[0]
    cands = [bare.rstrip('/')]
    cands.append(posixpath.normpath(posixpath.join(posixpath.dirname(src), bare.rstrip('/'))))
    for c in cands:
        if c and c != '.' and (c in fileset or c in dirset):
            return c
    return None


def file_reference(src, token):
    """The tracked FILE a token refers to under the must-link rule, or None.
    Files only (STYLE 4.2): a token with no '/' (a bare name such as
    `Makefile` or `main.go`), a directory (a trailing '/' or a token that
    resolves to one), the doc's own path, and prose such as a lone '/' are
    not references."""
    if '/' not in token or token.endswith('/'):
        return None
    if not (PATH_FILE.match(token) or PATH_NAMED.match(token)):
        return None
    bare = token.split('#', 1)[0]
    for c in (bare, posixpath.normpath(posixpath.join(posixpath.dirname(src), bare))):
        if c in fileset:
            return None if c == src else c
    return None


def clean_word(w):
    """A prose word with the punctuation a sentence wraps around it removed."""
    w = re.sub(r'^[(\[{"\'*_<]+', '', w)
    return re.sub(r'[)\]}"\'*_>,;:!?]+$', '', w).rstrip('.')


def check_must_link(src, fails):
    doc = get_doc(src)
    hlines = doc.heading_line_set()
    text = doc.text
    pos = 0
    n_checked = 0

    def judge(token, offset, from_span):
        nonlocal n_checked
        line = doc.line_of(offset)
        if line in hlines:
            return
        if LINE_CITE.match(token):
            fails.append(f'{src}:{line}: line citation `{token}`: cite the symbol, not the line')
            return
        hit = file_reference(src, token)
        if hit is not None:
            n_checked += 1
            kind = 'span' if from_span else 'word'
            fails.append(f'{src}:{line}: {kind} `{token}` names tracked {hit}: write it as a link')

    for m in TOKEN.finditer(text):
        prose = text[pos:m.start()]
        for w in re.finditer(r'\S+', prose):
            tok = clean_word(w.group(0))
            if tok:
                judge(tok, pos + w.start(), False)
        pos = m.end()
        if m.group('code'):
            ticks = m.group('ticks')
            judge(m.group('code')[len(ticks):-len(ticks)].strip(), m.start(), True)
            continue
        label = m.group('text').strip()
        sm = re.fullmatch(r'(`+)(.+?)\1', label, re.DOTALL)
        if sm:
            span = sm.group(2).strip()
            want = named_path(src, span) if '/' in span else None  # a bare name is not a reference
            if want is not None and not LINE_CITE.match(span):
                tgt = m.group('target').strip().strip('<>')
                status, _ = resolve(src, tgt)
                dest = None
                if status == 'ok':
                    p = unquote(tgt.partition('#')[0].split('?', 1)[0])
                    dest = posixpath.normpath(posixpath.join(posixpath.dirname(src), p)) if p else src
                if dest != want:
                    line = doc.line_of(m.start())
                    fails.append(f'{src}:{line}: link text `{span}` names {want} but the link points at {tgt}')
    for w in re.finditer(r'\S+', text[pos:]):
        tok = clean_word(w.group(0))
        if tok:
            judge(tok, pos + w.start(), False)
    return n_checked


# -------------------------------------------------------------- selftest --

def selftest():
    passed = 0
    failed = 0

    def check(group, name, ok, detail=''):
        nonlocal passed, failed
        if ok:
            passed += 1
        else:
            failed += 1
            print(f'  FAIL [{group}] {name} {detail}')

    # A. The anchors in tracked docs whose heading holds " — " or "_": the
    #    "--" shape the old collapsing slugger got wrong. Each (doc holding the
    #    link, target doc, anchor) must resolve against the target's headings.
    anchors = []
    for src in files:
        if not src.endswith('.md') or is_frozen(src):
            continue
        for line, tgt in targets_in(get_doc(src)):
            if SCHEME.match(tgt) or '#' not in tgt:
                continue
            path, _, frag = tgt.partition('#')
            if '--' in frag:
                dest = src if not path else posixpath.normpath(posixpath.join(posixpath.dirname(src), path))
                anchors.append((src, line, dest, frag))
    for src, line, dest, frag in anchors:
        ok = dest in fileset and frag in get_doc(dest).anchors
        check('A', f'{src}:{line} -> {dest}#{frag}', ok)
    group_a = len(anchors)
    # Floor: a tree where no "--" anchor is found means the scan above saw
    # nothing, not that every anchor is right.
    check('A', 'at least one double-dash anchor is checked', group_a > 0, '(found 0)')

    # B. The `doc:` values the console's policy-field help links to.
    group_b = 0
    if POLICY_HELP_TS in fileset and POLICY_DOC in fileset:
        with open(POLICY_HELP_TS, encoding='utf-8') as f:
            ts = f.read()
        values = re.findall(r'^\s*doc:\s*"([^"]+)"', ts, re.MULTILINE)
        check('B', f'{POLICY_HELP_TS} holds doc: values', bool(values))
        for v in values:
            check('B', f'{POLICY_DOC}#{v}', v in get_doc(POLICY_DOC).anchors)
        group_b = len(values)
    else:
        check('B', 'console help + POLICIES.md present', False,
              f'({POLICY_HELP_TS} / {POLICY_DOC} not tracked)')

    # C. A repeated heading gains -1, -2.
    sl = Slugger()
    got = [sl.slug('Dup'), sl.slug('Dup'), sl.slug('Dup'), sl.slug('Other'), sl.slug('Dup 1')]
    check('C', 'duplicate headings', got == ['dup', 'dup-1', 'dup-2', 'other', 'dup-1-1'], f'got {got}')

    # D. The slugger on its own, no repo needed.
    cases = [
        ('Retention, erasure and GDPR — a residual, not a solved problem',
         'retention-erasure-and-gdpr--a-residual-not-a-solved-problem'),
        ('`push_rules` — `PushRulesSpec`', 'push_rules--pushrulesspec'),
        ('push_rules — PushRulesSpec', 'push_rules--pushrulesspec'),
        ('The hash chain — what a rewritten row looks like',
         'the-hash-chain--what-a-rewritten-row-looks-like'),
        ('`-L` port forwarding', '-l-port-forwarding'),
        ('4. `-L` port forwarding', '4--l-port-forwarding'),
        ('Test / internal-only (not operator configuration)',
         'test--internal-only-not-operator-configuration'),
        ('**Bold** and *italic* text', 'bold-and-italic-text'),
        ('Trailing dash —', 'trailing-dash-'),
        ('Café déjà vu', 'café-déjà-vu'),
        ('HTTP/2 & TLS 1.3?', 'http2--tls-13'),
        ('A [link](x.md) here', 'a-link-here'),
        ('`_keep_` this', '_keep_-this'),
        ('x² squared ½ cup', 'x-squared--cup'),
        ('Chapter Ⅻ', 'chapter-ⅻ'),
        ('⚠️ Daemon trust', '\ufe0f-daemon-trust'),
    ]
    for text, want in cases:
        got = slug_base(text)
        check('D', f'{text!r}', got == want, f'got {got!r}, want {want!r}')
    print(f'selftest: A {group_a} double-dash anchors, B {group_b} console doc values, '
          f'C 1 duplicate-heading case, D {len(cases)} slugger cases')
    print(f'selftest: {passed}/{passed + failed} checks passed')
    return failed == 0


# ------------------------------------------------------------------ main --

def main():
    if mode == 'selftest':
        ok = selftest()
        print('doc-links selftest: ' + ('PASS' if ok else 'FAIL'))
        return 0 if ok else 1

    md_files = [p for p in files if p.endswith('.md')]
    scan = [p for p in md_files if not is_frozen(p)]
    frozen = [p for p in md_files if is_frozen(p)]

    fails = []
    n_links = n_anchors = n_external = 0
    for src in scan:
        doc = get_doc(src)
        for line, tgt in targets_in(doc):
            status, detail = resolve(src, tgt)
            if status == 'skip':
                n_external += 1
                continue
            if status == 'fail':
                fails.append(f'{src}:{line}: dead link {detail}')
                continue
            n_links += 1
            if detail:
                dest, frag = detail
                n_anchors += 1
                if frag not in get_doc(dest).anchors:
                    near = [a for a in get_doc(dest).anchors if a.lower() == frag.lower()]
                    hint = f' (did you mean #{near[0]}?)' if near else ''
                    fails.append(f'{src}:{line}: no heading anchor #{frag} in {dest}{hint}')

    frozen_dead = 0
    for src in frozen:
        for line, tgt in targets_in(get_doc(src)):
            status, detail = resolve(src, tgt)
            if status == 'fail':
                frozen_dead += 1
                print(f'  INFO frozen {src}:{line}: dead link {detail}')

    must_docs = []
    for p in listed_must + extra_must:
        if p not in must_docs:
            must_docs.append(p)
    must_fails = []
    warns = 0
    for src in scan:
        if src in must_docs or src.startswith('docs/design/'):
            continue
        doc = get_doc(src)
        warns += len(re.findall(r'`[A-Za-z0-9_./-]+\.(?:go|md|ts|sh):\d+(?:-\d+)?`', doc.text))
    for p in must_docs:
        if p.startswith('docs/design/'):
            print(f'  SKIP must-link {p} (docs/design/** is exempt)')
            continue
        if p not in fileset or not p.endswith('.md'):
            fails.append(f'{p}: named for must-link but is not a tracked .md file')
            continue
        check_must_link(p, must_fails)
    fails.extend(must_fails)

    print('=== doc-links report ===')
    print(f'{len(scan)} docs scanned ({len(frozen)} frozen, {frozen_dead} dead link(s) there, not asserted)')
    print(f'{n_links} internal links resolved, {n_anchors} with a heading anchor, {n_external} external skipped')
    print(f'{len(must_docs)} must-link doc(s); {warns} line citation(s) outside them (WARN, cite the symbol)')
    print()
    print('=== doc-links assertions ===')
    for f in fails:
        print(f'  FAIL {f}')
    if not fails:
        print(f'  ok   {n_links} links resolve; {len(must_docs)} must-link doc(s) clean')
    print()
    print('doc-links gate: ' + ('PASS' if not fails else 'FAIL'))
    return 0 if not fails else 1


sys.exit(main())
PYEOF
