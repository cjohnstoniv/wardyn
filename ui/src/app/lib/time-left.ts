/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Time left as the console's request and run screens spell it: "3 h 42 min", "4 h", "25 min". Whole minutes,
// rounded; the minutes are left out when they are zero, and nothing below zero is shown.
export function timeLeft(ms: number): string {
  const min = Math.max(0, Math.round(ms / 60_000));
  const h = Math.floor(min / 60);
  const m = min % 60;
  return h && m ? `${h} h ${m} min` : h ? `${h} h` : `${m} min`;
}
