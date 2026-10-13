/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Make's small barrier visual: a box in the barrier's metal with its glyph, its name and strength strip,
// and the strength word under the name. Review's Where group and the Effective policy card show it.

import { cn } from "../../../ui/utils";
import { BarrierStrengthStrip } from "../../../wardyn/barrier-strength-strip";
import { CC_META } from "../../../wardyn/cc-meta";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { TierIllustration } from "../../../wardyn/tier-illustration";
import type { ConfinementClass } from "../../../../lib/types/runs";

const TONE: Record<ConfinementClass, { box: string; name: string }> = {
  CC1: { box: "border-fence-border bg-fence-bg", name: "text-fence-fg" },
  CC2: { box: "border-wall-border bg-wall-bg", name: "text-wall-fg" },
  CC3: { box: "border-vault-border bg-vault-bg", name: "text-vault-fg" },
};
const TAGLINE: Record<ConfinementClass, string> = { CC1: NR_RUN.TAGLINE_FENCE, CC2: NR_RUN.TAGLINE_WALL, CC3: NR_RUN.TAGLINE_VAULT };

/** `caps` sets the words in capitals, as the policy card's list does. */
export function BarrierChip({ cc, caps = false }: { cc: ConfinementClass; caps?: boolean }) {
  return (
    <span className={cn("inline-flex w-fit max-w-full items-center gap-2 rounded-lg border py-1 pr-2 pl-1", TONE[cc].box)}>
      <span aria-hidden>
        <TierIllustration cc={cc} className="size-8 rounded-md p-1.5" />
      </span>
      <span className={cn("flex min-w-0 flex-col items-start gap-0.5", caps && "uppercase tracking-[0.08em]")}>
        <span className="flex items-center gap-2">
          <strong className={cn("text-sm font-semibold", TONE[cc].name)}>{CC_META[cc].label}</strong>
          <BarrierStrengthStrip tier={cc} />
        </span>
        <small className="text-xs text-muted-foreground">{TAGLINE[cc]}</small>
      </span>
    </span>
  );
}
