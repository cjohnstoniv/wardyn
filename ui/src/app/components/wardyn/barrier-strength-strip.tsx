/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { cn } from "../ui/utils";
import { confinementTierMeta } from "./primitives";
import type { ConfinementClass } from "../../lib/types";

// Redundant, non-color ordinal cue for the Fence/Wall/Vault barrier ladder —
// three ~16px segments, filled up to the tier's ordinal (Fence=1, Wall=2,
// Vault=3) and tinted with that tier's metal color, so the ladder position
// still reads for anyone who can't rely on the chip's color alone.
const SEGMENTS = [1, 2, 3] as const;

export function BarrierStrengthStrip({
  tier,
  muted,
}: {
  tier: ConfinementClass;
  /** Render every segment in a low-emphasis neutral tone instead of the tier's metal color. */
  muted?: boolean;
}) {
  const { ordinal, fillClass } = confinementTierMeta(tier);
  const label = `Barrier strength ${ordinal} of 3`;
  return (
    <span role="img" aria-label={label} title={label} className="inline-flex items-center gap-0.5">
      {SEGMENTS.map((n) => (
        <span
          key={n}
          aria-hidden="true"
          className={cn(
            "h-1.5 w-4 rounded-full",
            n <= ordinal ? (muted ? "bg-muted-foreground/40" : fillClass) : "bg-muted",
          )}
        />
      ))}
    </span>
  );
}
