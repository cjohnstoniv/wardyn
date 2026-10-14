/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A barrier's glyph, its name (always text, in the barrier's metal colour) and,
// on request, the three-segment strength strip. The glyph is decorative.
import { cn } from "../ui/utils";
import { BarrierStrengthStrip } from "./barrier-strength-strip";
import { CC_META } from "./cc-meta";
import { TierIllustration } from "./tier-illustration";
import type { ConfinementClass } from "../../lib/types";

const NAME_TINT: Record<ConfinementClass, string> = {
  CC1: "text-fence-fg",
  CC2: "text-wall-fg",
  CC3: "text-vault-fg",
};

export function BarrierMark({
  cc,
  size = "md",
  strength,
  nameId,
}: {
  cc: ConfinementClass;
  size?: "sm" | "md";
  strength?: boolean;
  /** Lets a control name itself by the barrier's name. */
  nameId?: string;
}) {
  return (
    <span className="inline-flex items-center gap-2">
      <span aria-hidden>
        <TierIllustration cc={cc} className={size === "sm" ? "size-8 rounded-lg p-1.5" : undefined} />
      </span>
      <span className={cn("font-medium", NAME_TINT[cc])} id={nameId}>
        {CC_META[cc].label}
      </span>
      {strength && <BarrierStrengthStrip tier={cc} />}
    </span>
  );
}
