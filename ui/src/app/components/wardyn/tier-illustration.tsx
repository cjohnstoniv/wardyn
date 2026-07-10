/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Per-tier barrier marks for the Environment step's protection-matrix picker,
// ported from the Figma onboarding snapshot (setup/TierIllustration.tsx). The
// SVG paths are verbatim; only the color classes are swapped to this repo's real
// metal-ramp tokens (fence/wall/vault in theme.css), NOT the snapshot's --tier-*.
// Fence = pickets with visible gaps, Wall = solid brick face, Vault = enclosed
// strongbox. Colored via currentColor so the token drives fill.
//
// The three-segment strength meter is intentionally NOT here — it's the existing
// BarrierStrengthStrip (barrier-strength-strip.tsx), reused as-is.
import type { ReactElement } from "react";
import { cn } from "../ui/utils";
import type { ConfinementClass } from "../../lib/types";

function FenceMark() {
  return (
    <svg viewBox="0 0 64 48" fill="none" className="size-full" aria-hidden>
      <g stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
        <path d="M10 44V14l4-4 4 4v30" />
        <path d="M28 44V14l4-4 4 4v30" />
        <path d="M46 44V14l4-4 4 4v30" />
        <path d="M6 22h52" opacity="0.7" />
        <path d="M6 34h52" opacity="0.7" />
      </g>
    </svg>
  );
}

function WallMark() {
  return (
    <svg viewBox="0 0 64 48" fill="none" className="size-full" aria-hidden>
      <g stroke="currentColor" strokeWidth="2.5" strokeLinejoin="round">
        <rect x="8" y="10" width="48" height="30" rx="2" />
        <path
          d="M8 20h48M8 30h48M24 10v10M40 20v10M16 30v10M32 10v10M48 30v10M24 30v10"
          opacity="0.8"
        />
      </g>
    </svg>
  );
}

function VaultMark() {
  return (
    <svg viewBox="0 0 64 48" fill="none" className="size-full" aria-hidden>
      <g stroke="currentColor" strokeWidth="2.5" strokeLinejoin="round">
        <rect x="10" y="8" width="44" height="34" rx="4" />
        <circle cx="32" cy="25" r="8" />
        <path
          d="M32 17v-3M32 36v-3M40 25h3M21 25h3M37.7 19.3l2-2M24.3 32.7l-2 2M37.7 30.7l2 2M24.3 17.3l-2-2"
          opacity="0.85"
        />
      </g>
    </svg>
  );
}

const MARKS: Record<ConfinementClass, () => ReactElement> = {
  CC1: FenceMark,
  CC2: WallMark,
  CC3: VaultMark,
};

// Real theme tokens (theme.css) — the metal ramp, not the snapshot's --tier-*.
const WASH: Record<ConfinementClass, string> = {
  CC1: "bg-fence-bg text-fence-fg",
  CC2: "bg-wall-bg text-wall-fg",
  CC3: "bg-vault-bg text-vault-fg",
};

export function TierIllustration({
  cc,
  title,
  className,
}: {
  cc: ConfinementClass;
  /** Native tooltip — the short Fence/Wall/Vault metaphor line. */
  title?: string;
  className?: string;
}) {
  const Mark = MARKS[cc];
  return (
    <div
      title={title}
      className={cn(
        "flex size-14 shrink-0 items-center justify-center rounded-xl p-2.5",
        WASH[cc],
        className,
      )}
    >
      <Mark />
      {/* AT copy of the native title: the SVG is aria-hidden and a div's `title`
          isn't reliably announced, so the metaphor also rides in as sr-only text.
          When this renders inside the tier radio it joins the radio's accname. */}
      {title && <span className="sr-only">{title}</span>}
    </div>
  );
}
