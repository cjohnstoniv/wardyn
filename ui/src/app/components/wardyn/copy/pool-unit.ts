/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/** A runner pool's "Who may use it" naming a unit: the picker of profiles and the saved row's lines. */
export const POOL_UNIT = {
  LABEL: "Unit",
  HELP: "A profile is a governance unit.",
  /** After the unit's name: a unit row admits runs launched in that unit or below it. */
  BELOW: "and the units below it",
  LOADING: "Loading profiles…",
  LOAD_FAILED: "Couldn't load profiles.",
  RETRY: "Try again",
  ORPHAN: "No longer a profile. It admits no one.",
} as const;
