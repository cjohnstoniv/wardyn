#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Lay the narration timeline onto the recorded video.

    scripts/narrate-mux.py --video console.mp4 --timeline narration.json \
                           --out narrated.mp4 [--ffmpeg /path/to/ffmpeg.exe] \
                           [--offset-ms N]

Each cue is delayed to its own timestamp and the whole set is mixed into one
track. Video is stream-copied — this never re-encodes the picture, so it cannot
soften the text the framing work was done to keep sharp.

--offset-ms shifts every cue, for the `--with-terminal` case where the console
recording is concatenated AFTER a screen-grabbed Act 0 and every narration
timestamp is therefore late by Act 0's duration.

Built as a script rather than shell string-building because a full take is ~50
cues: that is ~50 inputs and a ~100-clause filtergraph, which is unreadable and
unquotable in bash.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

# One resolver for the whole pipeline (record-demo.sh's ladder); this copy used
# to prefer a plain `ffmpeg` on PATH over the Windows build the take was encoded
# with. No package here, so the sibling dir goes on the path.
sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
from ffmpeg import _ffmpeg  # noqa: E402


def winpath(ffmpeg: str, p: str) -> str:
    """ffmpeg.exe cannot read /home/... — hand it a Windows path."""
    if not ffmpeg.endswith(".exe"):
        return p
    out = subprocess.run(["wslpath", "-w", p], capture_output=True, text=True)
    return out.stdout.strip() or p


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--video", required=True)
    ap.add_argument("--timeline", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--ffmpeg")
    ap.add_argument("--offset-ms", type=int, default=0)
    ap.add_argument("--drift-fit", help="demo-drift.py --emit-fit JSON: lay cues on the picture's own clock")
    args = ap.parse_args()

    ffmpeg = _ffmpeg(args.ffmpeg)
    cues = json.loads(Path(args.timeline).read_text()).get("cues", [])
    cues = [c for c in cues if Path(c["file"]).exists()]
    if not cues:
        print("narrate-mux: no cues — leaving the video silent", file=sys.stderr)
        return 2

    # The picture's clock and the cue clock disagree by a small, fitted rate
    # (Playwright's screencast timebase vs the monotonic cue stamps — measured
    # per take by demo-drift.py). Laying cues at raw tMs leaves the narration
    # sliding late by rate*t; mapping every cue through the fit puts each line
    # where its caption actually changes on screen. Applied only when the fit
    # is reliable — a guessed correction is worse than the drift.
    fit = None
    if args.drift_fit and Path(args.drift_fit).exists():
        fit = json.loads(Path(args.drift_fit).read_text())
        if fit.get("good_fit"):
            r, o = fit["rate"], fit["offset_s"] * 1000
            for c in cues:
                c["tMs"] = max(0, int(c["tMs"] * r + o))
            print(f"narrate-mux: cues mapped to the picture's clock (rate {r} offset {fit['offset_s']}s)", file=sys.stderr)
        else:
            print("narrate-mux: drift fit marked unreliable — laying cues uncorrected", file=sys.stderr)
            fit = None

    # Two voices at once is the one defect every listener notices. A SMALL
    # collision (the V01 class: a consistent few-hundred-ms accounting bias
    # between the spec's awaited duration and the clip the mux actually lays
    # down) is resolved here, at the layer that owns physical realizability:
    # slide the colliding cue to just after the previous clip. The on-screen
    # caption led its audio by that same sub-second sliver — invisible. A BIG
    # collision stays a loud warning and an unshifted cue: that is a directing
    # bug (a line genuinely cut off on screen) the verifier must keep failing,
    # not something to quietly re-time into a different video.
    CLAMP_MS = 1500
    GAP_MS = 120
    shifted = 0
    big = 0
    for i in range(1, len(cues)):
        prev_end = cues[i - 1]["tMs"] + cues[i - 1]["durMs"]
        gap = cues[i]["tMs"] - prev_end
        if gap >= 0:
            continue
        if -gap <= CLAMP_MS:
            cues[i]["tMs"] = prev_end + GAP_MS
            shifted += 1
        else:
            big += 1
    if shifted:
        print(f"narrate-mux: {shifted} cue(s) slid to clear a sub-{CLAMP_MS}ms collision", file=sys.stderr)
    if big:
        print(f"narrate-mux: WARNING {big} cue(s) overlap the previous line by >{CLAMP_MS}ms — a line was cut off on screen", file=sys.stderr)

    cmd = [ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", winpath(ffmpeg, args.video)]
    for c in cues:
        cmd += ["-i", winpath(ffmpeg, c["file"])]

    parts = []
    for i, c in enumerate(cues):
        delay = max(0, c["tMs"] + args.offset_ms)
        # adelay wants one value per channel; the clips are mono, but state both
        # so a stereo clip is not silently half-delayed.
        parts.append(f"[{i + 1}:a]adelay={delay}|{delay}[a{i}]")
    mix = "".join(f"[a{i}]" for i in range(len(cues)))
    # normalize=0: amix otherwise divides volume by the input count, which with
    # ~50 inputs makes the narration inaudible.
    parts.append(f"{mix}amix=inputs={len(cues)}:normalize=0:dropout_transition=0[a]")

    cmd += [
        "-filter_complex", ";".join(parts),
        "-map", "0:v", "-map", "[a]",
        "-c:v", "copy", "-c:a", "aac", "-b:a", "160k",
        # Deliberately NO -shortest: the mixed track ends at the last cue and the
        # video runs on past it, so -shortest would truncate the picture to the
        # final spoken word. Default behaviour keeps the longest input, which is
        # the video — exactly what we want.
        winpath(ffmpeg, args.out),
    ]

    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print(res.stderr.strip()[-800:], file=sys.stderr)
        return 1
    if fit is not None:
        fit["applied"] = True
        fit["out"] = args.out
        Path(args.drift_fit).write_text(json.dumps(fit, indent=1) + "\n")
    print(f"narrate-mux: {len(cues)} cues -> {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
