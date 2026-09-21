#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Re-apply the repo's overrides to the staged design-sync library.
#
# `.ds-sync/` is staged by the /design-sync skill and is not tracked, so every
# re-stage returns the library to its stock form. Wardyn is dark-first: the
# theme provider applies `.dark` in a post-paint effect, so preview cards
# capture light unless the card template carries the class itself. Without this
# the design system ships every component on a white ground.
#
# Scope is the two card templates only — the ones that emit an `@dsCard`
# marker. The review contact sheet further down the file is a local index whose
# figure chrome is hardcoded light; darkening its shell alone leaves pale
# captions on a dark ground, so it is deliberately left stock.
#
# Idempotent. Run after staging, before building the bundle.
set -euo pipefail

EMIT="${1:-.ds-sync/lib/emit.mjs}"
[ -f "$EMIT" ] || { echo "design-sync: $EMIT not found — stage .ds-sync first" >&2; exit 1; }

python3 - "$EMIT" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()

# Each card template opens with its @dsCard marker, then the doctype, then the
# <html> element. Anchor on the marker so the review sheet is never matched.
card = re.compile(r'(@dsCard group=[^\n]*\n<!doctype html>\n<html)(?!\s+class="dark")>')
s, n_html = card.subn(r'\1 class="dark">', s)

# The card body paints on the theme's own ground, with literal fallbacks for the
# frame rendered before the stylesheet resolves.
body = re.compile(r'body\{margin:0;padding:24px;background:#fff\}')
s, n_body = body.subn(
    'body{margin:0;padding:24px;background:var(--background,#0a0a0a);color:var(--foreground,#e5e5e5)}', s)

open(p, "w").write(s)
total = s.count('class="dark"')
if total != 2:
    sys.exit(f"design-sync: expected exactly 2 dark card templates, found {total} — "
             "the library's shape changed; re-derive the edit before syncing")
print(f"design-sync: card templates dark (newly patched this run: {n_html} html, {n_body} body)")
PY
