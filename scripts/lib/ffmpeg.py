# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""The ONE ffmpeg the demo pipeline's python steps use.

    WARDYN_DEMO_FFMPEG  ->  ffmpeg.exe on PATH  ->  winget's Gyan.FFmpeg  ->  ffmpeg

gdigrab is a Windows capture device, so a take is encoded by the WINDOWS build,
which winget hides on the WINDOWS PATH: its Gyan.FFmpeg is a zip package that
appends to that PATH, and a WSL shell only inherits it at startup — so a
freshly-installed ffmpeg stays invisible until a new shell. Resolve it directly
rather than making that the operator's problem.

Four scripts (demo-drift, narrate-mux, demo-ffwd, demo-review/build-corpus) each
carried their own copy of this ladder and all three rungs had DRIFTED:

  - none of them read WARDYN_DEMO_FFMPEG — the documented override
    record-demo.sh honours, and the only way a Linux ffmpeg gets pressed into
    service on a night when the ffmpeg.exe interop socket is flapping. So the
    override that recorded a take was not the binary that then measured it;
  - narrate-mux/demo-ffwd preferred a plain `ffmpeg` on PATH OVER the Windows
    build, so a box with both used a different encoder than the recorder;
  - build-corpus had no explicit escape at all and died on "no ffmpeg.exe".

This is record-demo.sh's ladder, with demo-drift.py's trailing plain-`ffmpeg`
rung kept as the union's last resort — the recorder itself does not have that
one (it needs gdigrab), but mux/ffwd/corpus only transcode.
"""

from __future__ import annotations

import os
import shutil
from pathlib import Path

WINGET_GLOB = "*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe"


def _ffmpeg(explicit: str | None = None) -> str:
    """Resolve the ffmpeg to run; exit with the install hint when there is none."""
    for cand in (explicit, os.environ.get("WARDYN_DEMO_FFMPEG"), shutil.which("ffmpeg.exe")):
        if cand:
            return cand
    # sorted() so a box with two winget versions picks the same one every run.
    for hit in sorted(Path("/mnt/c/Users").glob(WINGET_GLOB)):
        return str(hit)
    if plain := shutil.which("ffmpeg"):
        return plain
    raise SystemExit(
        "no ffmpeg found — pass --ffmpeg, set WARDYN_DEMO_FFMPEG, "
        "or install one once with: winget.exe install Gyan.FFmpeg"
    )
