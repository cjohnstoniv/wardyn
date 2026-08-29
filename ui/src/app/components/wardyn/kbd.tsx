/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Kbd — one key, or one chord, drawn as a key cap.
//
// Deliberately NOT a keybindings registry. It renders what the caller says is
// bound; the binding itself stays where it is handled (focus-mode.tsx's own
// keydown listener), because a registry that can drift from the handler is a
// hint that lies, and a hint that lies is worse than no hint — the same rule
// that keeps ⌘K, ⌘1–4 and ⇧⌘F off the focus strip.
//
// Platform awareness is the one thing a caller cannot do inline: the modifier
// is ⌘ on a Mac and Ctrl everywhere else, and the two join differently (⌘\ has
// no separator, Ctrl+\ does).
import { cn } from "../ui/utils";

/** The platform modifier, spelled `Mod` by callers. */
export const MOD = "Mod";

// navigator.platform is deprecated and still the only signal every browser
// agrees on — userAgentData.platform is Chromium-only, and the userAgent
// fallback covers the rest. Read per render rather than cached at module load
// so a test can drive it, and because it is one regex on a short string.
export function isApplePlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  const nav = navigator as Navigator & { userAgentData?: { platform?: string } };
  return /mac|iphone|ipad|ipod/i.test(nav.userAgentData?.platform || nav.platform || nav.userAgent);
}

/** The chord as text — exported so a title/aria-label can say the same thing. */
export function chordLabel(keys: readonly string[], apple = isApplePlatform()): string {
  const parts = keys.map((k) => (k === MOD ? (apple ? "⌘" : "Ctrl") : k));
  // On a Mac the modifier glyphs abut (⌘\); everywhere else they are joined.
  return apple ? parts.join("") : parts.join("+");
}

export function Kbd({ keys, className }: { keys: readonly string[]; className?: string }) {
  return (
    <kbd
      className={cn(
        // 11px rung, mono (it is a literal — CONSOLE-RULES §3), hairline only:
        // a key cap is not a control and must not read as one.
        "inline-flex items-center rounded-md border border-border bg-muted px-1.5 py-0.5 font-mono text-meta font-medium text-foreground",
        className,
      )}
    >
      {chordLabel(keys)}
    </kbd>
  );
}
