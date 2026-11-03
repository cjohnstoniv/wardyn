/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings Host card's own words (#2876, owner option A). The tiles' fact strings are the console's
// existing ones and stay with the card; this is only what that rework added.
export const HOST_CARD = {
  // The state pill when the host has no sandbox runner. A host with one reads "Runs default to <tier>".
  CANT_LAUNCH: "Can't launch",
  // #3342: the card's own button; the shell's no-barrier notice says "Re-check barrier".
  RECHECK: "Re-check host",
  RECHECKING: "Re-checking…",
  CHECKED_JUST_NOW: "Checked just now",
  // Settings could not read /setup/status at all (a 5xx or a dead network): nothing on the page would be true.
  UNREACHABLE_TITLE: "Couldn't reach Wardyn",
  UNREACHABLE_BODY: "Settings didn't get an answer, so nothing here would be accurate. Try again in a moment.",
} as const;
