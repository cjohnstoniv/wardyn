#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Rewrite a take's cue clock onto its picture clock.

    scripts/demo-clockfix.py --fit drift-fit.json --timeline narration.json \
                             [--spans speedups.json]

Playwright's screencast timebase and the monotonic cue clock disagree by a
small per-take rate (demo-drift.py measures and fits it). Everything downstream
consumes cue times — the ffwd cutter slices the VIDEO at span times, the mux
lays audio at cue times — so the one correct place to reconcile the clocks is
here, once, before either consumer runs: map every cue tMs and every span
start/end through the fitted rate/offset. After this, video, spans and cues
share one clock; verify-demo-take's drift gate then asserts the RESULT
(webm vs corrected cues ≈ rate 1.000) instead of gating on the raw mismatch.

Idempotent by refusal: a corrected file carries "clockfixed": true and is not
mapped twice. Originals are kept beside as *-raw.json (refreshed on every
mapping — a work dir outlives its takes). The
fit file is stamped "applied": true on success — verify's proof for takes.
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path


def remap(ms: int, rate: float, off_ms: float) -> int:
    return max(0, int(ms * rate + off_ms))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--fit", required=True)
    ap.add_argument("--timeline", required=True)
    ap.add_argument("--spans")
    args = ap.parse_args()

    fit = json.loads(Path(args.fit).read_text())
    if not fit.get("good_fit"):
        print("demo-clockfix: fit marked unreliable — refusing to map on a guess", file=sys.stderr)
        return 1
    rate, off_ms = fit["rate"], fit["offset_s"] * 1000

    for path, kind in ((args.timeline, "timeline"), (args.spans, "spans")):
        if not path or not Path(path).exists():
            continue
        p = Path(path)
        doc = json.loads(p.read_text())
        if doc.get("clockfixed"):
            print(f"demo-clockfix: {p.name} already on the picture clock — skipped", file=sys.stderr)
            continue
        # We only get here when the doc is NOT yet on the picture clock, i.e. it
        # IS the raw one — so the backup is refreshed every time. Keeping the
        # first one left take 1's cues beside take 2's picture (03b, 2026-09-08).
        shutil.copy2(p, p.with_name(p.stem + "-raw.json"))
        if kind == "timeline":
            for c in doc.get("cues", []):
                c["tMs"] = remap(c["tMs"], rate, off_ms)
        else:
            for s in doc.get("spans", []):
                s["startMs"] = remap(s["startMs"], rate, off_ms)
                s["endMs"] = remap(s["endMs"], rate, off_ms)
        doc["clockfixed"] = True
        p.write_text(json.dumps(doc, indent=1) + "\n")
        print(f"demo-clockfix: {p.name} -> picture clock (rate {rate} offset {fit['offset_s']}s)")

    fit["applied"] = True
    Path(args.fit).write_text(json.dumps(fit, indent=1) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
