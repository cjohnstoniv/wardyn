#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-doc-links.sh — check-doc-links.sh still SEES every shape of broken link
# it claims to catch, still passes every shape it must accept, and its
# slugger self-test still bites, in a throwaway git repo.
#
# Why this exists: the gate's link scanner and GitHub slugger live in an
# embedded Python parser with no test of its own. A parser change that stops
# seeing wrapped links, or a slugger that collapses "--" again, leaves every
# real doc green while the gate catches nothing — a gate that stops detecting
# is worse than no gate, so this pins BOTH directions: shapes that must FAIL
# (with the message that names them) and shapes that must stay OK.
#
# Daemon-free, network-free: it runs the real gate against a throwaway repo.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts/doc-form.d"
cp "$ROOT/scripts/check-doc-links.sh" "$TMP/scripts/"
(cd "$TMP" && git init -q && git config user.email t@example.invalid && git config user.name t)

fail() { echo "FAIL: $*" >&2; exit 1; }

# The gate resolves its own repo root from its location, so running the COPY
# scans $TMP and nothing else. Only TRACKED files count: stage everything but
# the files named in $UNTRACKED (one path per line), which stay out of the
# index on purpose.
UNTRACKED=""
stage() {
  (cd "$TMP" && git rm -q -r --cached . >/dev/null 2>&1 || true
   git add -A
   for u in $UNTRACKED; do git rm -q --cached "$u" >/dev/null 2>&1 || true; done)
}
gate() { stage; (cd "$TMP" && ./scripts/check-doc-links.sh "$@" 2>&1); }
# run_gate — status only; gate_says — output regardless of status. Failing
# cases assert on the MESSAGE, not just the status: a crash would satisfy a
# bare non-zero assertion while detecting nothing.
run_gate() { gate "$@" >/dev/null; }
gate_says() { gate "$@" || true; }

