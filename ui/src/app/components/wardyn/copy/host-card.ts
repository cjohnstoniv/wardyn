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
  // The page could not read /setup/status (a 5xx, a 404 or a dead network): nothing on it would be true. A 5xx or
  // a 404 is an answer, so neither line says Wardyn could not be reached.
  UNREACHABLE_TITLE: "Couldn't load setup status",
  UNREACHABLE_BODY: "Wardyn's setup status couldn't be read, so nothing here would be accurate. Try again in a moment.",
} as const;
