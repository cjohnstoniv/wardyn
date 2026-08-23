#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Build a persona-viewing corpus from a finished demo take.

A persona "watches" a video as frames-in-time-order interleaved with the spoken
lines (scripts/demo-review/personas/protocol.md). Round 1 assembled these by
hand; every later round rebuilds one per retake, so it is a script now.

    scripts/demo-review/build-corpus.py VIDEO.mp4 TIMELINE.json OUTDIR

TIMELINE is the take's narration timeline — narration-ffwd.json when the take
was fast-forwarded (the published picture's clock), else narration.json.

Writes into OUTDIR:
  transcript.txt          [  12.3s] line   (plus the video's basename header)
  tNNNNN.N_cueNN.png      each cue at +0.8s (the line's on-screen moment)
  tNNNNN.N_cueNNb.png     each cue at +2.8s (what the moment did)
  tNNNNN.N_gapNN.png      the midpoint of every silence > 5s

Frame budget: 60 (R1's cap). Gap frames are never dropped; +2.8s frames thin
first, then +0.8s frames, both evenly — same policy R1 used on V01.

Extraction uses the Windows ffmpeg (this box has no Linux one — same resolve
as record-demo.sh), one -ss seek per frame, written to the WSL path directly.
"""

from __future__ import annotations

import glob
import json
import shutil
import subprocess
import sys
from pathlib import Path


def ffmpeg_exe() -> str:
    p = shutil.which("ffmpeg.exe")
    if p:
        return p
    for c in glob.glob("/mnt/c/Users/*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe"):
        return c
    sys.exit("no ffmpeg.exe found (winget.exe install Gyan.FFmpeg)")


def winpath(p: Path) -> str:
    return subprocess.run(["wslpath", "-w", str(p)], capture_output=True, text=True, check=True).stdout.strip()


def main() -> int:
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    video, timeline, outdir = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
    if not video.is_file():
        sys.exit(f"no such video: {video}")
    cues = json.loads(timeline.read_text()).get("cues", [])
    if not cues:
        sys.exit(f"no cues in {timeline}")
    outdir.mkdir(parents=True, exist_ok=True)

    # Transcript — the persona's "what the narrator says at that moment".
    with (outdir / "transcript.txt").open("w") as f:
        f.write(f"# {video.name}\n")
        for c in cues:
            f.write(f"[{c['tMs'] / 1000:7.1f}s] {c['text']}\n")

    # The shot list: (t_seconds, name, priority). Lower priority thins first.
    shots: list[tuple[float, str, int]] = []
    for i, c in enumerate(cues):
        t = c["tMs"] / 1000
        shots.append((t + 0.8, f"cue{i:02d}", 1))
        shots.append((t + 2.8, f"cue{i:02d}b", 0))
    gaps = 0
    for i in range(1, len(cues)):
        end = (cues[i - 1]["tMs"] + cues[i - 1]["durMs"]) / 1000
        start = cues[i]["tMs"] / 1000
        if start - end > 5.0:
            shots.append(((start + end) / 2, f"gap{gaps:02d}", 2))
            gaps += 1

    cap = 60
    for prio in (0, 1):  # thin b-frames first, then main cue frames; never gaps
        excess = len(shots) - cap
        if excess <= 0:
            break
        pool = [s for s in shots if s[2] == prio]
        drop = {id(s) for s in pool[:: max(1, len(pool) // excess)][:excess]}
        shots = [s for s in shots if id(s) not in drop]

    ff = ffmpeg_exe()
    vwin = winpath(video)
    kept = 0
    for t, name, _ in sorted(shots):
        out = outdir / f"t{t:07.1f}_{name}.png"
        r = subprocess.run(
            [ff, "-hide_banner", "-loglevel", "error", "-y", "-ss", f"{t:.2f}", "-i", vwin,
             "-frames:v", "1", winpath(out)],
            capture_output=True, text=True,
        )
        if r.returncode == 0 and out.is_file() and out.stat().st_size > 0:
            kept += 1
        else:
            print(f"  frame {name}@{t:.1f}s failed: {r.stderr.strip().splitlines()[-1] if r.stderr.strip() else 'no output'}")
    print(f"{outdir}: {len(cues)} cues, {gaps} gaps → {kept} frames + transcript.txt")
    return 0


if __name__ == "__main__":
    sys.exit(main())
