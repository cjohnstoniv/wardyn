/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Why can't I?" card beside a refusal (approved board M2, Option A).
// Strings are byte-exact from the board; WHY_CARD.TEAM_FALLBACK is the one
// string the boards do not carry (see the lane's PR body).
export const WHY_CARD = {
  SET_BY: "Set by",
  OWNER: "Owner",
  ENFORCED: "Enforced",
  CAN_CHANGE: "Can change",
  NOT_LISTED: "Not listed",
  COPY_DETAILS: "Copy details",
  COPIED: "Copied",
  BOUNDARY: "Wardyn boundary",
  RUN_POLICY: "This run's policy",
  TEAM: (name: string) => `${name} walls`,
  TEAM_FALLBACK: "Your team's walls",
  CAN_CHANGE_NOT_HERE: "Not from here. The owner can.",
  ENFORCED_AT_ATTACH: "By Wardyn, before the terminal opens",
  NO_WAY_TO_ASK: (owner: string) => `No way to ask is listed. Contact ${owner} directly.`,
  NO_OWNER: "This limit names no owner. Ask your Wardyn admin.",
} as const;
