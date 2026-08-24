#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
"""Measure how far the PICTURE has drifted from the NARRATION on a take.

    scripts/demo-drift.py --video ui/test-results/demo-video-10/console.webm \
                          --timeline ui/test-results/demo-video-10/narration.json

Every cue in narration.json is stamped at the instant overlay.ts put that
caption on screen, so the caption bubble must CHANGE at the cue's own tMs. This
finds the caption changes in the picture and reports the mapping between the two
clocks: a rate of 1.000 means the take is in sync, 1.030 means the picture runs
3% long and the narration slides progressively late.

That is not hypothetical. Take 10 (2026-08-24) shipped at rate 1.031 — the
caption for a cue landed +0.0s at t=20s and +6.4s at t=213s — because the
browser lane's picture is Playwright's recordVideo webm, timed off Chromium's
frame-swap clock (CLOCK_MONOTONIC), while narrator.ts stamped its cues off
Date.now() (CLOCK_REALTIME). Under WSL2 those two run at different RATES: this
box clocked +3.5% (90.00s monotonic per 86.95s realtime), because WSL2 keeps
slewing realtime back to the Windows host while monotonic free-runs.

Run it on the RAW picture (console.webm, or the assembled mp4 BEFORE ffwd) with
the matching pre-ffwd narration.json — demo-ffwd.py re-times both together, so
a fast-forwarded take hides nothing but measures nothing either.

--strip T dumps a readable filmstrip PNG around T so a row can be confirmed by
eye rather than trusted.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from bisect import bisect_left
from pathlib import Path

# The caption pill, in a 1920x1080 frame. overlay.ts pins .cap to
# `bottom: 44px` with 20px/1.4 text and 14px padding, so it occupies y 980-1036;
# the text is centre-aligned on x=960. A centre strip is enough to see the ink
# change and narrow enough to miss the page's own bottom furniture, which
# changes on its own and would read as a caption change.
CROP = "520:40:700:986"
SAMPLE_FPS = 10
# A cue counts as "found" when a caption change lands this close to where the
# fitted mapping puts it. Half the sample step plus slack for the 0.3s fade.
TOL_S = 0.45


def find_ffmpeg(explicit: str | None) -> str:
    """Same resolution record-demo.sh does — gdigrab needs the Windows build."""
    if explicit:
        return explicit
    from shutil import which

    if w := which("ffmpeg.exe"):
        return w
    hits = sorted(
        Path("/mnt/c/Users").glob(
            "*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe"
        )
    )
    if hits:
        return str(hits[0])
    return which("ffmpeg") or "ffmpeg"


def winpath(ffmpeg: str, p: str) -> str:
    """ffmpeg.exe cannot read /home/... — hand it a Windows path."""
    if not ffmpeg.endswith(".exe"):
        return p
    out = subprocess.run(["wslpath", "-w", p], capture_output=True, text=True)
    return out.stdout.strip() or p


def transitions(ffmpeg: str, video: str) -> list[float]:
    """Picture times (seconds) where the caption bubble's ink changed.

    ffmpeg does the arithmetic: difference each sampled frame against the one
    before it (tblend) and print its average luma (signalstats). No numpy, no
    frame dump — one pass, a few hundred KB of text.
    """
    out = subprocess.run(
        [
            ffmpeg, "-hide_banner", "-loglevel", "error", "-i", winpath(ffmpeg, video),
            "-vf",
            f"crop={CROP},fps={SAMPLE_FPS},format=gray,tblend=all_mode=difference,"
            "signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-",
            "-f", "null", "-",
        ],
        capture_output=True,
        text=True,
    ).stdout

    samples: list[tuple[float, float]] = []
    t = None
    for line in out.splitlines():
        if m := re.search(r"pts_time:([\d.]+)", line):
            t = float(m.group(1))
        elif t is not None and (m := re.search(r"YAVG=([\d.]+)", line)):
            samples.append((t, float(m.group(1))))
            t = None
    if not samples:
        return []

    # A caption swap moves far more ink than the compression noise between two
    # identical frames, so the floor is set off the take's own noise rather than
    # a tuned constant that would need re-tuning per theme.
    vals = sorted(v for _, v in samples)
    noise = vals[len(vals) // 2]
    thr = max(0.4, noise * 8)

    hits: list[float] = []
    for t, v in samples:
        if v > thr and (not hits or t - hits[-1] > 0.6):
            hits.append(t)
    return hits


def _nearest(hits: list[float], want: float) -> float:
    """Distance from `want` to the closest caption change. hits must be sorted."""
    i = bisect_left(hits, want)
    lo = want - hits[i - 1] if i else float("inf")
    hi = hits[i] - want if i < len(hits) else float("inf")
    return min(lo, hi)


def fit(cues_s: list[float], hits: list[float]) -> tuple[float, float, int]:
    """Best (rate, offset) mapping cue time -> picture time, and how many landed.

    A grid search rather than a least-squares line: the drift accumulates in
    bursts (WSL2 corrects realtime in steps), so an outlier-sensitive fit is
    pulled around by whichever burst is biggest. Counting cues that land on a
    real caption change is robust to every one of them.

    Matches first, total residual second. Matches alone are degenerate — with a
    tolerance this wide, rate can trade against offset over the take's length
    and still hit every cue — and a rate that is only approximately right is the
    one number this whole script exists to report.
    """
    hits = sorted(hits)
    best = (1.0, 0.0, -1, 0.0)
    for i in range(-60, 141):  # rate 0.970 .. 1.070
        rate = 1.0 + i * 0.0005
        for j in range(-40, 41):  # offset -2.0s .. +2.0s
            off = j * 0.05
            n = 0
            err = 0.0
            for c in cues_s:
                d = _nearest(hits, c * rate + off)
                if d <= TOL_S:
                    n += 1
                    err += d
                else:
                    err += TOL_S
            if (n, -err) > (best[2], -best[3]):
                best = (rate, off, n, err)
    return best[0], best[1], best[2]


def selftest() -> None:
    hits = [round(t * 1.03 + 0.2, 2) for t in (1, 9, 20, 44, 80, 130, 190, 212)]
    rate, _off, n = fit([1, 9, 20, 44, 80, 130, 190, 212], hits)
    assert abs(rate - 1.03) < 0.002, rate
    assert n == 8, n
    assert fit([1, 9, 20], [1.0, 9.0, 20.0])[0] == 1.0
    print("demo-drift: selftest ok")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--video")
    ap.add_argument("--timeline")
    ap.add_argument("--ffmpeg")
    ap.add_argument("--strip", type=float, help="dump a filmstrip PNG around this picture time")
    ap.add_argument("--quiet", action="store_true", help="just the verdict line")
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()

    if args.selftest:
        selftest()
        return 0
    if not args.video:
        ap.error("--video is required")

    ffmpeg = find_ffmpeg(args.ffmpeg)

    if args.strip is not None:
        out = f"{Path(args.video).stem}-strip-{args.strip:.0f}.png"
        subprocess.run(
            [
                ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
                "-ss", str(max(0.0, args.strip - 3)), "-t", "12", "-i", winpath(ffmpeg, args.video),
                "-vf", "fps=1,crop=1200:56:360:982,tile=1x12", "-frames:v", "1",
                winpath(ffmpeg, str(Path(out).absolute())),
            ],
            check=False,
        )
        print(f"demo-drift: {out} — 12 rows, 1s apart, from t={max(0.0, args.strip - 3):.0f}s")
        return 0

    if not args.timeline:
        ap.error("--timeline is required")

    cues = json.loads(Path(args.timeline).read_text()).get("cues", [])
    if len(cues) < 5:
        print("demo-drift: fewer than 5 cues — nothing to measure", file=sys.stderr)
        return 2

    hits = transitions(ffmpeg, args.video)
    if not hits:
        print("demo-drift: no caption changes found in the picture", file=sys.stderr)
        return 2

    cues_s = [c["tMs"] / 1000 for c in cues]
    rate, off, matched = fit(cues_s, hits)

    if not args.quiet:
        print(f"  {'cue':>9}  {'picture':>9}  {'lag':>7}   text")
    worst = 0.0
    for c, txt in zip(cues_s, (c["text"] for c in cues)):
        want = c * rate + off
        near = [h for h in hits if abs(h - want) <= TOL_S]
        if not near:
            if not args.quiet:
                print(f"  {c:9.2f}  {'—':>9}  {'':>7}   {txt[:46]}")
            continue
        got = near[0]
        worst = max(worst, abs(got - c))
        if not args.quiet:
            print(f"  {c:9.2f}  {got:9.2f}  {got - c:+7.2f}   {txt[:46]}")

    pct = (rate - 1) * 100
    print(
        f"{'' if args.quiet else chr(10)}  rate {rate:.4f} ({pct:+.2f}%)  offset {off:+.2f}s  "
        f"matched {matched}/{len(cues_s)} cues  worst lag {worst:.2f}s"
    )
    if abs(pct) > 1.0:
        print(
            f"  DRIFT: the picture runs {pct:+.2f}% against the cue clock. The narration slides\n"
            f"  {worst:.1f}s late by the end of this take. See nowMs() in ui/e2e/demo/narrator.ts.",
            file=sys.stderr,
        )
        return 1
    print("  in sync (within 1%)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
