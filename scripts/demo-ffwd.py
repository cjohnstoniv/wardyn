#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Squeeze the dead air out of a demo take.

    scripts/demo-ffwd.py --video console.mp4 --spans speedups.json \
                         --timeline narration.json --out ffwd.mp4 \
                         --timeline-out narration-ffwd.json \
                         [--factor 12] [--ffmpeg /path/to/ffmpeg.exe]

overlay.ts's ffwdStart/ffwdEnd write the spans where the video is nothing but
the agent working. This compresses each of those stretches by --factor and
slides every narration cue after it up by exactly the time removed, so the
voice still lands on the frame it describes.

Video only, deliberately: the input at this point in record-demo.sh is a silent
mp4 and narrate-mux.py runs AFTER — which is what lets the cues be re-timed
here instead of being stretched with the picture.

Times are milliseconds from the take's zero, the same basis narration.json's
cue tMs use (narrator.ts's narrationZero, set by installOverlay).
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
from pathlib import Path

Span = tuple[int, int]


# Same resolution as narrate-mux.py: gdigrab is a Windows device, so the take is
# encoded by the Windows ffmpeg, which winget hides on the WINDOWS PATH.
def find_ffmpeg(explicit: str | None) -> str:
    if explicit:
        return explicit
    which = shutil.which("ffmpeg.exe") or shutil.which("ffmpeg")
    if which:
        return which
    for p in Path("/mnt/c/Users").glob(
        "*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe"
    ):
        return str(p)
    raise SystemExit("demo-ffwd: no ffmpeg found (pass --ffmpeg)")


def winpath(ffmpeg: str, p: str) -> str:
    """ffmpeg.exe cannot read /home/... — hand it a Windows path."""
    if not ffmpeg.endswith(".exe"):
        return p
    out = subprocess.run(["wslpath", "-w", p], capture_output=True, text=True)
    return out.stdout.strip() or p


def duration_ms(ffmpeg: str, video: str) -> int:
    """Probe the picture's length off `ffmpeg -i` stderr.

    Same parse record-demo.sh does for the join offset, and for its reason: the
    Windows ffmpeg package does not always ship ffprobe.
    """
    err = subprocess.run(
        [ffmpeg, "-hide_banner", "-i", winpath(ffmpeg, video)], capture_output=True, text=True
    ).stderr
    for line in err.splitlines():
        if "Duration:" in line:
            h, m, rest = line.split("Duration:")[1].split(",")[0].strip().split(":")
            return int((int(h) * 3600 + int(m) * 60 + float(rest)) * 1000)
    return 0


def usable_spans(raw: list[dict], total_ms: int) -> list[Span]:
    """Sort, clamp to the real picture, and drop what cannot be compressed."""
    spans: list[Span] = []
    end_of_last = 0
    for s in sorted(raw, key=lambda s: int(s["startMs"])):
        start, end = int(s["startMs"]), int(s["endMs"])
        if start >= total_ms:
            print(f"demo-ffwd: WARNING span {start}-{end}ms is past the video — dropped", file=sys.stderr)
            continue
        start, end = max(start, end_of_last), min(end, total_ms)
        if end - start < 1000:  # nothing worth a re-encode, and 0-length breaks trim
            print(f"demo-ffwd: WARNING span {start}-{end}ms is too short — dropped", file=sys.stderr)
            continue
        spans.append((start, end))
        end_of_last = end
    return spans


def shift_cues(cues: list[dict], spans: list[Span], factor: float) -> list[dict]:
    """Re-time cues onto the compressed picture. Spans sorted, non-overlapping."""
    for c in cues:
        t = out = int(c["tMs"])
        for start, end in spans:
            if t >= end:
                out -= int((end - start) * (1 - 1 / factor))
            elif t > start:
                # Inside a span. The spec says nothing here — but if it ever
                # does, park the line on the span's first frame rather than
                # letting it drift into the compressed stretch.
                out -= t - start
                break
            else:
                break
        c["tMs"] = out
    return cues


def selftest() -> int:
    spans = [(1_000, 13_000), (20_000, 32_000)]  # 12s each; at 12x each loses 11s
    cues = [{"tMs": t} for t in (500, 5_000, 13_000, 15_000, 25_000, 40_000)]
    got = [c["tMs"] for c in shift_cues(cues, spans, 12)]
    assert got == [500, 1_000, 2_000, 4_000, 9_000, 18_000], got
    assert usable_spans([{"startMs": 0, "endMs": 9_000}], 5_000) == [(0, 5_000)]
    assert usable_spans([{"startMs": 9_000, "endMs": 12_000}], 5_000) == []
    print("demo-ffwd: selftest ok")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--video")
    ap.add_argument("--spans")
    ap.add_argument("--timeline")
    ap.add_argument("--out")
    ap.add_argument("--timeline-out")
    ap.add_argument("--factor", type=float, default=12)
    ap.add_argument("--ffmpeg")
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()
    if args.selftest:
        return selftest()
    for req in ("video", "spans", "out"):
        if not getattr(args, req):
            ap.error(f"--{req} is required")
    if args.factor < 1:
        ap.error("--factor must be >= 1")

    ffmpeg = find_ffmpeg(args.ffmpeg)
    total = duration_ms(ffmpeg, args.video)
    if not total:
        print(f"demo-ffwd: cannot read a duration from {args.video}", file=sys.stderr)
        return 1
    spans = usable_spans(json.loads(Path(args.spans).read_text()).get("spans", []), total)
    if not spans:
        print("demo-ffwd: no usable spans — leaving the take in real time", file=sys.stderr)
        return 2

    # Alternating pass-through / compressed segments, concatenated. trim wants
    # seconds; the last segment has no end so a take that runs past the final
    # span keeps its tail.
    parts, cuts, at = [], [], 0
    for start, end in spans:
        if start > at:
            cuts.append((at / 1000, start / 1000, 1.0))
        cuts.append((start / 1000, end / 1000, args.factor))
        at = end
    cuts.append((at / 1000, None, 1.0))
    for i, (a, b, f) in enumerate(cuts):
        trim = f"trim=start={a:.3f}" + ("" if b is None else f":end={b:.3f}")
        parts.append(f"[0:v]{trim},setpts=(PTS-STARTPTS)/{f}[v{i}]")
    parts.append("".join(f"[v{i}]" for i in range(len(cuts))) + f"concat=n={len(cuts)}:v=1:a=0[v]")

    cmd = [
        ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
        "-i", winpath(ffmpeg, args.video),
        "-filter_complex", ";".join(parts),
        "-map", "[v]",
        # Matches the assembly encode in record-demo.sh — this is the last time
        # the picture is touched before narration is stream-copied on top.
        "-c:v", "libx264", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p",
        winpath(ffmpeg, args.out),
    ]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print(res.stderr.strip()[-800:], file=sys.stderr)
        return 1

    removed = sum(int((e - s) * (1 - 1 / args.factor)) for s, e in spans)
    print(f"demo-ffwd: {len(spans)} span(s) at {args.factor:g}x -> {removed / 1000:.1f}s removed")

    # A --silent take has no timeline at all; compressing its picture is still
    # worth doing, so a missing one is not an error.
    if args.timeline and args.timeline_out and Path(args.timeline).is_file():
        tl = json.loads(Path(args.timeline).read_text())
        tl["cues"] = shift_cues(tl.get("cues", []), spans, args.factor)
        Path(args.timeline_out).write_text(json.dumps(tl, indent=2))
        print(f"demo-ffwd: {len(tl['cues'])} cue(s) re-timed -> {args.timeline_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
