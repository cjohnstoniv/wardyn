/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Default barrier tier (0.7.8) — pure resolution helpers, no persistence. The
// default is a SERVER fact now (runs_policy.go's strongestAdvertisedAtOrAbove:
// the strongest installed class at or above the policy floor), so there is
// nothing left for the browser to remember across sessions — the
// wardyn-default-confinement localStorage key this file used to own is gone.
//
// What's left is the pure "which tier wins" math, still shared by every
// surface that shows a barrier default: New Run resolves its own from
// /setup/status directly (new-run-screen.tsx); Getting started and Settings'
// Host card pass an in-session pick (never persisted) as `persisted` below,
// purely so a click during THIS view stays highlighted across a re-check;
// onboarding-screen/member-getting-started/record-pane read strongestAvailable
// straight, with no pick to prefer at all.
import { CC_ORDER, type ConfinementClass } from "../../lib/types";

/** The strongest class present in `available` — the last CC_ORDER member present. */
export function strongestAvailable(available: ConfinementClass[]): ConfinementClass | undefined {
  return CC_ORDER.filter((cc) => available.includes(cc)).at(-1);
}

/**
 * Resolve the barrier tier to show as selected: `persisted` (an in-session
 * pick, e.g. a click) if this host can still run it, else the strongest tier
 * this host can run, else CC1 (nothing available — never leave the picker
 * with no selection at all).
 */
export function resolveDefaultCc(
  persisted: ConfinementClass | null,
  available: ConfinementClass[],
): ConfinementClass {
  if (persisted && available.includes(persisted)) return persisted;
  return strongestAvailable(available) ?? "CC1";
}
