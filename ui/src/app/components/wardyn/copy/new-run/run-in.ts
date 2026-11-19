/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run's "Run in" choice: the unit a run runs in, and what Review says about a unit the person can only ask for.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts.

export const NR_RUN_IN = {
  QUESTION: "Which unit should it run in?",
  LABEL: "Run in",
  HINT: "Runs use your home unit's limits. Pick another of your units to run under its limits instead.",
  HOME_VALUE: (unit: string) => `${unit} (home)`,
  GROUP_YOURS: "Your units",
  GROUP_ELEVATED: "Elevated",
  GROUP_ASK: "You can ask for access",
  CHIP_HOME: "Home",
  CHIP_ELEVATED: "Elevated",
  CHIP_NEEDS: "Needs elevation",
  ELEVATED_LINE: (at: string, left: string) => `Ends ${at} (in ${left})`,
  ASK_LINE: (max: string) => `Up to ${max}, with a person's yes`,
  REVIEW_ROW: (unit: string) => `Run in: ${unit}`,
  REVIEW_ENDS: (at: string) => `ends ${at}`,
  BLOCKER_ASK: (unit: string) => `${unit} needs access you don't have. Ask for time-limited access to run in it.`,
  REQUEST_ELEVATION: "Request elevation",
  BLOCKER_WAITING: (unit: string) => `Your request for ${unit} is waiting for approval. You can launch once someone approves it, or pick another unit.`,
  LOADING: "Loading your units…",
  UNITS_UNREADABLE: "Couldn't load your other units. This run goes to your home unit.",
  ENDED: (unit: string, home: string) => `Your elevation in ${unit} ended, so this run goes to your home unit, ${home}.`,
  NO_LONGER_IN: (unit: string, home: string) => `You're no longer in ${unit}, so this run goes to your home unit, ${home}.`,
} as const;
