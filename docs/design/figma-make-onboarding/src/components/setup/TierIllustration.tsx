import type { ReactElement } from "react";
import type { TierId } from "../../data/setupFixtures";
import { cn } from "../ui/utils";

// Inline SVG per tier — colored via `currentColor` so the metal-ramp tokens drive them.
// Fence: pickets with visible gaps. Wall: solid brick face. Vault: fully enclosed strongbox.
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
        <path d="M8 20h48M8 30h48M24 10v10M40 20v10M16 30v10M32 10v10M48 30v10M24 30v10" opacity="0.8" />
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
        <path d="M32 17v-3M32 36v-3M40 25h3M21 25h3M37.7 19.3l2-2M24.3 32.7l-2 2M37.7 30.7l2 2M24.3 17.3l-2-2" opacity="0.85" />
      </g>
    </svg>
  );
}

const MARKS: Record<TierId, () => ReactElement> = {
  fence: FenceMark,
  wall: WallMark,
  vault: VaultMark,
};

const TIER_COLOR: Record<TierId, { text: string; wash: string; meter: string }> = {
  fence: { text: "text-tier-fence", wash: "bg-tier-fence-wash", meter: "bg-tier-fence" },
  wall: { text: "text-tier-wall", wash: "bg-tier-wall-wash", meter: "bg-tier-wall" },
  vault: { text: "text-tier-vault", wash: "bg-tier-vault-wash", meter: "bg-tier-vault" },
};

const STRENGTH: Record<TierId, number> = { fence: 1, wall: 2, vault: 3 };

export function TierIllustration({ tier, className }: { tier: TierId; className?: string }) {
  const Mark = MARKS[tier];
  const color = TIER_COLOR[tier];
  return (
    <div
      className={cn(
        "flex size-14 items-center justify-center rounded-xl p-2.5",
        color.wash,
        color.text,
        className,
      )}
    >
      <Mark />
    </div>
  );
}

// Real strength meter — three segments, filled per tier, not color-only (filled vs outline).
export function StrengthMeter({ tier }: { tier: TierId }) {
  const level = STRENGTH[tier];
  const color = TIER_COLOR[tier];
  return (
    <div
      className="flex items-center gap-1"
      role="img"
      aria-label={`Isolation strength ${level} of 3`}
    >
      {[1, 2, 3].map((seg) => (
        <span
          key={seg}
          className={cn(
            "h-1.5 w-6 rounded-full border",
            seg <= level
              ? `${color.meter} border-transparent`
              : "border-border-strong bg-transparent",
          )}
        />
      ))}
    </div>
  );
}
