#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Build a persona-viewing corpus from a finished demo take.

A persona "watches" a video as frames-in-time-order interleaved with the spoken
lines (scripts/demo-review/personas/protocol.md). Round 1 assembled these by
hand; every later round rebuilds one per retake, so it is a script now.

    scripts/demo-review/build-corpus.py VIDEO.mp4 TIMELINE.json OUTDIR

TIMELINE is the take's narration timeline. record-demo.sh writes four shapes and
which one to pass follows the picture, not the episode number:

  narration.json           browser-only take, no fast-forward. Cue times are the
                           console recording's clock, which IS the mp4's clock.
  narration-ffwd.json      browser-only take that was fast-forwarded
                           (record-demo.sh:625-651). demo-ffwd.py re-times the
                           cues onto the squeezed picture, so these are the
                           published mp4's times. Pass this, never narration.json.
  narration-terminal.json  terminal-only take (13). demo-typist.sh stamps every
                           cue against WARDYN_DEMO_CAPTURE_ZERO, the moment the
                           screen grab started — so already the mp4's clock.
  narration-joined.json    terminal + console concatenated, terminal first
                           (11/12/13 hybrids; record-demo.sh:598-607 joins the
                           picture, :686-695 the cues). The merge shifts every
                           BROWSER cue by the terminal segment's duration and
                           leaves the terminal cues alone, then muxes at
                           --offset-ms 0 — so this file, too, is absolute mp4
                           time, and it is the only correct timeline for a
                           *-full-*.mp4. Its cues are two concatenated arrays,
                           so they are sorted here before use.

Every shape therefore samples at cue["tMs"] as-is; the shapes differ only in
which file carries the truth, and passing the wrong one silently shifts the
whole corpus (the joined case is guarded below).

Writes into OUTDIR:
  transcript.txt          [  12.3s] line   (plus the video's basename header)
  timeline.json           the timeline this corpus was built from (tile-sheets.py
                          labels tiles from it; also records which shape was used)
  tNNNNN.N_cueNN.png      each cue at +0.8s (the line's on-screen moment)
  tNNNNN.N_cueNNm.png     the MIDPOINT of a cue longer than 6s — the long beats
                          are the ones whose picture changes while it is spoken
  tNNNNN.N_gapNN.png      the midpoint of every silence > 5s

Frame budget: 66 — six-up across tile-sheets.py's eleven sheets. Gap frames are never dropped; mid-cue frames thin
first, then +0.8s frames, both evenly — same policy R1 used on V01.

Extraction uses the pipeline's own ffmpeg (scripts/lib/ffmpeg.py — record-demo.sh's
ladder, Windows build first), one -ss seek per frame, written to the WSL path
directly.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path

# One resolver for the whole pipeline (record-demo.sh's ladder); this copy had
# no override at all, so a box without the winget install could not build a
# corpus even with an ffmpeg sitting on PATH. No package here, so the lib dir
# goes on the path.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "lib"))
from ffmpeg import _ffmpeg  # noqa: E402

COLS_X_SHEETS = 66  # keep in step with tile-sheets.py --per-sheet x --max-sheets


def winpath(p: Path) -> str:
    return subprocess.run(["wslpath", "-w", str(p)], capture_output=True, text=True, check=True).stdout.strip()


def main() -> int:
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    video, timeline, outdir = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
    if not video.is_file():
        sys.exit(f"no such video: {video}")
    # A joined take's picture starts with the terminal segment, so browser-lane
    # cues are late by that segment's whole duration — minutes, on 11/12. The
    # corpus would still build, and every frame would be wrong.
    if timeline.name == "narration.json" and ("-full" in video.name or "-ffwd" in video.name):
        sys.exit(f"{video.name} needs its re-timed timeline (narration-joined.json / narration-ffwd.json), not narration.json")
    cues = sorted(json.loads(timeline.read_text()).get("cues", []), key=lambda c: c["tMs"])
    if not cues:
        sys.exit(f"no cues in {timeline}")
    outdir.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(timeline, outdir / "timeline.json")

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
        # One extra frame for a long line only: a 1.5s caption is one picture,
        # a 9s one is usually two. Sampling every cue twice doubled the corpus
        # and the 60-frame cap then thinned the extras straight back out.
        if c.get("durMs", 0) > 6000:
            shots.append((t + c["durMs"] / 2000, f"cue{i:02d}m", 0))
    gaps = 0
    for i in range(1, len(cues)):
        end = (cues[i - 1]["tMs"] + cues[i - 1]["durMs"]) / 1000
        start = cues[i]["tMs"] / 1000
        if start - end > 5.0:
            shots.append(((start + end) / 2, f"gap{gaps:02d}", 2))
            gaps += 1

    cap = COLS_X_SHEETS  # 6-up x 11 sheets — tile-sheets.py's default budget
    for prio in (0, 1):  # thin mid-cue frames first, then main cue frames; never gaps
        excess = len(shots) - cap
        if excess <= 0:
            break
        pool = [s for s in shots if s[2] == prio]
        # From index 1: the opening frame is the title card, never the one to lose.
        drop = {id(s) for s in pool[1 :: max(1, len(pool) // excess)][:excess]}
        shots = [s for s in shots if id(s) not in drop]

    ff = _ffmpeg()
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
