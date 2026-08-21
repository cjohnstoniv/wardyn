#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Convert a util-linux `script` capture (typescript + --timing file) into an
asciicast v2 file for asciinema-player.

    scripts/cast-convert.py TYPESCRIPT TIMING OUT.cast [--cols 110] [--rows 30]

Why script(1) and not asciinema: the capture happens inside record-demo.sh's
Act 0 on whatever host runs the take, and util-linux is always there. The
classic timing format is `<delay> <bytecount>` per line against the raw
typescript byte stream (whose first "Script started" header line and trailing
"Script done" line are outside the timed stream's interesting content but
inside the byte accounting — the first header line IS consumed by the byte
offsets, so it is kept in the stream and simply renders for one frame).
"""

from __future__ import annotations

import argparse
import json
import sys


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("typescript")
    ap.add_argument("timing")
    ap.add_argument("out")
    ap.add_argument("--cols", type=int, default=110)
    ap.add_argument("--rows", type=int, default=30)
    args = ap.parse_args()

    data = open(args.typescript, "rb").read()
    # Drop script(1)'s own header line from the byte stream; the timing file's
    # offsets start AFTER it (script writes the header before timing begins).
    nl = data.find(b"\n")
    stream = data[nl + 1 :] if nl >= 0 else data

    events = []
    t = 0.0
    off = 0
    for line in open(args.timing):
        parts = line.split()
        if len(parts) != 2:
            continue
        delay, count = float(parts[0]), int(parts[1])
        t += delay
        chunk = stream[off : off + count]
        off += count
        if chunk:
            events.append([round(t, 4), "o", chunk.decode("utf-8", errors="replace")])

    with open(args.out, "w") as f:
        f.write(json.dumps({"version": 2, "width": args.cols, "height": args.rows}) + "\n")
        for e in events:
            f.write(json.dumps(e) + "\n")
    dur = events[-1][0] if events else 0
    print(f"cast: {len(events)} events, {dur:.1f}s -> {args.out}")
    return 0 if events else 1


if __name__ == "__main__":
    sys.exit(main())
