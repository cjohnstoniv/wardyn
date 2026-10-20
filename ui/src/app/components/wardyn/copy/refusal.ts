/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3047 (R6RUN, approved 2026-10-20): the "Why can't I?" card's row labels, and the one card a run the
// viewer cannot open gets. The server answers a missing run and someone else's run with the same plain 404,
// so this card names neither an id, a title, an owner nor a state, and the two cases cannot differ.
export const REFUSAL = {
  ARIA: "Why this was blocked",
  WALL: "Wall",
  SET_BY: "Set by",
  ENFORCED: "Enforced",
  CAN_CHANGE: "Can change",
} as const;

export const RUN_NOT_FOUND = {
  TITLE: "Run not found",
  CHIP: "Not found",
  BODY: "This run may have been archived or deleted, the link is stale, or you don't have access to it.",
  WALL: "None found in Wardyn",
  CAN_CHANGE: "Check the link, or go back to your runs.",
  CTA: "Back to your runs",
} as const;
