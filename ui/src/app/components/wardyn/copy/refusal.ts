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

// #2884: the card an address the console does not know gets, in place of a silent redirect to Runs. The path is the
// router's own location, rendered as text beside BODY_BEFORE and BODY_AFTER; the chip, rows and button reuse the run card's.
export const PAGE_NOT_FOUND = {
  TITLE: "Page not found",
  BODY_BEFORE: "There's no page at ",
  BODY_AFTER: ".",
  CTA: "Back to Runs",
} as const;
