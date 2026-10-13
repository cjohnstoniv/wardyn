/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

export const PREFLIGHT_FRESH_MS = 60_000;

/** A preflight answer as the gate weighs it: which body generation it judged, and when. */
export interface PreflightRead {
  generation: number;
  at: number;
}

/**
 * Launch opens only with no blockers and a preflight answer for exactly the
 * current body that is under a minute old. Create stays the authority.
 */
export function launchGateOpen(g: { blockers: number; preflight: PreflightRead | null; generation: number; now: number }): boolean {
  return g.blockers === 0 && g.preflight !== null && g.preflight.generation === g.generation &&
    g.now - g.preflight.at < PREFLIGHT_FRESH_MS;
}
