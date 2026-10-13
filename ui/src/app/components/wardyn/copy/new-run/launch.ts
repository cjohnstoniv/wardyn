/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for the Name dialog and the Launched screen, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_LAUNCH = {
  TITLE: "Name this run",
  INTRO: "A title and description help you find and understand this run later.",
  TITLE_LABEL: "Title",
  TITLE_PLACEHOLDER: "e.g. Fix payment test",
  DESCRIPTION_LABEL: "Description",
  DESCRIPTION_PLACEHOLDER: "Add context for your team",
  CHECKED_JUST_NOW: "Checked just now",
  /** pending W-28: the recommended text until the owner answers. */
  CHECKING: "Checking…",
  SUMMARY: (pool: string | number, cpu: string | number, lifetime: string | number) =>
    `${pool} · ${cpu} CPU · ${lifetime}`,
  CANCEL: "Cancel",
  LAUNCH: "Launch",
  BADGE_STARTING: "Starting",
  BADGE_WAITING: "Waiting",
  STARTING_BODY: "Your governed sandbox is being prepared.",
  WAITING_BODY: (pool: string | number, n: string | number) =>
    `${pool} is running its limit of ${n}. This run starts when there's room.`,
  POSITION: "Position",
  IN_LINE: (ordinal: string | number) => `${ordinal} in line`,
  WAITING_SINCE: "Waiting since",
  JUST_NOW: "Just now",
  GIVES_UP: "Gives up",
  GIVES_UP_IN: (wait: string | number) => `In ${wait}`,
  CANCEL_RUN: "Cancel run",
  START_ANOTHER: "Start another run",
  VIEW_RUN: "View run",
} as const;
