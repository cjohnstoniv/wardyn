#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Render every caption the recorder is likely to speak, BEFORE a take.
#
# Without this the first render of each line happens inline during the
# recording: the caption sits on screen for the ~1.5s the TTS takes, which is
# dead air in the finished video, once per line. Warmed, every lookup is a
# cache hit and the pacing is the pacing the driver intended.
#
# Extraction is deliberately best-effort — it greps double-quoted strings out of
# the driver and its data file. A missed line is harmless (it renders inline, as
# it would have anyway) and a spurious line just warms a clip nobody plays.
# Template literals carrying ${...} are skipped: their text is not known until
# the take runs.
#
#   scripts/narrate-prewarm.sh

set -uo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY="${WARDYN_NARRATE_PY:-${HOME}/.cache/wardyn-narrate/venv/bin/python}"
SERVER="${REPO_ROOT}/scripts/narrate-server.py"

[[ -x "${PY}" ]] || { echo "prewarm: no renderer at ${PY} — skipping"; exit 0; }

# Every string literal long enough to be a spoken line, from every demo spec and
# helper. The whole directory rather than two named files: the 0.5 series is ten
# specs, and a per-video file left off this list is a take that stops to render
# each of its own lines — exactly the dead air this script exists to remove.
# `sort -u` because act()/caption() repeat some copy verbatim.
mapfile -t LINES < <(
  grep -ohE '"[A-Za-z][^"]{24,}"' "${REPO_ROOT}"/ui/e2e/demo/*.ts 2>/dev/null \
  | sed 's/^"//; s/"$//' | sort -u
)

# Act 3 speaks `${demo.label} — ${demo.caption}` per funnel demo, which no
# literal grep can see. Reconstruct those five joined lines so Act 3 does not
# stop to render one before each demo.
mapfile -t JOINED < <(
  python3 - "${REPO_ROOT}/ui/e2e/demo/task.ts" <<'PYEOF'
import re, sys
src = open(sys.argv[1]).read()
block = re.search(r"export const FUNNEL_DEMOS.*?\n\] as const", src, re.S)
if block:
    for label, caption in re.findall(
        r'label:\s*"((?:[^"\\]|\\.)*)".*?caption:\s*"((?:[^"\\]|\\.)*)"',
        block.group(0), re.S):
        # Match the driver's own join, em-dash included.
        print(f"{label} \u2014 {caption}".replace('\\"', '"'))
PYEOF
)
LINES+=("${JOINED[@]}")

# The TERMINAL lane's lines live in the beat scripts, not in a spec, and the
# cache key is the spoken TEXT — so a beats line that no spec repeats renders
# inline mid-take, once per line, on camera (the 2026-08-24 pronunciation fix
# re-keyed every line it touched). Same best-effort rule as above: the typist's
# say "…" and chapter "Title" "Sub" — spoken as ONE line "Title. Sub."
# (demo-typist.sh's own join) — with anything carrying a ${…} expansion skipped,
# since its text is not known until the take runs.
mapfile -t BEATS < <(
  python3 - "${REPO_ROOT}"/scripts/demo-beats/*.sh <<'PYEOF'
import re, sys
for path in sys.argv[1:]:
    try: src = open(path).read()
    except OSError: continue
    for m in re.finditer(r'^\s*(say|chapter)\s+"([^"\\]*)"(?:\s+"([^"\\]*)")?', src, re.M):
        kind, a, b = m.groups()
        line = f"{a}. {b}." if kind == "chapter" and b else a
        if line and "${" not in line:
            print(line)
PYEOF
)
LINES+=("${BEATS[@]}")

if [[ "${#LINES[@]}" -eq 0 ]]; then
  echo "prewarm: found no caption text — skipping"
  exit 0
fi

echo "prewarm: rendering ${#LINES[@]} lines…"
# One process, one model load, N renders — the whole reason the renderer is a
# server rather than a CLI.
printf '%s\n' "${LINES[@]}" \
  | python3 -c 'import json,sys
for line in sys.stdin:
    line = line.strip()
    if line:
        print(json.dumps({"text": line}))' \
  | "${PY}" "${SERVER}" 2>/dev/null \
  | python3 -c 'import json,sys
hit = miss = err = 0
for line in sys.stdin:
    try: r = json.loads(line)
    except Exception: continue
    if r.get("error"): err += 1
    elif r.get("cached"): hit += 1
    else: miss += 1
print(f"prewarm: {miss} rendered, {hit} already cached, {err} failed")'
