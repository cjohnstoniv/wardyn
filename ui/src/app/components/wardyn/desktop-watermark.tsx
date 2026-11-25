/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop watermark (#2969): the viewer's name and the run id, tiled on a slant over the picture. The mark is two-tone,
// the foreground at 20% filled and the background at 20% as an outline, so it reads on a dark window and on a light one. It is a
// layer the caller places beside the picture, never inside it: it takes no input, is hidden from assistive technology, and is
// only ever drawn on screen, so a downloaded recording carries none.
import * as React from "react";
import { cn } from "../ui/utils";
import { DESKTOP } from "./copy/desktop";

const GLYPH_PX = 12;
// A 12px monospace glyph is about 0.6 em wide; a little over, so a tile never clips its own text.
const GLYPH_ADVANCE_PX = 7.4;
const TILE_GAP_PX = 72;
const ROW_PX = 96;
const SLANT_DEG = -20;

export function DesktopWatermark({ who, run, className }: { who: string; run: string; className?: string }) {
  const text = DESKTOP.watermarkText(who, run);
  const pattern = `watermark-${React.useId().replace(/\W/g, "")}`;
  const w = Math.ceil(text.length * GLYPH_ADVANCE_PX) + TILE_GAP_PX;
  const half = w / 2;
  const mark = "fill-foreground/20 stroke-background/20 font-mono";
  return (
    <div aria-hidden="true" data-testid="desktop-watermark" className={cn("pointer-events-none absolute select-none overflow-hidden", className)}>
      <svg className="size-full" focusable="false">
        <defs>
          <pattern id={pattern} width={w} height={ROW_PX * 2} patternUnits="userSpaceOnUse" patternTransform={`rotate(${SLANT_DEG})`}>
            <text x={0} y={ROW_PX / 2} fontSize={GLYPH_PX} strokeWidth={1} style={{ paintOrder: "stroke" }} className={mark}>{text}</text>
            <text x={half} y={ROW_PX * 1.5} fontSize={GLYPH_PX} strokeWidth={1} style={{ paintOrder: "stroke" }} className={mark}>{text}</text>
            <text x={half - w} y={ROW_PX * 1.5} fontSize={GLYPH_PX} strokeWidth={1} style={{ paintOrder: "stroke" }} className={mark}>{text}</text>
          </pattern>
        </defs>
        <rect width="100%" height="100%" fill={`url(#${pattern})`} />
      </svg>
    </div>
  );
}

/**
 * The header control of a view that can show the watermark: the line that says the organisation requires it, or the per-view
 * button when it does not. The choice is not stored.
 */
export function WatermarkControl({ required, on, onToggle }: { required: boolean; on: boolean; onToggle: () => void }) {
  if (required) return <span className="text-muted-foreground">{DESKTOP.watermarkRequired}</span>;
  return (
    <div className="inline-flex rounded-[9px] border border-border bg-surface-2 p-[3px]">
      <button
        type="button"
        aria-pressed={on}
        onClick={onToggle}
        className={on ? "rounded-md bg-card px-3 py-[5px] shadow-sm" : "rounded-md px-3 py-[5px] text-muted-foreground"}
      >
        {DESKTOP.watermarkToggle}
      </button>
    </div>
  );
}
