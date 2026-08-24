#!/usr/bin/env python3
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

"""Tile a review corpus into labeled 6-up contact sheets.

    scripts/demo-review/tile-sheets.py CORPUS_DIR [--per-sheet 6] [--max-sheets 11]

CORPUS_DIR is what build-corpus.py wrote: frames named by timestamp, plus
transcript.txt and timeline.json. Frames go onto 3x2 sheets in time order, each
tile captioned with its timestamp and the line being spoken over it.

WHY sheets and not frames. A viewing lane that reads one PNG per turn holds
~150k context per turn and rewrites ~140k of prompt cache every few seconds —
one round burned two session-limit windows. Six frames per read is ~11 reads for
a ten-minute take, and the caption carries the narration the frame needs anyway.

The sampling is build-corpus.py's: one frame per cue, one extra at the midpoint
of any cue over 6s, one for every silence over 5s. If that still overruns
--per-sheet x --max-sheets, the mid-cue extras thin first (evenly), then the
gap frames, and only then the one-per-cue frames — so the last thing lost is
the guarantee that every spoken line has a picture.

Writes into CORPUS_DIR: sheet-NN.png and sheets.md (the index, with every
tile's caption, and the reading order for the lane prompt).

  --selftest   build a synthetic corpus in a temp dir and check the tiling
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import textwrap
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

SHEET_W = 1920           # anything wider is thrown away by the reader's downscale
COLS, ROWS = 3, 2
MARGIN, GUTTER = 16, 12
HEAD_H, LABEL_H = 34, 86
FONT_SIZE = 19
BG, INK, DIM, LINE = "#f2f2f2", "#111111", "#555555", "#999999"

# t00123.4_cue07m.png -> (123.4, "cue07m")
FRAME_RE = re.compile(r"^t(\d+(?:\.\d+)?)_(cue\d+m?|gap\d+)\.png$")
DEJAVU = "/usr/share/fonts/truetype/dejavu/DejaVuSans%s.ttf"


def font(size: int, bold: bool = False) -> ImageFont.FreeTypeFont:
    try:
        return ImageFont.truetype(DEJAVU % ("-Bold" if bold else ""), size)
    except OSError:
        return ImageFont.load_default(size=size)


def load_cues(corpus: Path) -> list[dict]:
    """Cues as {t, dur, text} in seconds. timeline.json has durations; a corpus
    built before it existed falls back to the transcript, which does not."""
    tl = corpus / "timeline.json"
    if tl.is_file():
        cues = json.loads(tl.read_text()).get("cues", [])
        return sorted(
            ({"t": c["tMs"] / 1000, "dur": c.get("durMs", 0) / 1000, "text": c["text"]} for c in cues),
            key=lambda c: c["t"],
        )
    out = []
    for line in (corpus / "transcript.txt").read_text().splitlines():
        m = re.match(r"^\[\s*([\d.]+)s\]\s?(.*)$", line)
        if m:
            out.append({"t": float(m.group(1)), "dur": 0.0, "text": m.group(2)})
    return out


def caption(cues: list[dict], t: float) -> str:
    """The narration line active at t. A line that has already finished is still
    the frame's context, so it is shown with a leading '…' rather than dropped —
    a persona must be able to tell speech from silence."""
    spoken = [c for c in cues if c["t"] <= t]
    if not spoken:
        return "(before the first line)"
    c = spoken[-1]
    return c["text"] if not c["dur"] or t <= c["t"] + c["dur"] else "… " + c["text"]


def kind(name: str) -> str:
    return "gap" if name.startswith("gap") else "mid" if name.endswith("m") else "cue"


def select(frames: list[tuple[float, str, Path]], budget: int) -> list[tuple[float, str, Path]]:
    for k in ("mid", "gap", "cue"):
        excess = len(frames) - budget
        if excess <= 0:
            break
        pool = [f for f in frames if kind(f[1]) == k]
        # From index 1: the opening frame is the title card, never the one to lose.
        drop = {f[2] for f in pool[1 :: max(1, len(pool) // excess)][:excess]}
        frames = [f for f in frames if f[2] not in drop]
    return frames


def grid(per_sheet: int) -> tuple[int, int]:
    """Columns x rows for --per-sheet. Three across is the default 6-up; a
    smaller budget narrows rather than leaving holes in the sheet."""
    cols = min(COLS, per_sheet)
    return cols, -(-per_sheet // cols)


def draw_sheet(tiles: list[tuple[float, str, Path, str]], head: str, out: Path, cols: int, rows: int) -> None:
    tile_w = (SHEET_W - 2 * MARGIN - (cols - 1) * GUTTER) // cols
    with Image.open(tiles[0][2]) as probe:
        tile_h = round(tile_w * probe.height / probe.width)
    cell_h = tile_h + LABEL_H
    sheet = Image.new("RGB", (SHEET_W, 2 * MARGIN + HEAD_H + rows * cell_h + (rows - 1) * GUTTER), BG)
    d = ImageDraw.Draw(sheet)
    d.text((MARGIN, MARGIN), head, font=font(FONT_SIZE + 2, bold=True), fill=INK)
    body, bold = font(FONT_SIZE), font(FONT_SIZE, bold=True)
    # ~2 px per point of DejaVu Sans at this size; measured once, not guessed.
    wrap_at = max(20, int(tile_w / (body.getlength("n") or 10)))

    for i, (t, name, path, text) in enumerate(tiles):
        x = MARGIN + (i % cols) * (tile_w + GUTTER)
        y = MARGIN + HEAD_H + (i // cols) * (cell_h + GUTTER)
        with Image.open(path) as im:
            sheet.paste(im.convert("RGB").resize((tile_w, tile_h), Image.LANCZOS), (x, y))
        d.rectangle([x, y, x + tile_w - 1, y + tile_h - 1], outline=LINE)
        tag = f"#{i + 1}  [{t:7.1f}s]  {name}"
        d.text((x, y + tile_h + 5), tag, font=bold, fill=INK)
        for j, ln in enumerate(textwrap.wrap(text, wrap_at)[:3]):
            d.text((x, y + tile_h + 5 + (j + 1) * (FONT_SIZE + 3)), ln, font=body, fill=DIM)
    sheet.save(out)


def build(corpus: Path, per_sheet: int, max_sheets: int) -> tuple[int, int]:
    frames = []
    for p in sorted(corpus.glob("t*.png")):
        m = FRAME_RE.match(p.name)
        if m:
            frames.append((float(m.group(1)), m.group(2), p))
    if not frames:
        sys.exit(f"no build-corpus frames (tNNNNN.N_*.png) in {corpus}")
    frames.sort()
    cols, rows = grid(per_sheet)
    frames = select(frames, per_sheet * max_sheets)
    cues = load_cues(corpus)
    tiles = [(t, n, p, caption(cues, t)) for t, n, p in frames]

    header = (corpus / "transcript.txt").read_text().splitlines()[0].lstrip("# ").strip() \
        if (corpus / "transcript.txt").is_file() else corpus.name
    for old in corpus.glob("sheet-*.png"):
        old.unlink()
    sheets = [tiles[i:i + per_sheet] for i in range(0, len(tiles), per_sheet)]
    md = [
        f"# Contact sheets — {header}",
        "",
        f"{len(sheets)} sheets · {len(tiles)} frames · {cols}x{rows} per sheet, in time order.",
        "Read sheet-01.png first and go in order; each tile is captioned with its",
        "number, timestamp and the narration line spoken over it (a leading `…` means",
        "the line had already finished — that frame is silence). Cite a frame as",
        "`sheet-03.png #2`.",
        "",
    ]
    for n, tiles_on in enumerate(sheets, 1):
        out = corpus / f"sheet-{n:02d}.png"
        # A short last sheet gets a short canvas, not a page of empty cells.
        draw_sheet(tiles_on, f"{header}  ·  sheet {n}/{len(sheets)}", out, cols,
                   min(rows, -(-len(tiles_on) // cols)))
        md.append(f"## sheet-{n:02d}.png")
        md += [f"{i + 1}. [{t:7.1f}s] {text}" for i, (t, _, _, text) in enumerate(tiles_on)]
        md.append("")
    (corpus / "sheets.md").write_text("\n".join(md))
    return len(sheets), len(tiles)


def selftest() -> None:
    import tempfile

    with tempfile.TemporaryDirectory() as tmp:
        c = Path(tmp)
        (c / "timeline.json").write_text(json.dumps({"cues": [
            {"tMs": 1000, "durMs": 2000, "text": "first line"},
            {"tMs": 9000, "durMs": 8000, "text": "a long line " * 12},
        ]}))
        (c / "transcript.txt").write_text("# take.mp4\n[    1.0s] first line\n")
        for t, n in ((1.8, "cue00"), (5.0, "gap00"), (9.8, "cue01"), (13.0, "cue01m"),
                     (20.0, "gap01"), (21.0, "cue02"), (22.0, "cue03")):
            Image.new("RGB", (1920, 1080), "#204060").save(c / f"t{t:07.1f}_{n}.png")
        assert build(c, 6, 11) == (2, 7), "7 frames under budget must tile 2x6"
        assert grid(6) == (3, 2) and grid(4) == (3, 2) and grid(2) == (2, 1)
        md = (c / "sheets.md").read_text()
        assert "## sheet-02.png" in md and len(list(c.glob("sheet-*.png"))) == 2
        assert "1. [    1.8s] first line" in md, "caption must be the active cue"
        assert "[    5.0s] … first line" in md, "silence must be marked, not dropped"
        assert "(before the first line)" not in md
        # Over budget: mid thins first, then gaps; every cue frame survives.
        assert build(c, 2, 2) == (2, 4)
        kept = {p.name.split("_")[1][:-4] for p in c.glob("t*.png")}  # nothing deleted
        assert len(kept) == 7
        md = (c / "sheets.md").read_text()
        assert "cue01m" not in md and md.count("[") >= 4
    print("selftest ok")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("corpus", nargs="?", type=Path)
    ap.add_argument("--per-sheet", type=int, default=COLS * ROWS)
    ap.add_argument("--max-sheets", type=int, default=11)
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args()
    if a.selftest:
        selftest()
        return 0
    if not a.corpus:
        ap.error("CORPUS_DIR is required")
    n, frames = build(a.corpus, a.per_sheet, a.max_sheets)
    print(f"{a.corpus}: {frames} frames → {n} sheets + sheets.md")
    return 0


if __name__ == "__main__":
    sys.exit(main())
