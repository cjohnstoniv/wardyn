/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — the D2 row glyph (design.md §1's colour-and-glyph rule): one
// glyph per row, aria-hidden (the printed status word carries the state, per
// §5), never the teal accent. This is a SEPARATE, NEW vocabulary from
// wardyn/run-state-glyph.tsx's RunStateGlyph — that component still renders
// the run cockpit's header and live-approvals (both unchanged by this lane;
// design.md §1's closing line about "run-state-glyph.tsx's header" describes
// that file's OWN standing rule, not an instruction to touch it here).
import type { RowHue } from "./runs-model";
import { cn } from "../../ui/utils";

export type RowGlyphKind = "dot" | "ring" | "need" | "fail" | "check" | "square-fill" | "square-outline";

const HUE_TEXT: Record<RowHue, string> = {
  blue: "text-info",
  amber: "text-warning",
  red: "text-danger",
  grey: "text-muted-foreground",
};

export function RowGlyph({ hue, kind, className }: { hue: RowHue; kind: RowGlyphKind; className?: string }) {
  const cls = cn("size-3.5 shrink-0", HUE_TEXT[hue], className);
  const common = { width: 14, height: 14, viewBox: "0 0 14 14", "aria-hidden": true, focusable: false } as const;
  switch (kind) {
    case "dot":
      // Running — a filled dot with a pulsing ring. theme.css's universal
      // prefers-reduced-motion guard freezes animate-ping for us; no
      // per-glyph motion-reduce class needed (same reason Chip's own pulse
      // dot, primitives.tsx, carries none either).
      return (
        <svg {...common} className={cls}>
          <circle
            cx="7"
            cy="7"
            r="6"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            className="origin-[7px_7px] animate-ping"
          />
          <circle cx="7" cy="7" r="4" fill="currentColor" />
        </svg>
      );
    case "ring":
      // Starting / Queued — a static hollow ring, no pulse.
      return (
        <svg {...common} className={cls}>
          <circle cx="7" cy="7" r="4.5" fill="none" stroke="currentColor" strokeWidth="2" />
        </svg>
      );
    case "need":
      // Every needs-you kind — one "!" disc, amber.
      return (
        <svg {...common} className={cls}>
          <circle cx="7" cy="7" r="6" fill="currentColor" />
          <rect x="6.2" y="3.2" width="1.6" height="4.6" rx="0.8" className="fill-card" />
          <circle cx="7" cy="10.2" r="0.95" className="fill-card" />
        </svg>
      );
    case "fail":
      // Failed only — a red "×" disc.
      return (
        <svg {...common} className={cls}>
          <circle cx="7" cy="7" r="6" fill="currentColor" />
          <path
            d="M4.8 4.8l4.4 4.4M9.2 4.8l-4.4 4.4"
            className="stroke-card"
            strokeWidth="1.6"
            strokeLinecap="round"
          />
        </svg>
      );
    case "check":
      // Completed — a check, no disc.
      return (
        <svg {...common} className={cls}>
          <path
            d="M2.5 7.4l3 3 6-6.4"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      );
    case "square-fill":
      // Killed — a filled square.
      return (
        <svg {...common} className={cls}>
          <rect x="3" y="3" width="8" height="8" rx="1.5" fill="currentColor" />
        </svg>
      );
    case "square-outline":
    default:
      // Stopped / Archived (and a lease-ended run, once L5 lands) — a square
      // outline.
      return (
        <svg {...common} className={cls}>
          <rect x="3.2" y="3.2" width="7.6" height="7.6" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
        </svg>
      );
  }
}

/** Maps a row's presentation onto the glyph SHAPE (hue is carried separately
 *  by RowPresentation.hue) — see design.md §1's table. */
export function glyphKindFor(hue: RowHue, word: string, state: string): RowGlyphKind {
  if (hue === "amber") return "need";
  if (hue === "red") return "fail";
  // Round 2 F5: a lease-ended run's `state` is still RUNNING (rowPresentation
  // pre-empts only the word/hue, not the field itself), so the RUNNING check
  // below would still hand it the pulsing dot. The mock's glyph() legend is
  // explicit: "Ended at its end time" is the square-outline shape, same as
  // any other over row — checked on the word, ahead of the state switch.
  if (word === "Ended at its end time") return "square-outline";
  if (state === "RUNNING") return "dot";
  if (state === "STARTING" || state === "PENDING") return "ring";
  if (word === "Completed") return "check";
  if (word === "Killed") return "square-fill";
  return "square-outline";
}
