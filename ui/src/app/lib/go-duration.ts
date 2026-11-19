/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go duration strings as the server stores a governance duration ("4h", "1h30m", "15m"): to milliseconds and
// back. Only the h, m and s units such a duration uses; anything else is NaN.
const UNIT_MS: Record<string, number> = { h: 3_600_000, m: 60_000, s: 1_000 };

export function goDurationMs(s: string): number {
  const parts = [...s.matchAll(/(\d+)([hms])/g)];
  if (!parts.length || parts.map((p) => p[0]).join("") !== s) return NaN;
  return parts.reduce((ms, p) => ms + Number(p[1]) * UNIT_MS[p[2]], 0);
}

// Whole minutes as types.CanonicalDuration writes them: 4h, 1h30m, 45m.
export function goDuration(ms: number): string {
  const min = Math.round(ms / 60_000);
  const h = Math.floor(min / 60);
  const m = min % 60;
  return h && m ? `${h}h${m}m` : h ? `${h}h` : `${m}m`;
}