reset_tree() {
  find "$TMP" -mindepth 1 -maxdepth 1 ! -name .git ! -name scripts -exec rm -rf {} +
  rm -f "$TMP"/scripts/doc-form.d/*
  UNTRACKED=""
  mkdir -p "$TMP/docs"
}

# ok NAME — the tree must pass; bad NAME FRAGMENT — it must fail naming FRAGMENT.
ok() {
  run_gate || fail "$1 must PASS: $(gate_says)"
  echo "ok  $1 passes"
}
bad() {
  local out
  if run_gate; then fail "$1 must FAIL"; fi
  out="$(gate_says)"
  grep -qF -- "$2" <<<"$out" || fail "$1 failed without naming it ($2): $out"
  echo "ok  $1 fails"
}

# 1. Links that must resolve: a file, a dir, a heading with " — " (the "--"
#    slug), a repeated heading (-1), a same-doc anchor, a wrapped link text,
#    an image, a link in a table, and a code file's fragment (never checked).
reset_tree
mkdir -p "$TMP/docs/sub" "$TMP/internal"
echo 'package x' > "$TMP/internal/x.go"
echo '# S' > "$TMP/docs/sub/file.md"
cat > "$TMP/docs/b.md" <<'DOC'
# B

## Alpha — beta

## Same

## Same

## `push_rules` — `PushRulesSpec`

<a id="custom-anchor"></a>
DOC
cat > "$TMP/docs/a.md" <<'DOC'
# A

See [b](b.md#alpha--beta), [dup](b.md#same-1), [code](b.md#push_rules--pushrulesspec),
[html](b.md#custom-anchor), [self](#a), [dir](sub/), [go](../internal/x.go#L10),
[wrapped
text](b.md#same), ![img](b.md), and
| col | [cell](b.md#same) |
| --- | --- |
DOC
ok "links, \`--\` slugs, duplicates and wrapped text"

# 2. Shapes that are NOT links: fenced code, inline code, comments, external.
cat >> "$TMP/docs/a.md" <<'DOC'

```
[fenced](missing.md)
```

Inline `[code](missing.md)`, <!-- [comment](missing.md) -->
[web](https://example.invalid/x.md#y) and [mail](mailto:a@example.invalid).
DOC
ok "fenced code, code spans, comments and external links are not links"

# 3. Broken links, each named.
echo '[x](missing.md)' > "$TMP/docs/dead.md"
bad "a dead file link" "dead link missing.md"
echo '[x](b.md#nope)' > "$TMP/docs/dead.md"
bad "a dead anchor" "no heading anchor #nope in docs/b.md"
echo '[x](b.md#alpha-beta)' > "$TMP/docs/dead.md"
bad "a collapsed-dash anchor (alpha-beta for 'Alpha — beta')" "no heading anchor #alpha-beta"
echo '[x](b.md#Alpha--beta)' > "$TMP/docs/dead.md"
bad "an anchor in the wrong case" "did you mean #alpha--beta"
echo '[x](b.md#same-2)' > "$TMP/docs/dead.md"
bad "a duplicate-heading suffix past the last repeat" "no heading anchor #same-2"
echo '[x](../../outside.md)' > "$TMP/docs/dead.md"
bad "a link that leaves the repository" "leaves the repository"
printf '[ref]: gone.md\n\n[x][ref]\n' > "$TMP/docs/dead.md"
bad "a dead reference definition" "dead link gone.md"
echo '<a href="gone.md">x</a>' > "$TMP/docs/dead.md"
bad "a dead HTML href" "dead link gone.md"
printf '[across\nlines](gone.md)\n' > "$TMP/docs/dead.md"
bad "a dead link whose text wraps across lines" "dead link gone.md"
rm "$TMP/docs/dead.md"

# 4. An untracked file never counts, even though it exists on disk.
echo '# U' > "$TMP/docs/untracked.md"
echo '[u](untracked.md)' > "$TMP/docs/uses.md"
UNTRACKED="docs/untracked.md"
bad "a link to an untracked file" "exists but is not tracked"
UNTRACKED=""
ok "the same link once the file is tracked"
rm "$TMP/docs/uses.md" "$TMP/docs/untracked.md"

# 5. Frozen files are history: a dead link there is shown, never failed.
echo '[gone](docs/MEMBERS.md)' > "$TMP/CHANGELOG.md"
ok "a dead link in a frozen file"
grep -qF "INFO frozen CHANGELOG.md:1" <<<"$(gate_says)" || fail "the frozen dead link must still be shown"
echo "ok  the frozen dead link is shown, not failed"
rm "$TMP/CHANGELOG.md"

# 6. Must-link: only for docs on a .links list (or --must-link).
mkdir -p "$TMP/docs/design"
echo 'x' > "$TMP/docs/design/frozen.md"
cat > "$TMP/docs/m.md" <<'DOC'
# M

Run `/etc/wardyn` and see [`docs/b.md`](b.md) and [the file](b.md).
DOC
ok "a doc not on the must-link list is not held to it"
echo "docs/m.md" > "$TMP/scripts/doc-form.d/t.links"
ok "a must-link doc with only links, runtime paths and a matching link text"

printf '# M\n\nSee `docs/b.md` for more.\n' > "$TMP/docs/m.md"
bad "a backticked file name that is not a link" "span \`docs/b.md\` names tracked docs/b.md"
printf '# M\n\nSee docs/b.md for more.\n' > "$TMP/docs/m.md"
bad "a bare file name that is not a link" "word \`docs/b.md\` names tracked docs/b.md"
printf '# M\n\nSee `docs/sub/file.md` here.\n' > "$TMP/docs/m.md"
bad "a repo-relative file in a subdirectory" "names tracked docs/sub/file.md"
printf '# M\n\nSee `sub/file.md` here.\n' > "$TMP/docs/m.md"
bad "a file named relative to the doc" "names tracked docs/sub/file.md"
echo 'all:' > "$TMP/docs/sub/Makefile"
printf '# M\n\nSee `docs/sub/Makefile` here.\n' > "$TMP/docs/m.md"
bad "a Makefile with a directory" "names tracked docs/sub/Makefile"
# Not references (STYLE 4.2: files only): the doc's own path, directories
# (trailing '/', or a token that resolves to one), a name with no '/', a lone
# '/' between two words. Every one of these names a tracked thing here.
echo 'x' > "$TMP/Makefile"
printf '%s\n' '# M' '' \
  'This page is `docs/m.md`. Sources live in `internal/` and under internal/ (bare word).' \
  'Also `docs/`, `docs/sub`, `docs/sub/` and the file `Makefile`; one / two; `x.go`.' > "$TMP/docs/m.md"
ok "own path, directories, names without a '/', a lone '/'"
rm "$TMP/Makefile" "$TMP/docs/sub/Makefile"
printf '# M\n\nSee `internal/x.go:12` here.\n' > "$TMP/docs/m.md"
bad "a line citation" "line citation"
printf '# M\n\nSee [`docs/a.md`](b.md) here.\n' > "$TMP/docs/m.md"
bad "link text naming one file while pointing at another" "link text \`docs/a.md\` names docs/a.md"
printf '# M\n\n## The `docs/b.md` file\n\n```\ndocs/b.md\n```\n' > "$TMP/docs/m.md"
ok "headings and fenced code are skipped"
printf '# M\n\nSee `docs/design/frozen.md` and `docs/nothing.md`.\n' > "$TMP/docs/m.md"
bad "a name that resolves is flagged even beside one that does not" "docs/design/frozen.md"
printf '# M\n\nSee `docs/nothing.md`.\n' > "$TMP/docs/m.md"
ok "a span that resolves to nothing is a runtime path, not a reference"
echo "docs/design/frozen.md" > "$TMP/scripts/doc-form.d/t.links"
printf '# F\n\nSee `docs/b.md` here.\n' > "$TMP/docs/design/frozen.md"
ok "docs/design/** is exempt even when listed"
rm "$TMP/scripts/doc-form.d/t.links"
printf '# M\n\nSee `docs/b.md` and `internal/x.go:3`.\n' > "$TMP/docs/m.md"
if run_gate --must-link docs/m.md; then fail "--must-link must FAIL on a doc with a bare reference"; fi
out="$(gate_says --must-link docs/m.md)"
grep -qF "names tracked docs/b.md" <<<"$out" || fail "--must-link must name the reference: $out"
echo "ok  --must-link applies the rule to a named doc"

# 7. The slugger self-test bites: a fake console help file and policy doc
#    pass it, and the same tree fails it once the slugger collapses "--".
reset_tree
mkdir -p "$TMP/ui/src/app/components/wardyn"
cat > "$TMP/docs/POLICIES.md" <<'DOC'
# Policies

## `push_rules` — `PushRulesSpec`

## Top-level
DOC
cat > "$TMP/ui/src/app/components/wardyn/policy-field-help.ts" <<'DOC'
export const FIELD_HELP = [
  {
    doc: "push_rules--pushrulesspec",
  },
  {
    doc: "top-level",
  },
];
DOC
stage
(cd "$TMP" && ./scripts/check-doc-links.sh --selftest >/dev/null 2>&1) || fail "the self-test must PASS on a correct slugger: $(cd "$TMP" && ./scripts/check-doc-links.sh --selftest 2>&1 | head -5)"
echo "ok  the self-test passes with a GitHub slugger"
sed -i "s/    return ''.join(keep)\$/    return ''.join(keep).replace('--', '-')/" "$TMP/scripts/check-doc-links.sh"
if (cd "$TMP" && ./scripts/check-doc-links.sh --selftest >/dev/null 2>&1); then
  fail "the self-test must FAIL on a slugger that collapses '--'"
fi
echo "ok  the self-test fails on a slugger that collapses '--'"
cp "$ROOT/scripts/check-doc-links.sh" "$TMP/scripts/"
printf '  {\n    doc: "no-such-heading",\n  },\n' >> "$TMP/ui/src/app/components/wardyn/policy-field-help.ts"
if (cd "$TMP" && ./scripts/check-doc-links.sh --selftest >/dev/null 2>&1); then
  fail "the self-test must FAIL on a console doc: value with no heading"
fi
echo "ok  the self-test fails on a console doc: value with no heading"

echo "doc-links tests: PASS"
