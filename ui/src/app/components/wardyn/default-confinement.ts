/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Default barrier tier (E3) — the single source for the operator's default
// barrier: the strongest available class (over CC_ORDER) plus the localStorage
// persistence for their explicit pick. The Getting-started barrier step persists
// the pick here; the New Run wizard, the workspace-import Record/Security chips,
// and onboarding-screen read it back via resolveDefaultCc()/getDefaultCc()/
// strongestAvailable(), treating it as a floor and never silently downgrading
// below what's persisted.
import type { ConfinementClass } from "../../lib/types";
import { lsGet, lsSet } from "../../lib/storage";
import { CC_ORDER } from "./cc-meta";

/** The strongest class present in `available` — the last CC_ORDER member present. */
export function strongestAvailable(available: ConfinementClass[]): ConfinementClass | undefined {
  return CC_ORDER.filter((cc) => available.includes(cc)).at(-1);
}

/**
 * Resolve the default barrier tier: the operator's persisted pick if this host
 * can still run it, else the strongest tier this host can run, else CC1 (nothing
 * available — never leave the wizard with no default at all).
 */
export function resolveDefaultCc(
  persisted: ConfinementClass | null,
  available: ConfinementClass[],
): ConfinementClass {
  if (persisted && available.includes(persisted)) return persisted;
  return strongestAvailable(available) ?? "CC1";
}

// ---------------------------------------------------------------------------
// Persisted default — via lib/storage's private-mode-tolerant lsGet/lsSet.
// ---------------------------------------------------------------------------
const DEFAULT_CC_KEY = "wardyn-default-confinement";

export function getDefaultCc(): ConfinementClass | null {
  const v = lsGet(DEFAULT_CC_KEY);
  return v === "CC1" || v === "CC2" || v === "CC3" ? v : null;
}

export function setDefaultCc(cc: ConfinementClass): void {
  lsSet(DEFAULT_CC_KEY, cc);
}
