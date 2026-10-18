/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings Host card's own words (#2876, owner option A). The tiles' fact strings are the console's
// existing ones and stay with the card; this is only what that rework added.
export const HOST_CARD = {
  // The state pill when the host has no sandbox runner. A host with one reads "Runs default to <tier>".
  CANT_LAUNCH: "Can't launch",
} as const;
